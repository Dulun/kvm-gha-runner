package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
)

type runnerClient interface {
	GenerateJitRunnerConfig(context.Context, *scaleset.RunnerScaleSetJitRunnerSetting, int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
	RemoveRunner(context.Context, int64) error
}

type Scaler struct {
	logger       *slog.Logger
	store        *StateStore
	backend      VMBackend
	client       runnerClient
	scaleSetID   int
	minRunners   int
	maxRunners   int
	domainPrefix string
	retainFailed bool
	maxFailed    int
}

func (s *Scaler) HandleDesiredRunnerCount(ctx context.Context, assignedJobs int) (int, error) {
	if err := s.reconcile(ctx); err != nil {
		return s.store.ActiveCount(), fmt.Errorf("reconcile runners: %w", err)
	}

	current := s.store.ActiveCount()
	target := min(s.maxRunners, s.minRunners+assignedJobs)
	if target <= current {
		return current, nil
	}

	s.logger.Info("scaling up runner VMs", "current", current, "target", target)
	for i := current; i < target; i++ {
		if _, err := s.startRunner(ctx); err != nil {
			return s.store.ActiveCount(), fmt.Errorf("start runner VM: %w", err)
		}
	}
	return s.store.ActiveCount(), nil
}

func (s *Scaler) HandleJobStarted(_ context.Context, job *scaleset.JobStarted) error {
	found, err := s.store.Change(job.RunnerName, func(record *RunnerRecord) {
		record.Status = RunnerBusy
		record.JobID = job.JobID
		record.LastError = ""
	})
	if err != nil {
		return fmt.Errorf("persist job start: %w", err)
	}
	if !found {
		s.logger.Warn("job started on an untracked runner", "runner", job.RunnerName, "job_id", job.JobID)
		return nil
	}
	s.logger.Info("job started", "runner", job.RunnerName, "job_id", job.JobID, "repository", job.RepositoryName)
	return nil
}

func (s *Scaler) HandleJobCompleted(ctx context.Context, job *scaleset.JobCompleted) error {
	record, found := s.store.Get(job.RunnerName)
	if !found {
		s.logger.Warn("job completed on an untracked runner", "runner", job.RunnerName, "job_id", job.JobID)
		return nil
	}
	s.logger.Info("job completed", "runner", job.RunnerName, "job_id", job.JobID, "result", job.Result)
	if err := s.cleanupRecord(ctx, record); err != nil {
		// The queue message is already acknowledged by the official listener. Keep
		// the failed cleanup in durable state and retry it during reconciliation.
		s.logger.Error("runner cleanup deferred", "runner", record.Name, "error", err)
	}
	return nil
}

func (s *Scaler) startRunner(ctx context.Context) (string, error) {
	name, err := randomRunnerName(s.domainPrefix)
	if err != nil {
		return "", err
	}
	record := RunnerRecord{
		Name:       name,
		Domain:     name,
		DiskVolume: name + ".qcow2",
		SeedVolume: name + "-seed.iso",
		Status:     RunnerProvisioning,
	}
	if err := s.store.Add(record); err != nil {
		return "", err
	}

	removeState := true
	defer func() {
		if removeState {
			_ = s.store.Remove(name)
		}
	}()

	jit, err := s.client.GenerateJitRunnerConfig(
		ctx,
		&scaleset.RunnerScaleSetJitRunnerSetting{Name: name, WorkFolder: "_work"},
		s.scaleSetID,
	)
	if err != nil {
		return "", fmt.Errorf("generate JIT runner config: %w", err)
	}
	if jit == nil || jit.Runner == nil || jit.Runner.ID == 0 || jit.EncodedJITConfig == "" {
		return "", fmt.Errorf("GitHub returned an incomplete JIT runner config")
	}
	record.RunnerID = int64(jit.Runner.ID)
	if _, err := s.store.Change(name, func(saved *RunnerRecord) { saved.RunnerID = record.RunnerID }); err != nil {
		_ = s.client.RemoveRunner(context.WithoutCancel(ctx), record.RunnerID)
		return "", err
	}

	// Start can fail after creating resources, including when its own partial cleanup fails.
	// Keep the durable record so reconciliation can retry cleanup after a restart.
	removeState = false
	if err := s.backend.Start(ctx, record, jit.EncodedJITConfig); err != nil {
		_, persistErr := s.store.Change(name, func(saved *RunnerRecord) {
			saved.Status = RunnerCleanupFailed
			saved.LastError = err.Error()
		})
		if removeErr := s.client.RemoveRunner(context.WithoutCancel(ctx), record.RunnerID); removeErr != nil {
			s.logger.Warn("failed to remove unused GitHub runner", "runner", name, "runner_id", record.RunnerID, "error", removeErr)
		}
		if persistErr != nil {
			return "", fmt.Errorf("%w; persist failed provisioning: %v", err, persistErr)
		}
		return "", err
	}
	// Keep the provisioning record if the following state write fails so a restarted
	// manager can recover the successfully started VM.
	if _, err := s.store.Change(name, func(saved *RunnerRecord) {
		saved.Status = RunnerIdle
		saved.LastError = ""
	}); err != nil {
		return "", fmt.Errorf("persist started runner: %w", err)
	}
	return name, nil
}

func (s *Scaler) reconcile(ctx context.Context) error {
	for _, record := range s.store.List() {
		switch record.Status {
		case RunnerCleaning, RunnerCleanupFailed:
			if err := s.cleanupRecord(ctx, record); err != nil {
				s.logger.Warn("cleanup retry failed", "runner", record.Name, "error", err)
			}
			continue
		case RunnerFailed:
			continue
		}

		state, err := s.backend.State(ctx, record)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", record.Name, err)
		}
		if state.Exists && state.Active {
			if record.Status == RunnerProvisioning {
				_, err = s.store.Change(record.Name, func(saved *RunnerRecord) { saved.Status = RunnerIdle })
				if err != nil {
					return err
				}
			}
			continue
		}

		reason := "domain is missing"
		if state.Exists {
			reason = "domain stopped unexpectedly: " + state.Name
		}
		if record.RunnerID != 0 {
			if err := s.client.RemoveRunner(context.WithoutCancel(ctx), record.RunnerID); err != nil {
				s.logger.Warn("failed to remove stopped runner registration", "runner", record.Name, "error", err)
			}
		}
		if s.retainFailed && state.Exists && s.maxFailed > 0 {
			if _, err := s.store.Change(record.Name, func(saved *RunnerRecord) {
				saved.Status = RunnerFailed
				saved.LastError = reason
			}); err != nil {
				return err
			}
			s.logger.Error("retaining failed runner VM for diagnostics", "runner", record.Name, "reason", reason)
			continue
		}
		if err := s.cleanupRecord(ctx, record); err != nil {
			s.logger.Warn("cleanup of stopped runner deferred", "runner", record.Name, "error", err)
		}
	}
	return s.pruneFailed(ctx)
}

func (s *Scaler) pruneFailed(ctx context.Context) error {
	var failed []RunnerRecord
	for _, record := range s.store.List() {
		if record.Status == RunnerFailed {
			failed = append(failed, record)
		}
	}
	for len(failed) > s.maxFailed {
		record := failed[0]
		failed = failed[1:]
		if err := s.cleanupRecord(ctx, record); err != nil {
			return err
		}
	}
	return nil
}

func (s *Scaler) cleanupRecord(ctx context.Context, record RunnerRecord) error {
	if _, err := s.store.Change(record.Name, func(saved *RunnerRecord) { saved.Status = RunnerCleaning }); err != nil {
		return err
	}
	if err := s.backend.Cleanup(ctx, record); err != nil {
		_, persistErr := s.store.Change(record.Name, func(saved *RunnerRecord) {
			saved.Status = RunnerCleanupFailed
			saved.LastError = err.Error()
		})
		if persistErr != nil {
			return fmt.Errorf("cleanup: %v; persist cleanup failure: %w", err, persistErr)
		}
		return err
	}
	return s.store.Remove(record.Name)
}

func randomRunnerName(prefix string) (string, error) {
	random := make([]byte, 6)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate runner name: %w", err)
	}
	return prefix + "-" + hex.EncodeToString(random), nil
}

var _ listener.Scaler = (*Scaler)(nil)

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/actions/scaleset"
)

type fakeRunnerClient struct {
	nextID  int
	removed []int64
}

func (f *fakeRunnerClient) GenerateJitRunnerConfig(context.Context, *scaleset.RunnerScaleSetJitRunnerSetting, int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	f.nextID++
	return &scaleset.RunnerScaleSetJitRunnerConfig{
		Runner:           &scaleset.RunnerReference{ID: f.nextID},
		EncodedJITConfig: "encoded-jit",
	}, nil
}

func (f *fakeRunnerClient) RemoveRunner(_ context.Context, id int64) error {
	f.removed = append(f.removed, id)
	return nil
}

type fakeVMBackend struct {
	started      map[string]bool
	cleaned      []string
	startError   error
	cleanupError error
}

func (f *fakeVMBackend) Validate(context.Context) error { return nil }
func (f *fakeVMBackend) Start(_ context.Context, record RunnerRecord, jit string) error {
	if jit != "encoded-jit" {
		panic("unexpected JIT config")
	}
	f.started[record.Name] = true
	return f.startError
}
func (f *fakeVMBackend) State(_ context.Context, record RunnerRecord) (VMState, error) {
	return VMState{Exists: f.started[record.Name], Active: f.started[record.Name], Name: "running"}, nil
}
func (f *fakeVMBackend) Cleanup(_ context.Context, record RunnerRecord) error {
	if f.cleanupError != nil {
		return f.cleanupError
	}
	delete(f.started, record.Name)
	f.cleaned = append(f.cleaned, record.Name)
	return nil
}

func TestScalerCreatesMarksBusyAndCleansRunner(t *testing.T) {
	store, err := openStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeRunnerClient{}
	backend := &fakeVMBackend{started: make(map[string]bool)}
	scaler := &Scaler{
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:        store,
		backend:      backend,
		client:       client,
		scaleSetID:   42,
		maxRunners:   2,
		domainPrefix: "kvm-gha",
		maxFailed:    2,
	}

	count, err := scaler.HandleDesiredRunnerCount(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || store.ActiveCount() != 1 {
		t.Fatalf("active runners = %d, want 1", count)
	}
	runners := store.List()
	if len(runners) != 1 || runners[0].Status != RunnerIdle {
		t.Fatalf("unexpected runners after scale-up: %+v", runners)
	}
	name := runners[0].Name
	if err := scaler.HandleJobStarted(context.Background(), &scaleset.JobStarted{
		RunnerName: name,
		JobMessageBase: scaleset.JobMessageBase{
			JobID: "job-1",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if saved, _ := store.Get(name); saved.Status != RunnerBusy {
		t.Fatalf("runner status = %q, want busy", saved.Status)
	}
	if err := scaler.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{
		RunnerName: name,
		JobMessageBase: scaleset.JobMessageBase{
			JobID: "job-1",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if store.ActiveCount() != 0 || len(store.List()) != 0 || len(backend.cleaned) != 1 {
		t.Fatalf("runner was not fully cleaned: state=%+v cleaned=%v", store.List(), backend.cleaned)
	}
}

func TestScalerRetriesCleanupAfterPartialProvisioningFailure(t *testing.T) {
	directory := t.TempDir()
	store, err := openStateStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	startError := errors.New("partially created VM could not start or clean up")
	backend := &fakeVMBackend{
		started: make(map[string]bool), startError: startError,
		cleanupError: errors.New("cleanup temporarily unavailable"),
	}
	scaler := &Scaler{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:  store, backend: backend, client: &fakeRunnerClient{},
		scaleSetID: 42, maxRunners: 1, domainPrefix: "kvm-gha",
	}
	if _, err := scaler.startRunner(context.Background()); !errors.Is(err, startError) {
		t.Fatalf("start error = %v, want injected failure", err)
	}
	recovered, err := openStateStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	records := recovered.List()
	if len(records) != 1 || records[0].Status != RunnerCleanupFailed || recovered.ActiveCount() != 0 {
		t.Fatalf("partial provisioning record was not recoverable: %+v", records)
	}
	scaler.store = recovered
	if err := scaler.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(recovered.List()) != 1 || len(backend.started) != 1 {
		t.Fatal("failed cleanup must preserve the residual VM and its record")
	}
	backend.cleanupError = nil
	if err := scaler.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(recovered.List()) != 0 || len(backend.started) != 0 || len(backend.cleaned) != 1 {
		t.Fatalf("cleanup retry did not remove the residual VM: records=%+v", recovered.List())
	}
}

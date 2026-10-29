package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const stateVersion = 1

type RunnerStatus string

const (
	RunnerProvisioning  RunnerStatus = "provisioning"
	RunnerIdle          RunnerStatus = "idle"
	RunnerBusy          RunnerStatus = "busy"
	RunnerFailed        RunnerStatus = "failed"
	RunnerCleaning      RunnerStatus = "cleaning"
	RunnerCleanupFailed RunnerStatus = "cleanup_failed"
)

type RunnerRecord struct {
	Name       string       `json:"name"`
	RunnerID   int64        `json:"runner_id,omitempty"`
	Domain     string       `json:"domain"`
	DiskVolume string       `json:"disk_volume"`
	SeedVolume string       `json:"seed_volume"`
	Status     RunnerStatus `json:"status"`
	JobID      string       `json:"job_id,omitempty"`
	CreatedAt  time.Time    `json:"created_at"`
	UpdatedAt  time.Time    `json:"updated_at"`
	LastError  string       `json:"last_error,omitempty"`
}

func (r RunnerRecord) active() bool {
	switch r.Status {
	case RunnerProvisioning, RunnerIdle, RunnerBusy:
		return true
	default:
		return false
	}
}

type PersistentState struct {
	Version         int                     `json:"version"`
	RegistrationURL string                  `json:"registration_url,omitempty"`
	ScaleSetName    string                  `json:"scale_set_name,omitempty"`
	ScaleSetID      int                     `json:"scale_set_id,omitempty"`
	Runners         map[string]RunnerRecord `json:"runners"`
}

type StateStore struct {
	mu    sync.Mutex
	path  string
	state PersistentState
}

func openStateStore(dir string) (*StateStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("secure state directory: %w", err)
	}

	store := &StateStore{
		path: filepath.Join(dir, "state.json"),
		state: PersistentState{
			Version: stateVersion,
			Runners: make(map[string]RunnerRecord),
		},
	}

	b, err := os.ReadFile(store.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := store.persistLocked(store.state); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, fmt.Errorf("read state: %w", err)
	default:
		if err := json.Unmarshal(b, &store.state); err != nil {
			return nil, fmt.Errorf("decode state %q: %w", store.path, err)
		}
		if store.state.Version != stateVersion {
			return nil, fmt.Errorf("unsupported state version %d", store.state.Version)
		}
		if store.state.Runners == nil {
			store.state.Runners = make(map[string]RunnerRecord)
		}
	}
	return store, nil
}

func (s *StateStore) Bind(registrationURL, scaleSetName string, scaleSetID int) error {
	return s.update(func(state *PersistentState) error {
		if state.RegistrationURL != "" && state.RegistrationURL != registrationURL && len(state.Runners) != 0 {
			return fmt.Errorf("state belongs to %q and still contains runners", state.RegistrationURL)
		}
		if state.ScaleSetName != "" && state.ScaleSetName != scaleSetName && len(state.Runners) != 0 {
			return fmt.Errorf("state belongs to scale set %q and still contains runners", state.ScaleSetName)
		}
		state.RegistrationURL = registrationURL
		state.ScaleSetName = scaleSetName
		state.ScaleSetID = scaleSetID
		return nil
	})
}

func (s *StateStore) Add(record RunnerRecord) error {
	return s.update(func(state *PersistentState) error {
		if _, exists := state.Runners[record.Name]; exists {
			return fmt.Errorf("runner %q already exists in state", record.Name)
		}
		now := time.Now().UTC()
		if record.CreatedAt.IsZero() {
			record.CreatedAt = now
		}
		record.UpdatedAt = now
		state.Runners[record.Name] = record
		return nil
	})
}

func (s *StateStore) Change(name string, change func(*RunnerRecord)) (bool, error) {
	found := false
	err := s.update(func(state *PersistentState) error {
		record, ok := state.Runners[name]
		if !ok {
			return nil
		}
		found = true
		change(&record)
		record.UpdatedAt = time.Now().UTC()
		state.Runners[name] = record
		return nil
	})
	return found, err
}

func (s *StateStore) Remove(name string) error {
	return s.update(func(state *PersistentState) error {
		delete(state.Runners, name)
		return nil
	})
}

func (s *StateStore) Get(name string) (RunnerRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.state.Runners[name]
	return record, ok
}

func (s *StateStore) List() []RunnerRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	runners := make([]RunnerRecord, 0, len(s.state.Runners))
	for _, record := range s.state.Runners {
		runners = append(runners, record)
	}
	sort.Slice(runners, func(i, j int) bool { return runners[i].CreatedAt.Before(runners[j].CreatedAt) })
	return runners
}

func (s *StateStore) ActiveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, record := range s.state.Runners {
		if record.active() {
			count++
		}
	}
	return count
}

func (s *StateStore) update(change func(*PersistentState) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := cloneState(s.state)
	if err := change(&next); err != nil {
		return err
	}
	if err := s.persistLocked(next); err != nil {
		return err
	}
	s.state = next
	return nil
}

func cloneState(state PersistentState) PersistentState {
	clone := state
	clone.Runners = make(map[string]RunnerRecord, len(state.Runners))
	for name, record := range state.Runners {
		clone.Runners[name] = record
	}
	return clone
}

func (s *StateStore) persistLocked(state PersistentState) error {
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	b = append(b, '\n')

	temp, err := os.CreateTemp(filepath.Dir(s.path), ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary state: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)

	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("secure temporary state: %w", err)
	}
	if _, err := temp.Write(b); err != nil {
		temp.Close()
		return fmt.Errorf("write state: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("sync state: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close state: %w", err)
	}
	if err := os.Rename(tempName, s.path); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	return nil
}

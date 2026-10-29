package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestStateStorePersistsRunnerLifecycle(t *testing.T) {
	dir := t.TempDir()
	store, err := openStateStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Bind("https://github.com/OWNER/REPO", "kvm-runner-set", 42); err != nil {
		t.Fatal(err)
	}
	record := RunnerRecord{
		Name:       "kvm-gha-0123456789ab",
		Domain:     "kvm-gha-0123456789ab",
		DiskVolume: "kvm-gha-0123456789ab.qcow2",
		SeedVolume: "kvm-gha-0123456789ab-seed.iso",
		Status:     RunnerProvisioning,
	}
	if err := store.Add(record); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Change(record.Name, func(saved *RunnerRecord) {
		saved.RunnerID = 99
		saved.Status = RunnerBusy
		saved.JobID = "job-1"
	}); err != nil {
		t.Fatal(err)
	}

	reopened, err := openStateStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	saved, ok := reopened.Get(record.Name)
	if !ok {
		t.Fatal("runner disappeared after reopening state")
	}
	if saved.Status != RunnerBusy || saved.RunnerID != 99 || saved.JobID != "job-1" {
		t.Fatalf("unexpected persisted runner: %+v", saved)
	}
	info, err := os.Stat(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("state permissions = %o, want 600", got)
	}
}

func TestStateNeverContainsJITConfig(t *testing.T) {
	dir := t.TempDir()
	store, err := openStateStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Add(RunnerRecord{
		Name:       "kvm-gha-aaaaaaaaaaaa",
		Domain:     "kvm-gha-aaaaaaaaaaaa",
		DiskVolume: "kvm-gha-aaaaaaaaaaaa.qcow2",
		SeedVolume: "kvm-gha-aaaaaaaaaaaa-seed.iso",
		Status:     RunnerIdle,
	}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("super-secret-jit-config")) {
		t.Fatal("state unexpectedly contains JIT data")
	}
}

package state

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestRemoveNonExistentVM(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManagerWithDir(dir)

	err := mgr.Remove("vm-nonexistent")
	if err == nil {
		t.Fatal("expected error when removing non-existent VM, got nil")
	}
}

func TestRemoveStoppedVM(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManagerWithDir(dir)

	vmDir := filepath.Join(dir, "vm-test123")
	os.MkdirAll(vmDir, 0700)
	os.WriteFile(filepath.Join(vmDir, "status"), []byte("stopped"), 0600)

	err := mgr.Remove("vm-test123")
	if err != nil {
		t.Fatalf("expected no error removing stopped VM, got: %v", err)
	}

	if _, err := os.Stat(vmDir); !os.IsNotExist(err) {
		t.Fatal("expected VM directory to be removed")
	}
}

func TestRemoveRunningVM(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManagerWithDir(dir)

	vmDir := filepath.Join(dir, "vm-running1")
	os.MkdirAll(vmDir, 0700)
	os.WriteFile(filepath.Join(vmDir, "status"), []byte("running"), 0600)
	os.WriteFile(filepath.Join(vmDir, "pid"), []byte(strconv.Itoa(os.Getpid())), 0600)

	err := mgr.Remove("vm-running1")
	if err == nil {
		t.Fatal("expected error when removing running VM, got nil")
	}
}

func TestRemoveRunningVMWithDeadProcess(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManagerWithDir(dir)

	vmDir := filepath.Join(dir, "vm-dead1")
	os.MkdirAll(vmDir, 0700)
	os.WriteFile(filepath.Join(vmDir, "status"), []byte("running"), 0600)
	os.WriteFile(filepath.Join(vmDir, "pid"), []byte("999999999"), 0600)

	err := mgr.Remove("vm-dead1")
	if err != nil {
		t.Fatalf("expected no error removing VM with dead process, got: %v", err)
	}

	if _, err := os.Stat(vmDir); !os.IsNotExist(err) {
		t.Fatal("expected VM directory to be removed")
	}
}

// TestUnregisterThenRemove_RmFlag is a regression test for
// https://github.com/jingkaihe/matchlock/issues/12
// When --rm is set, the VM state directory must be fully removed after Close().
// Previously, Close() only called Unregister() (marking status as "stopped")
// without calling Remove(), leaving stale state behind.
func TestUnregisterThenRemove_RmFlag(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManagerWithDir(dir)

	id := "vm-rm-test"

	if err := mgr.Register(id, map[string]string{"image": "alpine:latest"}); err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	states, err := mgr.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	found := false
	for _, s := range states {
		if s.ID == id {
			found = true
			if s.Status != "running" && s.Status != "crashed" {
				t.Fatalf("expected status running or crashed, got %s", s.Status)
			}
		}
	}
	if !found {
		t.Fatal("expected VM to be listed after Register")
	}

	// Simulate what sb.Close() does
	if err := mgr.Unregister(id); err != nil {
		t.Fatalf("Unregister failed: %v", err)
	}

	// After Unregister, the VM should still exist in state (as "stopped")
	state, err := mgr.Get(id)
	if err != nil {
		t.Fatalf("expected VM to still exist after Unregister, got: %v", err)
	}
	if state.Status != "stopped" {
		t.Fatalf("expected status stopped after Unregister, got %s", state.Status)
	}

	// Simulate what --rm should do: call Remove() after Close()
	if err := mgr.Remove(id); err != nil {
		t.Fatalf("Remove after Unregister failed: %v", err)
	}

	// After Remove, the VM should not be listed
	states, err = mgr.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	for _, s := range states {
		if s.ID == id {
			t.Fatalf("VM %s should not appear in list after Remove", id)
		}
	}

	// The state directory should be gone
	vmDir := filepath.Join(dir, id)
	if _, err := os.Stat(vmDir); !os.IsNotExist(err) {
		t.Fatal("expected VM directory to be fully removed after Unregister + Remove")
	}
}

// TestUnregisterWithoutRemove_NoRmFlag verifies that when --rm is NOT set,
// Unregister alone leaves the state directory intact so the VM remains visible.
func TestUnregisterWithoutRemove_NoRmFlag(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManagerWithDir(dir)

	id := "vm-no-rm-test"

	if err := mgr.Register(id, map[string]string{"image": "alpine:latest"}); err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	if err := mgr.Unregister(id); err != nil {
		t.Fatalf("Unregister failed: %v", err)
	}

	// Without Remove, VM should still be listed as stopped
	states, err := mgr.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	found := false
	for _, s := range states {
		if s.ID == id {
			found = true
			if s.Status != "stopped" {
				t.Fatalf("expected status stopped, got %s", s.Status)
			}
		}
	}
	if !found {
		t.Fatal("VM should still be listed after Unregister without Remove")
	}

	vmDir := filepath.Join(dir, id)
	if _, err := os.Stat(vmDir); os.IsNotExist(err) {
		t.Fatal("expected VM directory to persist after Unregister without Remove")
	}
}

package cmd

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func resetLockState() {
	if pipelineLockFile != nil {
		_ = syscall.Flock(int(pipelineLockFile.Fd()), syscall.LOCK_UN)
		_ = pipelineLockFile.Close()
		pipelineLockFile = nil
	}
}

func TestAcquireLock_BlocksSecondCaller(t *testing.T) {
	t.Cleanup(resetLockState)

	lockPath := filepath.Join(t.TempDir(), "pipeline.lock")

	if err := acquireLock(lockPath); err != nil {
		t.Fatalf("first lock acquisition failed: %v", err)
	}

	// Hold the lock in pipelineLockFile; save and swap it out so the second
	// call creates its own fd.
	held := pipelineLockFile
	pipelineLockFile = nil
	t.Cleanup(func() {
		_ = syscall.Flock(int(held.Fd()), syscall.LOCK_UN)
		_ = held.Close()
	})

	err := acquireLock(lockPath)
	if err == nil {
		t.Fatal("expected second lock acquisition to fail, but it succeeded")
	}
}

func TestAcquireLock_SucceedsWhenFree(t *testing.T) {
	t.Cleanup(resetLockState)

	lockPath := filepath.Join(t.TempDir(), "pipeline.lock")

	// Pre-create the file (simulates previous run).
	if err := os.WriteFile(lockPath, nil, 0644); err != nil {
		t.Fatal(err)
	}

	if err := acquireLock(lockPath); err != nil {
		t.Fatalf("lock acquisition should succeed on free file: %v", err)
	}
}

func TestEnsurePipelineLock_ReusesHeldLock(t *testing.T) {
	t.Cleanup(resetLockState)

	if err := acquireLock(filepath.Join(t.TempDir(), "pipeline.lock")); err != nil {
		t.Fatalf("lock acquisition failed: %v", err)
	}
	held := pipelineLockFile

	// A pipeline step calling ensurePipelineLock must not try to re-lock.
	if err := ensurePipelineLock(); err != nil {
		t.Fatalf("ensurePipelineLock with lock held: %v", err)
	}
	if pipelineLockFile != held {
		t.Error("ensurePipelineLock replaced the held lock")
	}
}

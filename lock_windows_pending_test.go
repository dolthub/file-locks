// Copyright 2026 Dolthub, Inc. Licensed under the Apache License, Version 2.0.
// See the LICENSE file in the root of this repository.

//go:build windows

package fslock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A Lock that met a pending delete must work again once the delete finishes.
// Fails on the PR head: reopen leaves the Lock closed, so the second TryLock
// reports "lock is closed".
func TestLockUsableAfterDeletePending(t *testing.T) {
	path := tempPath(t)
	stale, retained := newLock(t, path), newLock(t, path)
	markDeletePending(t, path)

	if err := stale.TryLock(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("TryLock(pending) = %v, want os.ErrNotExist", err)
	}
	if err := retained.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stale.TryLock(); err != nil {
		t.Fatalf("TryLock after the delete finished = %v, want nil", err)
	}
}

// A waiting acquire must wait out a pending delete rather than fail.
// Fails on the PR head: LockWithTimeout returns os.ErrNotExist at once.
func TestWaitOutlastsDeletePending(t *testing.T) {
	path := tempPath(t)
	stale, retained := newLock(t, path), newLock(t, path)
	markDeletePending(t, path)

	time.AfterFunc(50*time.Millisecond, func() { retained.Close() })
	err := within(t, func() error { return stale.LockWithTimeout(10 * time.Second) })
	if err != nil {
		t.Fatalf("LockWithTimeout across a pending delete = %v, want nil", err)
	}
}

// A pending delete is temporary and a missing directory is not, and a caller
// has to be able to tell which one it has.
// Fails on the PR head: both are Kind Failed wrapping os.ErrNotExist.
func TestDeletePendingIsTemporary(t *testing.T) {
	path := tempPath(t)
	newLock(t, path)
	markDeletePending(t, path)

	_, pending := New(path)
	if !errors.Is(pending, os.ErrNotExist) {
		t.Fatalf("New(pending) = %v, want os.ErrNotExist", pending)
	}
	if !IsTemporary(pending) {
		t.Errorf("IsTemporary(New(pending)) = false, want true: %v", pending)
	}
	_, missing := New(filepath.Join(t.TempDir(), "no-such-dir", "LOCK"))
	if IsTemporary(missing) {
		t.Errorf("IsTemporary(New(missing directory)) = true, want false: %v", missing)
	}
}

// The Dolt pattern that failed in CI: several cleaners take each lock file
// they find and os.Remove it while holding it, racing a creator of new ones.
// Access denied is retried for 100ms, as Dolt does, and only then reported.
// Outcome on the PR head is unknown: if this still fails with access denied,
// STATUS_DELETE_PENDING was not the cause of the Dolt failure.
func TestRacingRemoveWhileHeld(t *testing.T) {
	dir := t.TempDir()

	open := func(path string) (*Lock, error) {
		deadline := time.Now().Add(100 * time.Millisecond)
		for {
			l, err := New(path)
			if err == nil || !errors.Is(err, os.ErrPermission) || !time.Now().Before(deadline) {
				return l, err
			}
			time.Sleep(time.Millisecond)
		}
	}
	benign := func(err error) bool {
		return err == nil || errors.Is(err, ErrLocked) || errors.Is(err, os.ErrNotExist)
	}
	clean := func(path string) error {
		l, err := open(path)
		if err != nil {
			return err
		}
		defer l.Close()
		if err := l.TryLock(); err != nil {
			return err
		}
		return os.Remove(path)
	}

	const cleaners = 4
	stop := make(chan struct{})
	done := make(chan error, cleaners)
	for i := 0; i < cleaners; i++ {
		go func() {
			for {
				select {
				case <-stop:
					done <- nil
					return
				default:
				}
				entries, err := os.ReadDir(dir)
				if err != nil {
					done <- err
					return
				}
				for _, e := range entries {
					if err := clean(filepath.Join(dir, e.Name())); !benign(err) {
						done <- err
						return
					}
				}
			}
		}()
	}
	defer func() {
		close(stop)
		for i := 0; i < cleaners; i++ {
			if err := <-done; err != nil {
				t.Errorf("cleaner: %v", err)
			}
		}
	}()

	for i := 0; i < 2000; i++ {
		path := filepath.Join(dir, "LOCK-"+time.Now().Format("150405.000000000"))
		if err := os.WriteFile(path, nil, 0666); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		l, err := open(path)
		if err == nil {
			err = l.TryLock()
			l.Close()
		}
		if !benign(err) {
			t.Fatalf("iteration %d: %v", i, err)
		}
	}
}

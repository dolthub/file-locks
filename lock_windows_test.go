// Copyright 2026 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build windows

package fslock

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

// TestDeletePending distinguishes classic pending deletion from POSIX unlink
// and real permission failures while an unlocked contender retains a handle.
func TestDeletePending(t *testing.T) {
	path := tempPath(t)
	stale, retained := newLock(t, path), newLock(t, path)
	markDeletePending(t, path)

	f, err := sysOpen(path)
	if f != nil {
		f.Close()
	}
	// Only STATUS_DELETE_PENDING maps to this exact sentinel; an absent
	// name would instead be recreated by sysOpen, so this verifies the fixture.
	if err != os.ErrNotExist {
		t.Fatalf("sysOpen(pending) = %v, want exact os.ErrNotExist", err)
	}
	lock, err := New(path)
	if lock != nil {
		lock.Close()
	}
	if !errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
		t.Fatalf("New(pending) = %v, want only os.ErrNotExist", err)
	}
	// Retaining another handle keeps deletion pending after stale closes its
	// old handle to reopen the file during acquisition.
	if err := stale.TryLock(); !errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
		t.Fatalf("TryLock(pending) = %v, want only os.ErrNotExist", err)
	}
	if err := errors.Join(stale.Close(), retained.Close()); err != nil {
		t.Fatal(err)
	}
	fresh := newLock(t, path)
	if err := fresh.TryLock(); err != nil {
		t.Fatal(err)
	}
	if err := newLock(t, path).TryLock(); err != ErrLocked {
		t.Fatalf("TryLock(recreated) = %v, want ErrLocked", err)
	}

	t.Run("permission failure", func(t *testing.T) {
		path := tempPath(t)
		if err := os.WriteFile(path, nil, 0444); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(path, 0666) })
		lock, err := New(path)
		if lock != nil {
			lock.Close()
		}
		if !errors.Is(err, os.ErrPermission) || errors.Is(err, os.ErrNotExist) {
			t.Fatalf("New(read-only) = %v, want only os.ErrPermission", err)
		}
	})
}

// markDeletePending uses classic deletion semantics, without the POSIX unlink
// behavior that os.Remove can request on modern Windows.
func markDeletePending(t *testing.T, path string) {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.DELETE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	pending := byte(1)
	if err := windows.SetFileInformationByHandle(h, windows.FileDispositionInfo, &pending, 1); err != nil {
		windows.CloseHandle(h)
		t.Fatal(err)
	}
	if err := windows.CloseHandle(h); err != nil {
		t.Fatal(err)
	}
}

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

package fslock

// The two races this package is careful about: a lock file replaced between
// opening it and locking it, and a descriptor reaching a child process.

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// removeOpenFile unlinks a lock file this process has open, or skips the
// test where the platform will not allow it. On windows a file opened
// without FILE_SHARE_DELETE cannot be removed, which closes the replacement
// race by preventing it rather than by detecting it.
func removeOpenFile(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Skipf("this platform will not remove an open lock file (%v), so it cannot be replaced underneath one", err)
	}
}

// A lock on a file that has been unlinked excludes nobody: whoever comes
// along next creates a new file at the path and locks that instead. Taking
// the stale one and reporting success would be worse than useless, because
// the caller would go on to write the very files the lock protects.
func TestDoesNotLockAGhostFile(t *testing.T) {
	path := lockPath(t)
	stale := newTest(t, path)

	removeOpenFile(t, path)
	startHolder(t, path, 5*time.Second) // creates a new lock file and holds it

	if err := stale.TryLock(); err != ErrLocked {
		t.Fatalf("TryLock = %v, want ErrLocked: it locked a file nobody else can reach", err)
	}
}

// When the lock file has been replaced and the new one is free, the Lock
// moves to it rather than reporting success on the old one.
func TestReopensAReplacedLockFile(t *testing.T) {
	path := lockPath(t)
	stale := newTest(t, path)

	removeOpenFile(t, path)
	if err := os.WriteFile(path, nil, 0666); err != nil {
		t.Fatal(err)
	}

	if err := stale.TryLock(); err != nil {
		t.Fatalf("TryLock: %v", err)
	}
	// The lock it holds has to be the one that counts: the file that is at
	// the path now, as another process sees it.
	if code := exitCode(t, startChild(t, "try", path)); code != exitLocked {
		t.Fatalf("another process took the lock as well (exit %d)", code)
	}
}

// A lock file that is simply gone is created again.
func TestRecreatesARemovedLockFile(t *testing.T) {
	path := lockPath(t)
	lck := newTest(t, path)

	removeOpenFile(t, path)
	if err := lck.TryLock(); err != nil {
		t.Fatalf("TryLock: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the lock file was not put back: %v", err)
	}
}

// The lock file's descriptor must not reach a child process.
//
// os notes a race between opening a file and marking it close-on-exec, and
// is "content to live with" it because an inherited descriptor is usually
// just a leak. For a lock it is not: an flock(2) lock belongs to the open
// file description, so a child that inherits one keeps the lock alive after
// the process that took it is gone.
//
// The child here does exactly that - takes the lock, starts a process that
// outlives it, dies without unlocking - and the lock has to die with it.
func TestDescriptorDoesNotLeakIntoChildren(t *testing.T) {
	path := lockPath(t)
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")

	leaker := startChild(t, "leak", path, pidEnv+"="+pidFile)
	if code := exitCode(t, leaker); code != exitOK {
		t.Fatalf("the child exited with %d", code)
	}
	waitForFile(t, pidFile, "the grandchild never started")
	t.Cleanup(func() { killByPidFile(t, pidFile) })

	lck := newTest(t, path)
	if err := lck.TryLock(); err != nil {
		t.Fatalf("the lock outlived the process that took it (%v): its descriptor reached the grandchild", err)
	}
}

func killByPidFile(t *testing.T, pidFile string) {
	t.Helper()

	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Logf("reading %s: %v", pidFile, err)
		return
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Logf("parsing %s: %v", pidFile, err)
		return
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		t.Logf("finding pid %d: %v", pid, err)
		return
	}
	p.Kill()
	p.Wait()
}

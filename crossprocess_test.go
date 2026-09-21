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

// Cross-process tests, and the machinery the rest of the suite uses to get a
// second process. The point of the package is exclusion between processes, so
// the tests use real ones: the test binary re-executes itself with an
// environment variable that TestMain reads before any test runs. That works
// where `go run` would not - a sandboxed machine, a container with no
// toolchain, a cross-built binary.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

const (
	roleEnv  = "FSLOCK_TEST_ROLE"
	pathEnv  = "FSLOCK_TEST_PATH"
	readyEnv = "FSLOCK_TEST_READY"
	holdEnv  = "FSLOCK_TEST_HOLD"
	pidEnv   = "FSLOCK_TEST_PID"

	// Exit codes the children use to report what happened.
	exitOK      = 0
	exitLocked  = 3
	exitFailure = 4
)

func TestMain(m *testing.M) {
	switch role := os.Getenv(roleEnv); role {
	case "":
		os.Exit(m.Run())
	case "hold":
		os.Exit(childHold())
	case "try":
		os.Exit(childTry())
	case "leak":
		os.Exit(childLeak())
	case "sleep":
		os.Exit(childSleep())
	default:
		fmt.Fprintf(os.Stderr, "unknown child role %q\n", role)
		os.Exit(exitFailure)
	}
}

// childHold takes the lock, announces that it has it, and keeps it.
func childHold() int {
	lck, err := New(os.Getenv(pathEnv))
	if err != nil {
		fmt.Fprintln(os.Stderr, "child new:", err)
		return exitFailure
	}
	defer lck.Close()

	if err := lck.LockWithTimeout(30 * time.Second); err != nil {
		fmt.Fprintln(os.Stderr, "child lock:", err)
		return exitFailure
	}
	if err := os.WriteFile(os.Getenv(readyEnv), []byte("held"), 0666); err != nil {
		fmt.Fprintln(os.Stderr, "child ready:", err)
		return exitFailure
	}

	hold, err := time.ParseDuration(os.Getenv(holdEnv))
	if err != nil {
		fmt.Fprintln(os.Stderr, "child hold:", err)
		return exitFailure
	}
	time.Sleep(hold)
	return exitOK
}

// childTry reports whether the lock is free right now.
func childTry() int {
	lck, err := New(os.Getenv(pathEnv))
	if err != nil {
		fmt.Fprintln(os.Stderr, "child new:", err)
		return exitFailure
	}
	defer lck.Close()

	switch err := lck.TryLock(); {
	case err == nil:
		return exitOK
	case errors.Is(err, ErrLocked):
		return exitLocked
	default:
		fmt.Fprintln(os.Stderr, "child trylock:", err)
		return exitFailure
	}
}

// childLeak takes the lock, starts a process that outlives it, and exits
// without unlocking - the shape of a program that shells out and then dies.
// If the lock file's descriptor reached the grandchild, the lock outlives
// this process too.
func childLeak() int {
	lck, err := New(os.Getenv(pathEnv))
	if err != nil {
		fmt.Fprintln(os.Stderr, "child new:", err)
		return exitFailure
	}
	if err := lck.Lock(); err != nil {
		fmt.Fprintln(os.Stderr, "child lock:", err)
		return exitFailure
	}

	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "child executable:", err)
		return exitFailure
	}
	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), roleEnv+"=sleep")
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "child start:", err)
		return exitFailure
	}
	// Deliberately no Unlock and no Close: this process is about to die
	// holding the lock, which is the situation the OS is supposed to
	// clean up for us.
	return exitOK
}

// childSleep announces itself and stays alive, holding whatever it inherited.
func childSleep() int {
	if pidFile := os.Getenv(pidEnv); pidFile != "" {
		if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0666); err != nil {
			fmt.Fprintln(os.Stderr, "child pid:", err)
			return exitFailure
		}
	}
	time.Sleep(60 * time.Second)
	return exitOK
}

// startChild runs this test binary again in the given role.
func startChild(t *testing.T, role, path string, env ...string) *exec.Cmd {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locating the test binary: %v", err)
	}
	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), roleEnv+"="+role, pathEnv+"="+path)
	cmd.Env = append(cmd.Env, env...)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the %s child: %v", role, err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	return cmd
}

// startHolder starts a child that holds the lock for hold, and returns once
// it has it.
func startHolder(t *testing.T, path string, hold time.Duration) *exec.Cmd {
	t.Helper()

	ready := filepath.Join(t.TempDir(), "ready")
	cmd := startChild(t, "hold", path, readyEnv+"="+ready, holdEnv+"="+hold.String())
	waitForFile(t, ready, "the child never reported that it had the lock")
	return cmd
}

// waitForFile blocks until path exists.
func waitForFile(t *testing.T, path, message string) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(message)
		}
		time.Sleep(time.Millisecond)
	}
}

func exitCode(t *testing.T, cmd *exec.Cmd) int {
	t.Helper()

	err := cmd.Wait()
	if err == nil {
		return exitOK
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	t.Fatalf("waiting for the child: %v", err)
	return -1
}

func TestAnotherProcessExcludesUs(t *testing.T) {
	path := lockPath(t)
	holder := startHolder(t, path, 2*time.Second)

	lck := newTest(t, path)
	if err := lck.TryLock(); !errors.Is(err, ErrLocked) {
		t.Fatalf("TryLock = %v, want ErrLocked", err)
	}
	if err := lck.LockWithTimeout(50 * time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Fatalf("LockWithTimeout = %v, want ErrTimeout", err)
	}

	// Once the holder exits, the lock is ours.
	if code := exitCode(t, holder); code != exitOK {
		t.Fatalf("child exited with %d", code)
	}
	if err := lck.LockWithTimeout(10 * time.Second); err != nil {
		t.Fatalf("LockWithTimeout after the holder left: %v", err)
	}
}

func TestAnotherProcessSeesOurLock(t *testing.T) {
	path := lockPath(t)
	lck := newTest(t, path)
	if err := lck.Lock(); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	if code := exitCode(t, startChild(t, "try", path)); code != exitLocked {
		t.Fatalf("child exited with %d, want %d: it did not see our lock", code, exitLocked)
	}

	if err := lck.Unlock(); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if code := exitCode(t, startChild(t, "try", path)); code != exitOK {
		t.Fatalf("child exited with %d, want %d: the lock was not free", code, exitOK)
	}
}

// A holder that dies without unwinding leaves nothing behind: the OS drops
// the lock with the process. Nothing here cleans up stale lock files, so this
// is the guarantee that makes that safe.
func TestKilledHolderLeavesNothingBehind(t *testing.T) {
	path := lockPath(t)
	holder := startHolder(t, path, time.Hour)

	if err := holder.Process.Kill(); err != nil {
		t.Fatalf("killing the holder: %v", err)
	}
	holder.Wait()

	lck := newTest(t, path)
	if err := lck.LockWithTimeout(30 * time.Second); err != nil {
		t.Fatalf("the lock outlived the process that held it: %v", err)
	}
}

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

package filelock

// Cross-process tests. The point of the package is exclusion between
// processes, so these tests use a second one: the test binary re-executes
// itself with an environment variable that TestMain reads before any test
// runs. That keeps them working where `go run` would not - a sandboxed CI
// machine, a container without a toolchain, a cross-built binary.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const (
	roleEnv  = "FILELOCK_TEST_ROLE"
	pathEnv  = "FILELOCK_TEST_PATH"
	readyEnv = "FILELOCK_TEST_READY"
	holdEnv  = "FILELOCK_TEST_HOLD"

	// Exit codes the child uses to report what happened.
	exitOK      = 0
	exitBusy    = 3
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
	default:
		fmt.Fprintf(os.Stderr, "unknown child role %q\n", role)
		os.Exit(exitFailure)
	}
}

// childHold takes the claim, announces that it has it, and keeps it.
func childHold() int {
	g, err := Open(os.Getenv(pathEnv), WithHolderStamp())
	if err != nil {
		fmt.Fprintln(os.Stderr, "child open:", err)
		return exitFailure
	}
	defer g.Close()

	if err := g.AcquireWithin(context.Background(), 30*time.Second); err != nil {
		fmt.Fprintln(os.Stderr, "child acquire:", err)
		return exitFailure
	}
	if err := os.WriteFile(os.Getenv(readyEnv), []byte("held"), 0666); err != nil {
		fmt.Fprintln(os.Stderr, "child ready:", err)
		return exitFailure
	}

	hold, err := time.ParseDuration(os.Getenv(holdEnv))
	if err != nil {
		fmt.Fprintln(os.Stderr, "child hold duration:", err)
		return exitFailure
	}
	time.Sleep(hold)
	return exitOK
}

// childTry reports whether the claim is free right now.
func childTry() int {
	g, err := Open(os.Getenv(pathEnv))
	if err != nil {
		fmt.Fprintln(os.Stderr, "child open:", err)
		return exitFailure
	}
	defer g.Close()

	ok, err := g.AcquireNow()
	if err != nil {
		fmt.Fprintln(os.Stderr, "child acquire:", err)
		return exitFailure
	}
	if !ok {
		return exitBusy
	}
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

// startHolder starts a child that holds the claim for hold, and returns once
// it has it.
func startHolder(t *testing.T, path string, hold time.Duration) *exec.Cmd {
	t.Helper()

	ready := filepath.Join(filepath.Dir(path), "ready")
	cmd := startChild(t, "hold", path, readyEnv+"="+ready, holdEnv+"="+hold.String())

	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			return cmd
		}
		if time.Now().After(deadline) {
			t.Fatal("the child never reported that it had the claim")
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

	g := openTest(t, path)
	if acquireNow(t, g) {
		t.Fatal("claimed a lock file another process holds")
	}
	if err := g.AcquireWithin(context.Background(), 50*time.Millisecond); !errors.Is(err, ErrWaitExpired) {
		t.Fatalf("AcquireWithin = %v, want ErrWaitExpired", err)
	}

	// Once the holder exits, the claim is ours.
	if code := exitCode(t, holder); code != exitOK {
		t.Fatalf("child exited with %d", code)
	}
	if err := g.AcquireWithin(context.Background(), 10*time.Second); err != nil {
		t.Fatalf("AcquireWithin after the holder left: %v", err)
	}
}

func TestAnotherProcessSeesOurClaim(t *testing.T) {
	path := lockPath(t)
	g := openTest(t, path)
	if !acquireNow(t, g) {
		t.Fatal("could not take the claim")
	}

	if code := exitCode(t, startChild(t, "try", path)); code != exitBusy {
		t.Fatalf("child exited with %d, want %d: it did not see our claim", code, exitBusy)
	}

	if err := g.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if code := exitCode(t, startChild(t, "try", path)); code != exitOK {
		t.Fatalf("child exited with %d, want %d: the claim was not free", code, exitOK)
	}
}

// A holder that dies without unwinding leaves nothing behind: the OS drops the
// claim with the process. Nothing in this package cleans up stale lock files,
// so this is the guarantee that makes that safe.
func TestKilledHolderLeavesNothingBehind(t *testing.T) {
	path := lockPath(t)
	holder := startHolder(t, path, time.Hour)

	if err := holder.Process.Kill(); err != nil {
		t.Fatalf("killing the holder: %v", err)
	}
	holder.Wait()

	g := openTest(t, path)
	if err := g.AcquireWithin(context.Background(), 30*time.Second); err != nil {
		t.Fatalf("the claim outlived the process that held it: %v", err)
	}
}

// The holder stamp is readable while another process holds the claim, which
// is the whole reason to write it.
func TestReadHolderAcrossProcesses(t *testing.T) {
	path := lockPath(t)
	holder := startHolder(t, path, 2*time.Second)

	g := openTest(t, path)
	h, err := g.ReadHolder()
	if err != nil {
		t.Fatalf("ReadHolder: %v", err)
	}
	if h.PID != holder.Process.Pid {
		t.Errorf("stamped pid = %d, want the child's %d", h.PID, holder.Process.Pid)
	}
	if h.Since.IsZero() {
		t.Error("the stamp carries no time")
	}
}

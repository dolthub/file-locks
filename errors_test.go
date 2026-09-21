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

import (
	"context"
	"errors"
	"io/fs"
	"syscall"
	"testing"
)

func TestSentinelClassification(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		timeout   bool
		temporary bool
	}{
		{"locked", ErrLocked, false, true},
		{"timeout", ErrTimeout, true, true},
		{"canceled", canceled("lock", "/x", context.Canceled), false, false},
		{"replaced", stateError("lock", "/x", Replaced), false, true},
		{"not held", stateError("unlock", "/x", NotHeld), false, false},
		{"already held", stateError("lock", "/x", AlreadyHeld), false, false},
		{"closed", stateError("lock", "/x", Closed), false, false},
		{"out of locks", failure("lock", "/x", syscall.ENOLCK), false, true},
		{"interrupted", failure("lock", "/x", syscall.EINTR), false, true},
		{"permission denied", failure("new", "/x", syscall.EACCES), false, false},
		{"deadline underneath", failure("lock", "/x", context.DeadlineExceeded), true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var e *Error
			if !errors.As(tc.err, &e) {
				t.Fatalf("%v is not an *Error", tc.err)
			}
			if got := e.IsTimeout(); got != tc.timeout {
				t.Errorf("IsTimeout() = %v, want %v", got, tc.timeout)
			}
			if got := e.IsTemporary(); got != tc.temporary {
				t.Errorf("IsTemporary() = %v, want %v", got, tc.temporary)
			}
			// The package-level helpers answer the same way.
			if got := IsTimeout(tc.err); got != tc.timeout {
				t.Errorf("fslock.IsTimeout = %v, want %v", got, tc.timeout)
			}
			if got := IsTemporary(tc.err); got != tc.temporary {
				t.Errorf("fslock.IsTemporary = %v, want %v", got, tc.temporary)
			}
		})
	}
}

func TestHelpersIgnoreOtherErrors(t *testing.T) {
	for _, err := range []error{nil, errors.New("something else"), syscall.ETIMEDOUT} {
		if IsTimeout(err) {
			t.Errorf("IsTimeout(%v) = true", err)
		}
		if IsTemporary(err) {
			t.Errorf("IsTemporary(%v) = true", err)
		}
	}
}

// The sentinels are matched by kind, so an error that carries an operation
// and a path still answers to the value callers compare against.
func TestSentinelMatching(t *testing.T) {
	populated := timedOut("lock", "/x/LOCK", nil)

	if !errors.Is(populated, ErrTimeout) {
		t.Error("a populated timeout must match ErrTimeout")
	}
	if populated == ErrTimeout {
		t.Error("a populated timeout must not be the sentinel value itself")
	}
	if errors.Is(populated, ErrLocked) {
		t.Error("a timeout must not match ErrLocked")
	}
	if errors.Is(ErrLocked, ErrTimeout) || errors.Is(ErrTimeout, ErrLocked) {
		t.Error("the sentinels must not match each other")
	}
	if !errors.Is(ErrLocked, ErrLocked) {
		t.Error("ErrLocked must match itself")
	}

	// Matching by kind only goes one way: a bare sentinel is a class, a
	// populated error is a particular event.
	if errors.Is(ErrTimeout, populated) {
		t.Error("the sentinel must not match a populated error")
	}
}

func TestClosedMatchesFsErrClosed(t *testing.T) {
	err := stateError("lock", "/x", Closed)
	if !errors.Is(err, fs.ErrClosed) {
		t.Error("a closed Lock should report fs.ErrClosed")
	}
	if errors.Is(stateError("lock", "/x", NotHeld), fs.ErrClosed) {
		t.Error("only the closed state should report fs.ErrClosed")
	}
}

func TestErrorUnwrapsItsCause(t *testing.T) {
	err := failure("lock", "/x/LOCK", syscall.ENOLCK)
	if !errors.Is(err, syscall.ENOLCK) {
		t.Error("the underlying cause must be reachable with errors.Is")
	}

	var errno syscall.Errno
	if !errors.As(err, &errno) || errno != syscall.ENOLCK {
		t.Error("the underlying cause must be reachable with errors.As")
	}
}

func TestErrorMessages(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{ErrLocked, "fslock: file is locked"},
		{ErrTimeout, "fslock: timed out waiting for the lock"},
		{stateError("unlock", "/x/LOCK", NotHeld), "fslock unlock /x/LOCK: lock is not held by this Lock"},
		{failure("new", "/x/LOCK", syscall.EACCES), "fslock new /x/LOCK: " + syscall.EACCES.Error()},
		{canceled("lock", "/x/LOCK", context.Canceled), "fslock lock /x/LOCK: wait canceled: context canceled"},
	} {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("Error() = %q, want %q", got, tc.want)
		}
	}
}

func TestKindStrings(t *testing.T) {
	kinds := []Kind{Contended, TimedOut, Canceled, Replaced, AlreadyHeld, NotHeld, Closed, Failed}
	seen := make(map[string]Kind, len(kinds))
	for _, k := range kinds {
		s := k.String()
		if s == "" || s == "unknown error" {
			t.Errorf("Kind(%d).String() = %q", k, s)
		}
		if other, dup := seen[s]; dup {
			t.Errorf("Kind(%d) and Kind(%d) share the description %q", k, other, s)
		}
		seen[s] = k
	}
	if got := Kind(0).String(); got != "unknown error" {
		t.Errorf("Kind(0).String() = %q", got)
	}
}

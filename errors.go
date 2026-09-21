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
	"os"
	"syscall"
)

// A Kind says what went wrong, at the granularity a caller can act on.
type Kind int

const (
	// Contended: the lock is held, by another Lock here or by another
	// process. Nothing is wrong; someone else got there first.
	Contended Kind = iota + 1
	// TimedOut: the lock was still held when the caller ran out of time.
	TimedOut
	// Canceled: the caller's context ended the wait.
	Canceled
	// Replaced: the lock file at the path kept being replaced underneath
	// us, so a lock on it could not be made to mean anything. See the
	// discussion of the open/lock race in lock.go.
	Replaced
	// AlreadyHeld: the lock was taken on a Lock that already holds it.
	AlreadyHeld
	// NotHeld: Unlock was called on a Lock that does not hold the lock.
	NotHeld
	// Closed: the Lock has been closed.
	Closed
	// Failed: an operating system call failed. The cause is in Err.
	Failed
)

func (k Kind) String() string {
	switch k {
	case Contended:
		return "file is locked"
	case TimedOut:
		return "timed out waiting for the lock"
	case Canceled:
		return "wait canceled"
	case Replaced:
		return "lock file kept being replaced"
	case AlreadyHeld:
		return "lock is already held by this Lock"
	case NotHeld:
		return "lock is not held by this Lock"
	case Closed:
		return "lock is closed"
	case Failed:
		return "operation failed"
	}
	return "unknown error"
}

// An Error is every error this package reports.
//
// Two of them are values callers can compare against - [ErrLocked] and
// [ErrTimeout] - and the methods below let the rest be handled without
// enumerating them: [Error.IsTimeout] separates "not yet" from "no", and
// [Error.IsTemporary] separates a failure worth retrying from one that will
// keep happening.
type Error struct {
	// Op is the operation that failed: "new", "lock", "unlock", "close".
	// It is empty on the sentinel values.
	Op string
	// Path is the lock file. It is empty on the sentinel values.
	Path string
	// Kind is what went wrong.
	Kind Kind
	// Err is the underlying cause, if there was one.
	Err error
}

// ErrLocked and ErrTimeout are the two outcomes worth branching on. They are
// returned as these exact values, so `err == fslock.ErrLocked` works as well
// as errors.Is; an *Error of the same Kind matches them either way.
var (
	// ErrLocked is reported by [Lock.TryLock] when someone else holds the
	// lock.
	ErrLocked = &Error{Kind: Contended}

	// ErrTimeout is reported by [Lock.LockWithTimeout] when the lock was
	// still held when the time ran out.
	ErrTimeout = &Error{Kind: TimedOut}
)

func (e *Error) Error() string {
	msg := e.Kind.String()
	if e.Err != nil && e.Kind == Failed {
		msg = e.Err.Error()
	} else if e.Err != nil {
		msg = msg + ": " + e.Err.Error()
	}
	switch {
	case e.Op != "" && e.Path != "":
		return "fslock " + e.Op + " " + e.Path + ": " + msg
	case e.Path != "":
		return "fslock " + e.Path + ": " + msg
	case e.Op != "":
		return "fslock " + e.Op + ": " + msg
	}
	return "fslock: " + msg
}

// Unwrap returns the underlying cause, so that errors.Is and errors.As reach
// a syscall error or a context error underneath.
func (e *Error) Unwrap() error { return e.Err }

// Is matches an *Error against the package's sentinel values by Kind, and
// matches the closed state against fs.ErrClosed.
func (e *Error) Is(target error) bool {
	if t, ok := target.(*Error); ok {
		// Only a bare sentinel matches by kind; a fully populated Error
		// is matched by identity, as usual.
		if t.Op == "" && t.Path == "" && t.Err == nil {
			return t.Kind == e.Kind
		}
		return false
	}
	return e.Kind == Closed && target == fs.ErrClosed
}

// IsTimeout reports whether the lock was not taken because time ran out,
// rather than because something failed. A timeout says nothing about the lock
// file itself: the holder was simply still holding it.
func (e *Error) IsTimeout() bool {
	if e.Kind == TimedOut {
		return true
	}
	if errors.Is(e.Err, context.DeadlineExceeded) || errors.Is(e.Err, os.ErrDeadlineExceeded) {
		return true
	}
	var t interface{ Timeout() bool }
	return errors.As(e.Err, &t) && t.Timeout()
}

// IsTemporary reports whether trying the same thing later might work.
// Contention, timeouts and a lock file being replaced are all temporary; a
// closed Lock, a misused Lock, and most failed system calls are not.
func (e *Error) IsTemporary() bool {
	switch e.Kind {
	case Contended, TimedOut, Replaced:
		return true
	case Canceled, AlreadyHeld, NotHeld, Closed:
		return false
	}
	// A failed system call: some of them are worth another go.
	switch {
	case errors.Is(e.Err, syscall.EINTR),
		errors.Is(e.Err, syscall.EAGAIN),
		errors.Is(e.Err, syscall.ENOLCK),
		errors.Is(e.Err, syscall.ENOMEM):
		return true
	}
	var t interface{ Temporary() bool }
	return errors.As(e.Err, &t) && t.Temporary()
}

// IsTimeout reports whether err is an [*Error] that [Error.IsTimeout]
// accepts. It is false for a nil error and for errors from elsewhere.
func IsTimeout(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.IsTimeout()
}

// IsTemporary reports whether err is an [*Error] that [Error.IsTemporary]
// accepts. It is false for a nil error and for errors from elsewhere.
func IsTemporary(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.IsTemporary()
}

// Constructors for the errors raised below.

func failure(op, path string, err error) *Error {
	return &Error{Op: op, Path: path, Kind: Failed, Err: err}
}

func stateError(op, path string, kind Kind) *Error {
	return &Error{Op: op, Path: path, Kind: kind}
}

func canceled(op, path string, err error) *Error {
	return &Error{Op: op, Path: path, Kind: Canceled, Err: err}
}

// timedOut reports a deadline that passed. A wait bounded by a context
// deadline keeps the context error underneath, so that a caller who checks
// for context.DeadlineExceeded also finds what it is looking for.
func timedOut(op, path string, err error) *Error {
	if op == "" && path == "" && err == nil {
		return ErrTimeout
	}
	return &Error{Op: op, Path: path, Kind: TimedOut, Err: err}
}

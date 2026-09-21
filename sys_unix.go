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

//go:build linux || darwin

package fslock

import (
	"context"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// On linux and darwin the lock is an flock(2) lock.
//
// flock locks belong to the open file description, not to the process, which
// is what lets two Locks in one process exclude each other: each has its own
// descriptor from its own open. (POSIX record locks - fcntl F_SETLK - would
// not do: they belong to the process, so a second Lock here would be handed a
// lock that is already held, and closing any descriptor for the file would
// drop locks taken through every other one.)
//
// The kernel drops the lock when the last descriptor for the open file
// description is closed and when the process dies, however it dies, so a
// crashed holder never leaves a lock behind.

// sysWaitsInKernel is false here: flock(2) can block until the lock is free,
// but a thread blocked in it cannot be woken to be told the caller gave up.
// There is no descriptor to select on and no cancellation to hand it. A wait
// that must answer to a deadline or a context is therefore a retry loop in
// the caller, built on the non-blocking form below.
const sysWaitsInKernel = false

// sysAcquire tries once to take the lock, reporting whether it got it.
// block, ctx and deadline are accepted to match the windows implementation
// and ignored here; see sysWaitsInKernel.
func sysAcquire(f *os.File, block bool, ctx context.Context, deadline time.Time) (bool, error) {
	var locked bool
	var lockErr error
	err := control(f, func(fd uintptr) {
		for {
			switch err := unix.Flock(int(fd), unix.LOCK_EX|unix.LOCK_NB); err {
			case nil:
				locked = true
				return
			case unix.EINTR:
				continue
			case unix.EWOULDBLOCK:
				// Someone else holds it. An answer, not a failure.
				return
			default:
				lockErr = err
				return
			}
		}
	})
	if err != nil {
		return false, err
	}
	return locked, lockErr
}

// sysRelease drops the lock.
func sysRelease(f *os.File) error {
	var unlockErr error
	err := control(f, func(fd uintptr) {
		for {
			switch err := unix.Flock(int(fd), unix.LOCK_UN); err {
			case nil:
				return
			case unix.EINTR:
				continue
			default:
				unlockErr = err
				return
			}
		}
	})
	if err != nil {
		return err
	}
	return unlockErr
}

// sysIdentify returns the filesystem's identity for the open file.
func sysIdentify(f *os.File) (fileID, error) {
	var id fileID
	var statErr error
	err := control(f, func(fd uintptr) {
		var st unix.Stat_t
		if err := unix.Fstat(int(fd), &st); err != nil {
			statErr = err
			return
		}
		id = fileID{dev: uint64(st.Dev), ino: uint64(st.Ino)}
	})
	if err != nil {
		return fileID{}, err
	}
	return id, statErr
}

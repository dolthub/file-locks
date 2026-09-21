// Copyright 2026 Dolthub, Inc. Licensed under the Apache License, Version 2.0.
// See the LICENSE file in the root of this repository.

//go:build linux || darwin

package fslock

import (
	"context"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// flock(2) locks belong to the open file, not to the process, so two Locks in
// one process exclude each other the same way two processes do, and the
// kernel drops the lock when the process dies. (POSIX record locks belong to
// the process and would do neither.)

// sysWaits is false: a thread blocked in flock(2) cannot be told the caller
// gave up, so waiting is the caller's retry loop over the non-blocking form.
const sysWaits = false

// sysLock tries once to take the lock. wait, ctx and deadline are for the
// windows implementation and unused here.
func sysLock(f *os.File, wait bool, ctx context.Context, deadline time.Time) (bool, error) {
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

func sysUnlock(f *os.File) error {
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

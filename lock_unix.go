// Copyright 2026 Dolthub, Inc. Licensed under the Apache License, Version 2.0.
// See the LICENSE file in the root of this repository.

//go:build linux || darwin

package fslock

import (
	"os"

	"golang.org/x/sys/unix"
)

// flock(2) locks belong to the open file, not to the process, so two Locks in
// one process exclude each other the same way two processes do, and the
// kernel drops the lock when the process dies. (POSIX record locks belong to
// the process and would do neither.)

// sysOpen opens the lock file, creating it if it is not there. os.OpenFile
// sets close-on-exec atomically, which matters here: a descriptor that
// reached a forked child would keep the lock alive after this process gave it
// up, because the lock belongs to the open file.
func sysOpen(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0666)
}

// sysLock takes the lock, waiting in the kernel when wait is set. A thread
// blocked in flock(2) cannot be told the caller gave up, so a wait that has a
// deadline or a context asks for LOCK_NB and retries instead.
func sysLock(f *os.File, wait bool) (bool, error) {
	how := unix.LOCK_EX | unix.LOCK_NB
	if wait {
		how = unix.LOCK_EX
	}
	var locked bool
	var lockErr error
	err := control(f, func(fd uintptr) {
		for {
			switch err := unix.Flock(int(fd), how); err {
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

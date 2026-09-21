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

// Command locktool takes file locks from the shell, for checking by hand how
// a program that uses this package behaves against another participant.
//
//	locktool hold <path> [duration]   take the lock and keep it
//	locktool try <path>               report whether the lock is free
//	locktool wait <path> <duration>   wait up to duration for the lock
//
// hold exits when the duration elapses, or on interrupt if none was given.
// try and wait exit 0 when they took the lock and 3 when someone else has
// it, so a shell can branch on them.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"time"

	fslock "github.com/dolthub/file-locks"
)

const (
	exitError  = 1
	exitLocked = 3
)

func main() {
	if len(os.Args) < 3 {
		usage()
	}
	cmd, path := os.Args[1], os.Args[2]

	switch cmd {
	case "hold":
		hold(path, os.Args[3:])
	case "try":
		try(path)
	case "wait":
		if len(os.Args) < 4 {
			usage()
		}
		wait(path, os.Args[3])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: locktool hold|try|wait <path> [duration]")
	os.Exit(exitError)
}

func hold(path string, rest []string) {
	lck := open(path)
	defer lck.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := lck.LockWithContext(ctx); err != nil {
		fail(err)
	}
	fmt.Printf("holding %s\n", path)

	if len(rest) > 0 {
		d, err := time.ParseDuration(rest[0])
		if err != nil {
			fail(err)
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	<-ctx.Done()

	if err := lck.Unlock(); err != nil {
		fail(err)
	}
	fmt.Printf("released %s\n", path)
}

func try(path string) {
	lck := open(path)
	defer lck.Close()

	switch err := lck.TryLock(); {
	case errors.Is(err, fslock.ErrLocked):
		fmt.Printf("%s is held\n", path)
		os.Exit(exitLocked)
	case err != nil:
		fail(err)
	}
	fmt.Printf("%s is free\n", path)
}

func wait(path, limit string) {
	d, err := time.ParseDuration(limit)
	if err != nil {
		fail(err)
	}
	lck := open(path)
	defer lck.Close()

	start := time.Now()
	switch err := lck.LockWithTimeout(d); {
	case errors.Is(err, fslock.ErrTimeout):
		fmt.Printf("%s is still held after %s\n", path, d)
		os.Exit(exitLocked)
	case err != nil:
		fail(err)
	}
	fmt.Printf("took %s after %s\n", path, time.Since(start).Round(time.Millisecond))
}

func open(path string) *fslock.Lock {
	lck, err := fslock.New(path)
	if err != nil {
		fail(err)
	}
	return lck
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "locktool: %v (temporary=%v)\n", err, fslock.IsTemporary(err))
	os.Exit(exitError)
}

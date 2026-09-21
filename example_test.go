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

package fslock_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	fslock "github.com/dolthub/file-locks"
)

func tempDir() string {
	dir, err := os.MkdirTemp("", "fslock-example")
	if err != nil {
		log.Fatal(err)
	}
	return dir
}

// A Lock is opened once and taken around the work it protects. Running out
// of patience is an ordinary outcome worth handling.
func Example() {
	dir := tempDir()
	defer os.RemoveAll(dir)

	lck, err := fslock.New(filepath.Join(dir, "LOCK"))
	if err != nil {
		log.Fatal(err)
	}
	defer lck.Close()

	if err := lck.LockWithTimeout(100 * time.Millisecond); err != nil {
		if errors.Is(err, fslock.ErrTimeout) {
			fmt.Println("someone else is working here; carrying on read-only")
			return
		}
		log.Fatal(err)
	}
	defer lck.Unlock()

	fmt.Println("updating the store with the lock held")
	// Output: updating the store with the lock held
}

// TryLock answers whether the lock is free without waiting. ErrLocked is an
// answer, not a failure.
func ExampleLock_TryLock() {
	dir := tempDir()
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "LOCK")

	first, err := fslock.New(path)
	if err != nil {
		log.Fatal(err)
	}
	defer first.Close()

	second, err := fslock.New(path)
	if err != nil {
		log.Fatal(err)
	}
	defer second.Close()

	fmt.Println("first: ", first.TryLock())
	fmt.Println("second:", second.TryLock())

	// Output:
	// first:  <nil>
	// second: fslock: file is locked
}

// LockWithContext is the one addition to the interface: a wait the caller can
// call off.
func ExampleLock_LockWithContext() {
	dir := tempDir()
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "LOCK")

	holder, err := fslock.New(path)
	if err != nil {
		log.Fatal(err)
	}
	defer holder.Close()
	if err := holder.Lock(); err != nil {
		log.Fatal(err)
	}

	waiter, err := fslock.New(path)
	if err != nil {
		log.Fatal(err)
	}
	defer waiter.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	err = waiter.LockWithContext(ctx)
	fmt.Println("gave up:", errors.Is(err, context.Canceled))
	fmt.Println("timeout:", fslock.IsTimeout(err))
	// Output:
	// gave up: true
	// timeout: false
}

// Whatever went wrong, the error says whether waiting would have helped and
// whether trying again might.
func ExampleIsTemporary() {
	dir := tempDir()
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "LOCK")

	holder, err := fslock.New(path)
	if err != nil {
		log.Fatal(err)
	}
	defer holder.Close()
	if err := holder.Lock(); err != nil {
		log.Fatal(err)
	}

	waiter, err := fslock.New(path)
	if err != nil {
		log.Fatal(err)
	}
	defer waiter.Close()

	err = waiter.LockWithTimeout(10 * time.Millisecond)
	fmt.Println("timeout: ", fslock.IsTimeout(err))
	fmt.Println("temporary:", fslock.IsTemporary(err))

	_, err = fslock.New(filepath.Join(dir, "no-such-dir", "LOCK"))
	fmt.Println("temporary:", fslock.IsTemporary(err))

	// Output:
	// timeout:  true
	// temporary: true
	// temporary: false
}

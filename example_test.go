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

package filelock_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	filelock "github.com/dolthub/file-locks"
)

func tempDir() string {
	dir, err := os.MkdirTemp("", "filelock-example")
	if err != nil {
		log.Fatal(err)
	}
	return dir
}

// A guard is opened once and acquired around the work it protects. Running out
// of patience is an ordinary outcome worth handling.
func Example() {
	dir := tempDir()
	defer os.RemoveAll(dir)

	g, err := filelock.Open(filepath.Join(dir, "LOCK"))
	if err != nil {
		log.Fatal(err)
	}
	defer g.Close()

	if err := g.AcquireWithin(context.Background(), 100*time.Millisecond); err != nil {
		if errors.Is(err, filelock.ErrWaitExpired) {
			fmt.Println("someone else is working here; carrying on read-only")
			return
		}
		log.Fatal(err)
	}
	defer g.Release()

	fmt.Println("updating the store with the claim held")
	// Output: updating the store with the claim held
}

// AcquireNow answers whether the claim is free without waiting. A false return
// is an answer, not an error.
func ExampleGuard_AcquireNow() {
	dir := tempDir()
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "LOCK")

	first, err := filelock.Open(path)
	if err != nil {
		log.Fatal(err)
	}
	defer first.Close()

	second, err := filelock.Open(path)
	if err != nil {
		log.Fatal(err)
	}
	defer second.Close()

	got, err := first.AcquireNow()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("first:", got)

	got, err = second.AcquireNow()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("second:", got)

	// Output:
	// first: true
	// second: false
}

// Hold is the whole lifecycle for work that needs the claim once.
func ExampleHold() {
	dir := tempDir()
	defer os.RemoveAll(dir)

	err := filelock.Hold(context.Background(), filepath.Join(dir, "LOCK"), time.Second, func() error {
		fmt.Println("compacting")
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}
	// Output: compacting
}

// With a holder stamp, a guard that cannot acquire can say who has the claim.
func ExampleWithHolderStamp() {
	dir := tempDir()
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "LOCK")

	holder, err := filelock.Open(path, filelock.WithHolderStamp())
	if err != nil {
		log.Fatal(err)
	}
	defer holder.Close()
	if _, err := holder.AcquireNow(); err != nil {
		log.Fatal(err)
	}

	waiter, err := filelock.Open(path)
	if err != nil {
		log.Fatal(err)
	}
	defer waiter.Close()

	if err := waiter.AcquireWithin(context.Background(), 10*time.Millisecond); errors.Is(err, filelock.ErrWaitExpired) {
		if who, err := waiter.ReadHolder(); err == nil {
			fmt.Println("held by pid", who.PID == os.Getpid())
		}
	}
	// Output: held by pid true
}

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

// Command locktool takes and inspects file locks from the shell, for checking
// by hand how a program that uses this package behaves against another
// participant.
//
//	locktool hold <path> [duration]   take the claim and keep it
//	locktool try <path>               report whether the claim is free
//	locktool wait <path> <duration>   wait up to duration for the claim
//	locktool who <path>               print the holder stamp, if there is one
//
// hold exits when the duration elapses, or on interrupt if none was given.
// try and wait exit 0 when they got the claim and 3 when someone else has it,
// so a shell can branch on them.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"time"

	filelock "github.com/dolthub/file-locks"
)

const (
	exitError = 1
	exitBusy  = 3
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
	case "who":
		who(path)
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: locktool hold|try|wait|who <path> [duration]")
	os.Exit(exitError)
}

func hold(path string, rest []string) {
	g := open(path, filelock.WithHolderStamp())
	defer g.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := g.Acquire(ctx); err != nil {
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

	if err := g.Release(); err != nil {
		fail(err)
	}
	fmt.Printf("released %s\n", path)
}

func try(path string) {
	g := open(path)
	defer g.Close()

	ok, err := g.AcquireNow()
	if err != nil {
		fail(err)
	}
	if !ok {
		fmt.Printf("%s is held%s\n", path, byWhom(g))
		os.Exit(exitBusy)
	}
	fmt.Printf("%s is free\n", path)
}

func wait(path, limit string) {
	d, err := time.ParseDuration(limit)
	if err != nil {
		fail(err)
	}
	g := open(path)
	defer g.Close()

	start := time.Now()
	switch err := g.AcquireWithin(context.Background(), d); {
	case errors.Is(err, filelock.ErrWaitExpired):
		fmt.Printf("%s is still held after %s%s\n", path, d, byWhom(g))
		os.Exit(exitBusy)
	case err != nil:
		fail(err)
	}
	fmt.Printf("took %s after %s\n", path, time.Since(start).Round(time.Millisecond))
}

func who(path string) {
	h, err := filelock.ReadHolder(path)
	if errors.Is(err, filelock.ErrNoHolderStamp) {
		fmt.Printf("%s carries no holder stamp\n", path)
		return
	}
	if err != nil {
		fail(err)
	}
	fmt.Println(h)
}

// byWhom describes the holder if it left a stamp, for a message about a claim
// we could not take.
func byWhom(g *filelock.Guard) string {
	h, err := g.ReadHolder()
	if err != nil {
		return ""
	}
	return " by " + h.String()
}

func open(path string, opts ...filelock.Option) *filelock.Guard {
	g, err := filelock.Open(path, opts...)
	if err != nil {
		fail(err)
	}
	return g
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "locktool:", err)
	os.Exit(exitError)
}

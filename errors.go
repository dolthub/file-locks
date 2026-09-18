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

import "context"

// Errors reported by this package. Test for them with [errors.Is]; the values
// callers see are wrapped with the lock file's path.
var (
	// ErrWaitExpired means the claim was not free before the caller ran out
	// of time, either because a wait hit its own limit or because the
	// context's deadline passed. It is a report about contention, not a
	// failure: the lock file is fine and trying again later may well work.
	//
	// errors.Is(err, context.DeadlineExceeded) also reports true.
	ErrWaitExpired error = waitExpired{}

	// ErrAlreadyHolding means this Guard already holds the claim. A Guard
	// holds it once; it is not a recursive mutex.
	ErrAlreadyHolding = errorString("filelock: this guard already holds the claim")

	// ErrNotHolding means Release was called on a Guard that does not hold
	// the claim.
	ErrNotHolding = errorString("filelock: this guard does not hold the claim")

	// ErrGuardClosed means the Guard was closed and can no longer be used.
	ErrGuardClosed = errorString("filelock: guard is closed")

	// ErrNoHolderStamp means the lock file carries no holder stamp, either
	// because whoever holds it did not ask for one (see [WithHolderStamp])
	// or because nobody holds it.
	ErrNoHolderStamp = errorString("filelock: lock file carries no holder stamp")
)

// errorString is a comparable sentinel error, like errors.New but usable in a
// const-style var block without allocating a pointer identity.
type errorString string

func (e errorString) Error() string { return string(e) }

// waitExpired is the type of [ErrWaitExpired]. It reports itself as a deadline
// so that callers who reach for the standard library's vocabulary - either
// context.DeadlineExceeded or a net.Error-style Timeout method - find what
// they expect.
type waitExpired struct{}

func (waitExpired) Error() string { return "filelock: gave up waiting for the claim" }

func (waitExpired) Is(target error) bool { return target == context.DeadlineExceeded }

func (waitExpired) Timeout() bool { return true }

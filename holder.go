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

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// A Holder describes whoever last took a claim on a lock file with
// [WithHolderStamp] in effect.
//
// It is a comment, not a record. Nothing keeps it in step with the claim
// itself: the stamp outlives the holder that wrote it, a holder that did not
// ask for a stamp leaves none, and the writer and the reader may not even
// agree on what a process id means if the lock file is shared over a network.
// Use it in messages to people; never in a decision.
type Holder struct {
	// PID is the process id of the holder, on the holder's machine.
	PID int `json:"pid"`
	// Host is the holder's hostname, empty if it could not be determined.
	Host string `json:"host,omitempty"`
	// Since is when the holder took the claim, in the holder's clock.
	Since time.Time `json:"since"`
}

// String describes the holder in a form fit for an error message.
func (h Holder) String() string {
	where := h.Host
	if where == "" {
		where = "this host"
	}
	if h.Since.IsZero() {
		return fmt.Sprintf("pid %d on %s", h.PID, where)
	}
	return fmt.Sprintf("pid %d on %s since %s", h.PID, where, h.Since.Format(time.RFC3339))
}

// ReadHolder reads the holder stamp from the lock file this Guard was opened
// on. See [Holder] for how much to trust it. It reports [ErrNoHolderStamp] if
// the file carries no stamp.
func (g *Guard) ReadHolder() (Holder, error) {
	return ReadHolder(g.path)
}

// ReadHolder reads the holder stamp from the lock file at path without taking
// any claim on it. See [Holder] for how much to trust it.
func ReadHolder(path string) (Holder, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Holder{}, err
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return Holder{}, fmt.Errorf("%w (%s)", ErrNoHolderStamp, path)
	}
	var h Holder
	if err := json.Unmarshal(data, &h); err != nil {
		return Holder{}, fmt.Errorf("%w (%s): %v", ErrNoHolderStamp, path, err)
	}
	return h, nil
}

// writeHolder stamps the lock file through the handle that holds its claim.
func writeHolder(h sysHandle) error {
	data, err := json.Marshal(Holder{PID: os.Getpid(), Host: hostname(), Since: time.Now()})
	if err != nil {
		return err
	}
	return sysOverwrite(h, append(data, '\n'))
}

var hostname = sync.OnceValue(func() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return name
})

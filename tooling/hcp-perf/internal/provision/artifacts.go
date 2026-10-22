// Copyright 2026 Microsoft Corporation
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

package provision

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

type artifacts struct {
	root     *os.Root
	history  *os.File
	sequence int
}

type event struct {
	Time          time.Time `json:"time"`
	Method        string    `json:"method,omitempty"`
	URL           string    `json:"url,omitempty"`
	RequestID     string    `json:"requestID,omitempty"`
	CorrelationID string    `json:"correlationID,omitempty"`
	HTTPStatus    int       `json:"httpStatus,omitempty"`
	State         string    `json:"state,omitempty"`
}

func newArtifacts(dir string) (*artifacts, error) {
	if dir == "" {
		return nil, errors.New("a new output directory is required")
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	f, err := root.OpenFile("status.ndjson", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		root.Close()
		return nil, err
	}
	return &artifacts{root: root, history: f}, nil
}

func (a *artifacts) close() error {
	return errors.Join(a.history.Close(), a.root.Close())
}

func (a *artifacts) event(e event) error {
	e.Time = time.Now().UTC()
	if err := json.NewEncoder(a.history).Encode(e); err != nil {
		return err
	}
	return a.history.Sync()
}

func (a *artifacts) write(name string, data []byte) error {
	f, err := a.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	return errors.Join(writeErr, f.Sync(), f.Close())
}

func (a *artifacts) save(result Result) error {
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	if err := a.write(".operation.tmp", append(data, '\n')); err != nil {
		return err
	}
	if err := a.root.Rename(".operation.tmp", "operation.json"); err != nil {
		return err
	}
	dir, err := a.root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

// Bodies from the credential flow are never sent here, even on failure.
func (a *artifacts) snapshot(body []byte) error {
	a.sequence++
	return a.write(fmt.Sprintf("response-%04d.json", a.sequence), body)
}

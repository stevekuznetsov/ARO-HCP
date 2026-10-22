// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package iteration

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/performance"
	"os"
	"path/filepath"
	"time"
)

func (c *Controller) recheckPerformance(ctx context.Context, id string, index int, action Action) error {
	run, err := c.Status(ctx, id)
	if err != nil {
		return err
	}
	status := run.Actions[index]
	if !run.StopRequested || status.State != Blocked || len(status.Attempts) == 0 {
		return nil
	}
	attempt := status.Attempts[len(status.Attempts)-1]
	lock, err := acquireLock(c.attemptDir(id, attempt.Token) + ".lock")
	if errors.Is(err, errLocked) {
		return nil
	}
	if err != nil {
		return err
	}
	defer lock.Close()
	if sameProcess(attempt.Process) {
		return nil
	}
	checkCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	hash, err := performance.VerifyFinalized(checkCtx, *action.Performance, filepath.Join(c.attemptDir(id, attempt.Token), "performance"))
	if err != nil {
		return nil
	}
	receipt, err := json.Marshal(map[string]any{"attempt": attempt.Token, "specSHA256": run.SpecSHA256, "outcomeSHA256": hash, "verifiedAt": time.Now().UTC(), "verification": "read-only exact HCP/guest identity, drained canary and registered competitor Job identities/zero Pods when configured; population retained"})
	if err != nil {
		return err
	}
	// Publish only once; a reconciliation crash can safely repeat the live reads.
	path := filepath.Join(c.attemptDir(id, attempt.Token), "performance-verification.json")
	if err := writePrivate(path, receipt, 0400); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return c.update(ctx, id, func(current *Run) error {
		s := &current.Actions[index]
		latest := &s.Attempts[len(s.Attempts)-1]
		if !current.StopRequested || s.State != Blocked || latest.Token != attempt.Token {
			return errors.New("performance recovery fencing mismatch")
		}
		s.State, latest.State, s.Armed = Failed, Failed, false
		s.Message = "finalized failed performance verified read-only; no load replayed; population retained"
		summarize(current)
		return nil
	})
}

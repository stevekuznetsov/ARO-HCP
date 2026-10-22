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
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/retention"
)

// Recovery is limited to a stopped worker's fully verified final cycle. It only
// observes the recorded desired tags, never resumes the refresh loop or writes.
func (c *Controller) recheckRetention(ctx context.Context, id string, index int, action Action) error {
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
	file, err := os.Open(filepath.Join(c.attemptDir(id, attempt.Token), "retention.ndjson"))
	if err != nil {
		return nil
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var last retention.Result
	for scanner.Scan() {
		var cycle retention.Result
		if json.Unmarshal(scanner.Bytes(), &cycle) != nil {
			return nil
		}
		if cycle.Status == "verified" && !cycle.DryRun {
			last = cycle
			continue
		}
		for _, group := range cycle.ResourceGroups {
			if group.Attempted || group.Updated || group.PossiblyUpdated {
				return nil
			}
		}
	}
	if scanner.Err() != nil || last.Status != "verified" || last.DryRun {
		return nil
	}
	if err := verifyRetentionCycle(ctx, *action.Retention, last, nil); err != nil {
		return nil
	}
	return c.update(ctx, id, func(current *Run) error {
		status := &current.Actions[index]
		latest := &status.Attempts[len(status.Attempts)-1]
		if !current.StopRequested || status.State != Blocked || latest.Token != attempt.Token {
			return errors.New("retention verification fencing mismatch")
		}
		status.State, latest.State, status.Armed = Stopped, Stopped, false
		status.Message = "read-only verification of stopped retention final cycle; no writes replayed"
		status.RetentionVerification = &RetentionVerification{AttemptToken: attempt.Token, CreatedAt: last.CreatedAt, VerifiedAt: time.Now().UTC()}
		summarize(current)
		return nil
	})
}

func verifyRetentionCycle(ctx context.Context, spec retention.NativeSpec, last retention.Result, transport http.RoundTripper) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	if last.Status != "verified" || last.DryRun {
		return errors.New("cycle not verified")
	}
	if _, err := time.Parse(time.RFC3339Nano, last.CreatedAt); err != nil {
		return err
	}
	groups := append(append([]string{}, spec.Required.ResourceGroups...), spec.OptionalResourceGroups...)
	if len(groups) != len(last.ResourceGroups) {
		return errors.New("retention receipt set mismatch")
	}
	token, err := os.ReadFile(spec.TokenFile)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for index, id := range groups {
		g := last.ResourceGroups[index]
		if !strings.EqualFold(g.ID, id) {
			return errors.New("retention receipt identity mismatch")
		}
		optionalAbsent := index >= len(spec.Required.ResourceGroups) && g.Status == "not-created"
		if !optionalAbsent && (!g.Verified || g.PossiblyUpdated || g.NewCreatedAt != last.CreatedAt) {
			return errors.New("retention cycle incomplete")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://management.azure.com"+id+"?api-version=2021-04-01", nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if index >= len(spec.Required.ResourceGroups) && resp.StatusCode == 404 {
			continue
		}
		if readErr != nil || resp.StatusCode != 200 || optionalAbsent {
			return errors.New("retention resource state uncertain")
		}
		var current struct {
			ID   string            `json:"id"`
			Tags map[string]string `json:"tags"`
		}
		if json.Unmarshal(data, &current) != nil || !strings.EqualFold(current.ID, id) || current.Tags[g.TagKey] != last.CreatedAt {
			return errors.New("retention tag verification mismatch")
		}
	}
	return nil
}

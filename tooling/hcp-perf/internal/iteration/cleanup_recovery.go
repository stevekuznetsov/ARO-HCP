// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0.
package iteration

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/performance"
)

func (c *Controller) recheckCleanup(ctx context.Context, id string, index int, action Action) error {
	run, err := c.Status(ctx, id)
	if err != nil {
		return err
	}
	s := run.Actions[index]
	if !run.StopRequested || s.State != Blocked || len(s.Attempts) == 0 {
		return nil
	}
	a := s.Attempts[len(s.Attempts)-1]
	if sameProcess(a.Process) {
		return nil
	}
	lock, err := acquireLock(c.attemptDir(id, a.Token) + ".lock")
	if errors.Is(err, errLocked) {
		return nil
	}
	if err != nil {
		return err
	}
	defer lock.Close()
	check, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := performance.VerifyCleanup(check, *action.PerformanceCleanup, filepath.Join(c.attemptDir(id, a.Token), "cleanup")); err != nil {
		return nil
	}
	raw, err := json.Marshal(map[string]any{"attempt": a.Token, "at": time.Now().UTC(), "verification": "read-only frozen cleanup preflight, HCP/guest identity and every requested object absent; no delete replay"})
	if err != nil {
		return err
	}
	if err := writePrivate(filepath.Join(c.attemptDir(id, a.Token), "cleanup-verification.json"), raw, 0400); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return c.update(ctx, id, func(r *Run) error {
		s := &r.Actions[index]
		latest := &s.Attempts[len(s.Attempts)-1]
		if !r.StopRequested || s.State != Blocked || latest.Token != a.Token {
			return errors.New("cleanup recovery fence changed")
		}
		s.State, latest.State, s.Armed = Succeeded, Succeeded, false
		s.Message = "cleanup completion verified read-only; no writes replayed"
		summarize(r)
		return nil
	})
}

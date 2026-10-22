// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0.
package iteration

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/startup"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"os"
	"path/filepath"
	"reflect"
	"time"
)

func (c *Controller) recheckCatalogPin(ctx context.Context, id string, index int, action Action) error {
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
	client, err := startup.NewClients(action.CatalogPin.Kubeconfig, action.CatalogPin.AllowExecCredential)
	if err != nil {
		return nil
	}
	identity, err := startup.Discover(ctx, client, action.CatalogPin.Identity.ResourceID, action.CatalogPin.Identity)
	if err != nil || identity != action.CatalogPin.Identity {
		return nil
	}
	hc, err := client.Dynamic.Resource(startup.HostedClusters).Namespace(identity.HCNamespace).Get(ctx, identity.HCName, metav1.GetOptions{})
	if err != nil || string(hc.GetUID()) != identity.HCUID || !reflect.DeepEqual(hc.GetAnnotations(), action.CatalogPin.ExpectedAnnotations) {
		return nil
	}
	raw, err := json.Marshal(map[string]any{"at": time.Now().UTC(), "attempt": a.Token, "verification": "same HC/HCP identity and exact original annotation map; no retained catalog pin; no mutation replay"})
	if err != nil {
		return err
	}
	if err := writePrivate(filepath.Join(c.attemptDir(id, a.Token), "catalog-verification.json"), raw, 0400); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return c.update(ctx, id, func(r *Run) error {
		s := &r.Actions[index]
		latest := &s.Attempts[len(s.Attempts)-1]
		if !r.StopRequested || s.State != Blocked || latest.Token != a.Token {
			return errors.New("catalog recovery fence changed")
		}
		s.State, latest.State, s.Armed = Failed, Failed, false
		s.Message = "read-only verification found original annotations; no retained pin, no writes replayed"
		summarize(r)
		return nil
	})
}

// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0.
package performance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/seed"
)

// unloadPopulation follows a verified predecessor inventory. Each scale patch
// is issued once; uncertain failures leave the case blocked rather than replayed.
func unloadPopulation(ctx context.Context, client kubernetes.Interface, spec Spec, j *journal, inventory []populationObject) error {
	raw, err := os.ReadFile(filepath.Join(spec.ExistingPopulation.Directory, "spec.json"))
	if err != nil {
		return err
	}
	var prior Spec
	if err := json.Unmarshal(raw, &prior); err != nil {
		return err
	}
	if prior.Unloaded && !spec.RestorePopulation {
		return nil
	}
	from, to := int32(5), int32(0)
	prefix := "unloaded"
	if spec.RestorePopulation {
		from, to, prefix = 0, 5, "restored"
	}
	identities := map[string]string{}
	for _, o := range inventory {
		if o.Resource.Resource == "deployments" {
			identities[o.Namespace+"/"+o.Name] = o.UID
		}
	}
	for app := int64(0); app < spec.Seed.Applications; app++ {
		ns := fmt.Sprintf("%s-%d", spec.Seed.NamespacePrefix, app/20)
		name := fmt.Sprintf("app-%06d", app)
		d, err := client.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if string(d.UID) != identities[ns+"/"+name] || d.Labels[seed.RunIDLabel] != spec.Seed.ID || d.DeletionTimestamp != nil || d.Spec.Replicas == nil || *d.Spec.Replicas != from {
			return errors.New("unloaded scale identity/replica mismatch")
		}
		patch, err := json.Marshal([]map[string]any{{"op": "test", "path": "/metadata/uid", "value": string(d.UID)}, {"op": "test", "path": "/metadata/resourceVersion", "value": d.ResourceVersion}, {"op": "test", "path": "/spec/replicas", "value": from}, {"op": "replace", "path": "/spec/replicas", "value": to}})
		if err != nil {
			return err
		}
		if err := j.event(prefix+"-scale-intent", map[string]any{"namespace": ns, "name": name, "uid": d.UID, "resourceVersion": d.ResourceVersion, "from": from, "to": to}); err != nil {
			return err
		}
		updated, err := client.AppsV1().Deployments(ns).Patch(ctx, name, types.JSONPatchType, patch, metav1.PatchOptions{FieldManager: "hcp-perf-unloaded"})
		if err != nil {
			return err
		}
		if err := j.event(prefix+"-scale-response", map[string]any{"namespace": ns, "name": name, "uid": updated.UID, "resourceVersion": updated.ResourceVersion}); err != nil {
			return err
		}
	}
	for {
		ready := true
		for n := int64(0); n < (spec.Seed.Applications+19)/20; n++ {
			ns := fmt.Sprintf("%s-%d", spec.Seed.NamespacePrefix, n)
			pods, err := client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
			if err != nil {
				return err
			}
			if len(pods.Items) != int(min(int64(20), spec.Seed.Applications-n*20))*int(to) {
				ready = false
			}
			deploys, err := client.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
			if err != nil {
				return err
			}
			for _, d := range deploys.Items {
				if d.Labels[seed.RunIDLabel] != spec.Seed.ID || string(d.UID) != identities[ns+"/"+d.Name] || d.Spec.Replicas == nil || *d.Spec.Replicas != to {
					return errors.New("unloaded deployment drift")
				}
				if d.Status.Replicas != to || d.Status.ReadyReplicas != to || d.Status.UpdatedReplicas != to || d.Status.ObservedGeneration != d.Generation {
					ready = false
				}
			}
		}
		if ready {
			return j.event(prefix+"-population-converged", map[string]any{"applicationPods": int64(to) * spec.Seed.Applications, "deployments": spec.Seed.Applications, "turnoverEnabled": to != 0})
		}
		if err := wait(ctx, 2*time.Second); err != nil {
			return err
		}
	}
}

// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package performance

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/adversary"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/seed"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/startup"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/workload"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// VerifyFinalized proves failed execution finalized, its canary is drained, and
// any registered competitor Jobs are suspended with zero Pods. It is not success.
func VerifyFinalized(ctx context.Context, spec Spec, dir string) (string, error) {
	return verifyFinalized(ctx, spec, dir, false)
}

func finalizedOutcome(outcome Outcome, allowCompleted bool) bool {
	terminal := outcome.State == "failed" && outcome.ExecutionStatus == "failed"
	if allowCompleted {
		terminal = terminal || (outcome.State == "completed" || outcome.State == "failed") && outcome.ExecutionStatus == "completed"
	}
	return !outcome.Finished.IsZero() && !outcome.Started.IsZero() && !outcome.Finished.Before(outcome.Started) && terminal && (outcome.CleanupStatus == "not-needed" || outcome.CleanupStatus == "stopped-verified")
}

func verifyFinalized(ctx context.Context, spec Spec, dir string, allowCompleted bool) (string, error) {
	return verifyFinalizedCredential(ctx, spec, dir, allowCompleted, spec.GuestKubeconfig)
}

func verifyFinalizedCredential(ctx context.Context, spec Spec, dir string, allowCompleted bool, guestKubeconfig string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "spec.json"))
	if err != nil {
		return "", err
	}
	var frozen Spec
	if json.Unmarshal(raw, &frozen) != nil || !reflect.DeepEqual(frozen, spec) {
		return "", errors.New("performance spec receipt mismatch")
	}
	raw, err = os.ReadFile(filepath.Join(dir, "outcome.json"))
	if err != nil {
		return "", err
	}
	var outcome Outcome
	if json.Unmarshal(raw, &outcome) != nil || !finalizedOutcome(outcome, allowCompleted) {
		return "", errors.New("performance finalization unproven")
	}
	management, err := startup.NewClients(spec.Startup.Kubeconfig, spec.Startup.AllowExecCredential)
	if err != nil {
		return "", err
	}
	id, err := startup.Discover(ctx, management, spec.Startup.ResourceID, outcome.Identity)
	if err != nil || id != outcome.Identity || id.HCPUID == "" {
		return "", errors.New("performance identity changed")
	}
	if spec.Competitor != nil {
		if outcome.CleanupStatus != "stopped-verified" {
			return "", errors.New("competitor finalization unproven")
		}
		file, err := os.Open(filepath.Join(dir, "events.ndjson"))
		if err != nil {
			return "", err
		}
		defer file.Close()
		var workers []competitorWorker
		decoder := json.NewDecoder(file)
		for {
			var event struct {
				Kind  string          `json:"kind"`
				Value json.RawMessage `json:"value"`
			}
			if err := decoder.Decode(&event); err == io.EOF {
				break
			} else if err != nil {
				return "", err
			}
			if event.Kind == "competitor-identities" {
				if len(workers) != 0 {
					return "", errors.New("duplicate competitor identity receipt")
				}
				if err := json.Unmarshal(event.Value, &workers); err != nil {
					return "", err
				}
			}
		}
		status, err := adversary.Status(ctx, management.Kubernetes, spec.Competitor.Run.Namespace, spec.Competitor.Run.ID)
		if err != nil {
			return "", err
		}
		if err := stoppedWorkers(status, spec, workers, id.HCPNamespace); err != nil {
			return "", err
		}
	} else if outcome.CleanupStatus != "not-needed" {
		return "", errors.New("unexpected competitor cleanup receipt")
	}
	config, err := clientcmd.BuildConfigFromFlags("", guestKubeconfig)
	if err != nil {
		return "", err
	}
	config.Timeout = 20 * time.Second
	guest, err := kubernetes.NewForConfig(config)
	if err != nil {
		return "", err
	}
	dyn, err := dynamic.NewForConfig(config)
	if err != nil {
		return "", err
	}
	if err := verifyGuestIdentity(ctx, dyn, id.ClusterID); err != nil {
		return "", err
	}
	ns := spec.setup().CanaryNamespace()
	deployment, err := guest.AppsV1().Deployments(ns).Get(ctx, "canary", metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return "", err
	}
	if err == nil && (deployment.Labels[seed.RunIDLabel] != spec.Seed.ID || deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 0 || deployment.Status.Replicas != 0 || deployment.Status.ObservedGeneration < deployment.Generation) {
		return "", errors.New("canary not owned and drained")
	}
	if err == nil {
		if err := workload.VerifyCanaryIdentity(ctx, guest, spec.setup()); err != nil {
			return "", err
		}
	}
	pods, err := guest.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return "", err
	}
	if pods != nil && len(pods.Items) != 0 {
		return "", errors.New("canary pods remain")
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw)), nil
}

type CleanupObject struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	UID  string `json:"uid"`
}

type CleanupSpec struct {
	GuestKubeconfig string          `json:"guestKubeconfig,omitempty"`
	AbsentObjects   []CleanupObject `json:"absentObjects,omitempty"`
	Source          Spec            `json:"source"`
	Directory       string          `json:"directory"`
	OutcomeSHA256   string          `json:"outcomeSHA256"`
	Objects         []CleanupObject `json:"objects"`
}

func (s CleanupSpec) Validate() error {
	if s.GuestKubeconfig != "" && !filepath.IsAbs(s.GuestKubeconfig) {
		return errors.New("cleanup credential must be an absolute external path")
	}
	if err := s.Source.Validate(); err != nil {
		return err
	}
	if !filepath.IsAbs(s.Directory) || len(s.OutcomeSHA256) != 64 {
		return errors.New("cleanup requires absolute finalized case and outcome hash")
	}
	expected := map[string]bool{}
	canary := s.Source.setup().CanaryNamespace()
	expected["Namespace/"+canary] = true
	expected["FlowSchema/hcp-perf-canary-"+s.Source.Seed.ID] = true
	for _, profile := range []string{"benchmark", "canary"} {
		expected["PriorityLevelConfiguration/hcp-perf-"+profile+"-"+s.Source.Seed.ID] = true
	}
	for n := int64(0); n < (s.Source.Seed.Applications+19)/20; n++ {
		expected[fmt.Sprintf("Namespace/%s-%d", s.Source.Seed.NamespacePrefix, n)] = true
		expected[fmt.Sprintf("FlowSchema/hcp-perf-benchmark-%s-%d", s.Source.Seed.ID, n)] = true
	}
	if len(s.Objects)+len(s.AbsentObjects) != len(expected) {
		return errors.New("cleanup inventory incomplete")
	}
	for _, o := range s.Objects {
		key := o.Kind + "/" + o.Name
		if !expected[key] || o.UID == "" {
			return errors.New("cleanup object outside exact case inventory")
		}
		delete(expected, key)
	}
	for _, o := range s.AbsentObjects {
		key := o.Kind + "/" + o.Name
		if !expected[key] || o.UID != "" {
			return errors.New("absent cleanup object must have exact expected name and no fabricated UID")
		}
		delete(expected, key)
	}
	return nil
}

// Cleanup removes only reviewed namespace/APF UIDs after a complete preflight.
// No PVC/PV deletion or finalizer removal is supported. Lost writes never replay.
func Cleanup(ctx context.Context, spec CleanupSpec, dir string) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return err
	}
	if err := publish(dir, "spec.json", spec); err != nil {
		return err
	}
	credential := spec.Source.GuestKubeconfig
	if spec.GuestKubeconfig != "" {
		credential = spec.GuestKubeconfig
	}
	hash, err := verifyFinalizedCredential(ctx, spec.Source, spec.Directory, true, credential)
	if err != nil {
		return err
	}
	if hash != spec.OutcomeSHA256 {
		return errors.New("cleanup outcome hash mismatch")
	}
	config, err := clientcmd.BuildConfigFromFlags("", credential)
	if err != nil {
		return err
	}
	config.Timeout = 20 * time.Second
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return err
	}
	resources := map[string]schema.GroupVersionResource{"Namespace": {Version: "v1", Resource: "namespaces"}, "FlowSchema": {Group: "flowcontrol.apiserver.k8s.io", Version: "v1", Resource: "flowschemas"}, "PriorityLevelConfiguration": {Group: "flowcontrol.apiserver.k8s.io", Version: "v1", Resource: "prioritylevelconfigurations"}}
	if len(spec.AbsentObjects) != 0 {
		raw, err := os.ReadFile(filepath.Join(spec.Directory, "outcome.json"))
		if err != nil {
			return err
		}
		var outcome Outcome
		if json.Unmarshal(raw, &outcome) != nil || outcome.Phase != "seed" || outcome.ExecutionStatus != "failed" {
			return errors.New("absent setup objects require finalized seed-stage failure")
		}
		for _, o := range spec.AbsentObjects {
			if _, err := client.Resource(resources[o.Kind]).Get(ctx, o.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
				return errors.New("expected absent setup object exists or cannot be verified")
			}
		}
	}
	versions := map[string]string{}
	for _, o := range spec.Objects {
		obj, err := client.Resource(resources[o.Kind]).Get(ctx, o.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if err := cleanupIdentity(o, string(obj.GetUID()), obj.GetLabels()[seed.RunIDLabel], spec.Source.Seed.ID, obj.GetDeletionTimestamp() != nil); err != nil {
			return errors.New("cleanup UID/ownership mismatch")
		}
		versions[o.Kind+"/"+o.Name] = obj.GetResourceVersion()
		if o.Kind != "Namespace" {
			continue
		}
		pvcs, err := client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}).Namespace(o.Name).List(ctx, metav1.ListOptions{})
		if err != nil {
			return err
		}
		if len(pvcs.Items) != 0 {
			return errors.New("refusing namespace cleanup containing PVCs")
		}
		pods, err := client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "pods"}).Namespace(o.Name).List(ctx, metav1.ListOptions{})
		if err != nil {
			return err
		}
		for _, pod := range pods.Items {
			if pod.GetLabels()[seed.RunIDLabel] != spec.Source.Seed.ID {
				return errors.New("unexpected namespace pod")
			}
		}
		for _, gvr := range []schema.GroupVersionResource{{Group: "apps", Version: "v1", Resource: "deployments"}, {Group: "batch", Version: "v1", Resource: "jobs"}, {Group: "batch", Version: "v1", Resource: "cronjobs"}, {Group: "apps", Version: "v1", Resource: "statefulsets"}, {Group: "apps", Version: "v1", Resource: "daemonsets"}} {
			list, err := client.Resource(gvr).Namespace(o.Name).List(ctx, metav1.ListOptions{})
			if err != nil {
				return err
			}
			for _, v := range list.Items {
				if gvr.Resource != "deployments" || v.GetLabels()[seed.RunIDLabel] != spec.Source.Seed.ID {
					return errors.New("unexpected namespace workload")
				}
			}
		}
	}
	if err := publish(dir, "preflight.json", map[string]any{"at": time.Now().UTC(), "outcomeSHA256": hash, "objects": spec.Objects, "absentObjects": spec.AbsentObjects}); err != nil {
		return err
	}
	for _, kind := range []string{"Namespace", "FlowSchema", "PriorityLevelConfiguration"} {
		for _, o := range spec.Objects {
			if o.Kind != kind {
				continue
			}
			uid := types.UID(o.UID)
			rv := versions[o.Kind+"/"+o.Name]
			if err := client.Resource(resources[kind]).Delete(ctx, o.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}}); err != nil {
				return err
			}
		}
	}
	for {
		remaining := 0
		for _, o := range spec.Objects {
			_, err := client.Resource(resources[o.Kind]).Get(ctx, o.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return err
			}
			remaining++
		}
		if remaining == 0 {
			break
		}
		if err := wait(ctx, time.Second); err != nil {
			return err
		}
	}
	return publish(dir, "outcome.json", map[string]any{"state": "deleted-verified", "at": time.Now().UTC(), "objects": spec.Objects})
}

func cleanupIdentity(expected CleanupObject, uid, owner, run string, deleting bool) error {
	if uid != expected.UID || owner != run || deleting {
		return errors.New("cleanup UID/ownership mismatch")
	}
	return nil
}

// VerifyCleanup only observes a previously preflighted deletion; it never replays it.
func VerifyCleanup(ctx context.Context, spec CleanupSpec, dir string) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(dir, "spec.json"))
	if err != nil {
		return err
	}
	var frozen CleanupSpec
	if json.Unmarshal(raw, &frozen) != nil || !reflect.DeepEqual(frozen, spec) {
		return errors.New("cleanup spec receipt mismatch")
	}
	raw, err = os.ReadFile(filepath.Join(dir, "preflight.json"))
	if err != nil {
		return err
	}
	var receipt struct {
		OutcomeSHA256 string          `json:"outcomeSHA256"`
		Objects       []CleanupObject `json:"objects"`
	}
	if json.Unmarshal(raw, &receipt) != nil || receipt.OutcomeSHA256 != spec.OutcomeSHA256 || !reflect.DeepEqual(receipt.Objects, spec.Objects) {
		return errors.New("cleanup preflight receipt mismatch")
	}
	management, err := startup.NewClients(spec.Source.Startup.Kubeconfig, spec.Source.Startup.AllowExecCredential)
	if err != nil {
		return err
	}
	raw, err = os.ReadFile(filepath.Join(spec.Directory, "outcome.json"))
	if err != nil {
		return err
	}
	if fmt.Sprintf("%x", sha256.Sum256(raw)) != spec.OutcomeSHA256 {
		return errors.New("source outcome changed")
	}
	var outcome Outcome
	if json.Unmarshal(raw, &outcome) != nil {
		return errors.New("invalid source outcome")
	}
	identity, err := startup.Discover(ctx, management, spec.Source.Startup.ResourceID, outcome.Identity)
	if err != nil || identity != outcome.Identity {
		return errors.New("cleanup identity changed")
	}
	credential := spec.Source.GuestKubeconfig
	if spec.GuestKubeconfig != "" {
		credential = spec.GuestKubeconfig
	}
	config, err := clientcmd.BuildConfigFromFlags("", credential)
	if err != nil {
		return err
	}
	config.Timeout = 20 * time.Second
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return err
	}
	if err := verifyGuestIdentity(ctx, client, identity.ClusterID); err != nil {
		return err
	}
	resources := map[string]schema.GroupVersionResource{"Namespace": {Version: "v1", Resource: "namespaces"}, "FlowSchema": {Group: "flowcontrol.apiserver.k8s.io", Version: "v1", Resource: "flowschemas"}, "PriorityLevelConfiguration": {Group: "flowcontrol.apiserver.k8s.io", Version: "v1", Resource: "prioritylevelconfigurations"}}
	for _, o := range append(append([]CleanupObject{}, spec.Objects...), spec.AbsentObjects...) {
		if _, err := client.Resource(resources[o.Kind]).Get(ctx, o.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			return errors.New("cleanup object still present or unreadable")
		}
	}
	return nil
}

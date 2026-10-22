// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0.
package performance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/seed"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/startup"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/workload"
)

type PopulationReference struct {
	ScreenedPopulation *ScreenedPopulationProof `json:"screenedPopulation,omitempty"`
	ImageBindings      []PopulationImageBinding `json:"imageBindings,omitempty"`
	RecoverFailed      bool                     `json:"recoverFailed,omitempty"`
	Directory          string                   `json:"directory"`
	OutcomeSHA256      string                   `json:"outcomeSHA256"`
	InventorySHA256    string                   `json:"inventorySHA256"`
}

// ScreenedPopulationProof explicitly selects the finalized run's own inventory,
// not failed-execution rollout recovery against an ancestor inventory.
type ScreenedPopulationProof struct {
	SpecSHA256    string `json:"specSHA256"`
	ResultsSHA256 string `json:"resultsSHA256"`
}

func (r PopulationReference) validateScreenedPopulation() error {
	if r.ScreenedPopulation == nil {
		return nil
	}
	if r.RecoverFailed {
		return errors.New("screened population and failed recovery are mutually exclusive")
	}
	for _, hash := range []string{r.OutcomeSHA256, r.InventorySHA256, r.ScreenedPopulation.SpecSHA256, r.ScreenedPopulation.ResultsSHA256} {
		decoded, err := hex.DecodeString(hash)
		if err != nil || len(decoded) != sha256.Size {
			return errors.New("screened population requires outcome, inventory, spec and results SHA256 pins")
		}
	}
	return nil
}

type populationObject struct {
	Resource  schema.GroupVersionResource `json:"resource"`
	Namespace string                      `json:"namespace"`
	Name      string                      `json:"name"`
	UID       string                      `json:"uid"`
	SHA256    string                      `json:"sha256"`
}

func populationInventory(ctx context.Context, client dynamic.Interface, spec Spec) ([]populationObject, error) {
	return populationInventoryForSpec(ctx, client, spec, nil)
}

func populationInventoryForSpec(ctx context.Context, client dynamic.Interface, spec Spec, turnover *workload.TurnoverResult) ([]populationObject, error) {
	var topology map[string]*unstructured.Unstructured
	rollouts := map[string]string{}
	if turnover != nil {
		var err error
		topology, err = screenedTopology(spec)
		if err != nil {
			return nil, err
		}
		for _, intent := range turnover.Intents {
			if !intent.AcceptedAt.IsZero() {
				rollouts[intent.Namespace+"/"+intent.Deployment] = intent.RolloutID
			}
		}
	}
	objects := []populationObject{}
	for _, r := range []schema.GroupVersionResource{
		{Version: "v1", Resource: "namespaces"}, {Version: "v1", Resource: "services"}, {Version: "v1", Resource: "configmaps"}, {Version: "v1", Resource: "secrets"}, {Version: "v1", Resource: "serviceaccounts"},
		{Group: "apps", Version: "v1", Resource: "deployments"}, {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}, {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"},
		{Group: "flowcontrol.apiserver.k8s.io", Version: "v1", Resource: "flowschemas"}, {Group: "flowcontrol.apiserver.k8s.io", Version: "v1", Resource: "prioritylevelconfigurations"},
	} {
		list, err := client.Resource(r).List(ctx, metav1.ListOptions{LabelSelector: seed.RunIDLabel + "=" + spec.Seed.ID})
		if err != nil {
			return nil, err
		}
		for _, o := range list.Items {
			if turnover != nil {
				key := populationKey(r, o.GetNamespace(), o.GetName())
				want, ok := topology[key]
				if !ok || o.GetUID() == "" {
					return nil, fmt.Errorf("unexpected screened population object: %s", key)
				}
				if err := screenedObject(spec, &o, want); err != nil {
					return nil, fmt.Errorf("screened population %s: %w", key, err)
				}
				if r.Resource == "deployments" {
					marker, _, err := unstructured.NestedString(o.Object, "spec", "template", "metadata", "annotations", workload.RolloutIDAnnotation)
					recorded := rollouts[o.GetNamespace()+"/"+o.GetName()]
					if err != nil || recorded != "" && marker != recorded || recorded == "" && strings.HasPrefix(marker, turnover.RunID+"-") {
						return nil, errors.New("screened population rollout differs from finalized intents")
					}
				}
				delete(topology, key)
			}
			if o.GetDeletionTimestamp() != nil {
				return nil, errors.New("population object deleting")
			}
			if r.Resource == "deployments" {
				want := int64(5)
				if spec.Unloaded || o.GetNamespace() == spec.setup().CanaryNamespace() {
					want = 0
				}
				for _, path := range [][]string{{"spec", "replicas"}, {"status", "replicas"}, {"status", "readyReplicas"}, {"status", "updatedReplicas"}} {
					v, _, err := unstructured.NestedInt64(o.Object, path...)
					if err != nil || v != want {
						return nil, errors.New("population Deployment not converged")
					}
				}
				observed, _, _ := unstructured.NestedInt64(o.Object, "status", "observedGeneration")
				if observed != o.GetGeneration() {
					return nil, errors.New("population generation not observed")
				}
			}
			// Hash desired content, including RBAC/APF and synthetic Secret data,
			// without exporting Secret contents or volatile controller metadata.
			content := o.DeepCopy().Object
			delete(content, "status")
			delete(content, "metadata")
			content["labels"] = o.GetLabels()
			content["owners"] = o.GetOwnerReferences()
			raw, err := json.Marshal(content)
			if err != nil {
				return nil, err
			}
			objects = append(objects, populationObject{r, o.GetNamespace(), o.GetName(), string(o.GetUID()), fmt.Sprintf("%x", sha256.Sum256(raw))})
		}
	}
	counts := map[string]int{}
	if len(topology) != 0 {
		return nil, errors.New("screened population topology incomplete")
	}
	for _, o := range objects {
		counts[o.Resource.Resource]++
	}
	n := int((spec.Seed.Applications + 19) / 20)
	if counts["deployments"] != int(spec.Seed.Applications)+1 || counts["services"] != int(spec.Seed.Applications) || counts["configmaps"] != int(spec.Seed.Applications) || counts["secrets"] != int(spec.Seed.Applications) || counts["namespaces"] != n+1 || counts["flowschemas"] != n+1 || counts["prioritylevelconfigurations"] != 2 {
		return nil, errors.New("population inventory incomplete")
	}
	sort.Slice(objects, func(i, j int) bool {
		return fmt.Sprint(objects[i].Resource, objects[i].Namespace, objects[i].Name) < fmt.Sprint(objects[j].Resource, objects[j].Namespace, objects[j].Name)
	})
	return objects, nil
}

func verifyPopulation(ctx context.Context, client dynamic.Interface, spec Spec, identity startup.Identity) error {
	prior, err := populationPredecessor(spec, identity)
	if err != nil {
		return err
	}
	ref := spec.ExistingPopulation
	if ref.RecoverFailed {
		return recoverFailedPopulation(ctx, client, spec, prior, identity)
	}
	raw, err := os.ReadFile(filepath.Join(ref.Directory, "population.json"))
	if err != nil {
		return err
	}
	if fmt.Sprintf("%x", sha256.Sum256(raw)) != ref.InventorySHA256 {
		return errors.New("population receipt hash mismatch")
	}
	var expected []populationObject
	if err := json.Unmarshal(raw, &expected); err != nil {
		return err
	}
	var turnover *workload.TurnoverResult
	if ref.ScreenedPopulation != nil {
		raw, err := os.ReadFile(filepath.Join(ref.Directory, "results.json"))
		if err != nil {
			return err
		}
		if fmt.Sprintf("%x", sha256.Sum256(raw)) != ref.ScreenedPopulation.ResultsSHA256 {
			return errors.New("population receipt hash mismatch")
		}
		var results Results
		if err := json.Unmarshal(raw, &results); err != nil {
			return err
		}
		turnover = results.Turnover
	}
	actual, err := populationInventoryForSpec(ctx, client, prior, turnover)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(expected, actual) {
		return errors.New("population UID or desired content changed")
	}
	if ref.ScreenedPopulation != nil {
		pods, err := client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "pods"}).Namespace(prior.setup().CanaryNamespace()).List(ctx, metav1.ListOptions{})
		if err != nil {
			return err
		}
		if len(pods.Items) != 0 {
			return errors.New("canary pods remain")
		}
	}
	return nil
}

// Each link uses its own reviewed bindings. Return the original predecessor, not
// the normalized proof, so a later history walk cannot inherit a newer exception.
func populationPredecessor(spec Spec, identity startup.Identity) (Spec, error) {
	var prior Spec
	ref := spec.ExistingPopulation
	if ref == nil || !filepath.IsAbs(ref.Directory) || len(ref.OutcomeSHA256) != 64 || len(ref.InventorySHA256) != 64 {
		return prior, errors.New("population reference requires absolute directory and receipt hashes")
	}
	if err := ref.validateScreenedPopulation(); err != nil {
		return prior, err
	}
	read := func(name, hash string) ([]byte, error) {
		raw, err := os.ReadFile(filepath.Join(ref.Directory, name))
		if err != nil {
			return nil, err
		}
		if hash != "" && fmt.Sprintf("%x", sha256.Sum256(raw)) != hash {
			return nil, errors.New("population receipt hash mismatch")
		}
		return raw, nil
	}
	raw, err := read("outcome.json", ref.OutcomeSHA256)
	if err != nil {
		return prior, err
	}
	var outcome Outcome
	if json.Unmarshal(raw, &outcome) != nil || (outcome.State != "completed" && outcome.State != "failed") || (outcome.ExecutionStatus != "completed" && !(ref.RecoverFailed && outcome.ExecutionStatus == "failed" && outcome.CleanupStatus == "not-needed")) || outcome.Finished.IsZero() || outcome.Identity != identity || (!outcome.Workload.FullWindow && !ref.RecoverFailed && ref.ScreenedPopulation == nil) {
		return prior, errors.New("population predecessor not completed")
	}
	specHash := ""
	if proof := ref.ScreenedPopulation; proof != nil {
		if !finalizedOutcome(outcome, true) || outcome.ExecutionStatus != "completed" || outcome.State != "failed" || outcome.CleanupStatus != "not-needed" || !outcome.PopulationRetained || outcome.Phase != "recovery" || outcome.Workload.FullWindow || outcome.Workload.Status != "screened-out" {
			return prior, errors.New("screened population requires finalized completed screened-out execution")
		}
		specHash = proof.SpecSHA256
	}
	raw, err = read("spec.json", specHash)
	if err != nil {
		return prior, err
	}
	if json.Unmarshal(raw, &prior) != nil || !reflect.DeepEqual(prior.Seed, spec.Seed) || !reflect.DeepEqual(prior.ExpectedGuestNodes, spec.ExpectedGuestNodes) || !reflect.DeepEqual(prior.Admission, spec.Admission) || prior.Competitor != nil {
		return prior, errors.New("population policy changed")
	}
	if ref.ScreenedPopulation != nil && (prior.Unloaded || spec.Unloaded || spec.RestorePopulation || spec.Competitor != nil) {
		return prior, errors.New("screened population requires a loaded quiet population")
	}
	if proof := ref.ScreenedPopulation; proof != nil {
		raw, err = read("results.json", proof.ResultsSHA256)
		if err != nil {
			return prior, err
		}
		var results Results
		if err := json.Unmarshal(raw, &results); err != nil {
			return prior, err
		}
		raw, err = read("population.json", ref.InventorySHA256)
		if err != nil {
			return prior, err
		}
		var inventory []populationObject
		if err := json.Unmarshal(raw, &inventory); err != nil {
			return prior, err
		}
		if err := screenedReceipts(prior, outcome, results, inventory); err != nil {
			return prior, err
		}
	}
	if err := populationStartup(prior.Startup, spec.Startup, ref.ImageBindings); err != nil {
		return prior, err
	}
	if prior.Unloaded && !spec.Unloaded && !spec.RestorePopulation {
		return prior, errors.New("restoring application replicas requires a separate reviewed action")
	}
	if spec.RestorePopulation && !prior.Unloaded {
		return prior, errors.New("restore requires a zero-replica predecessor")
	}
	return prior, nil
}

// Recover only a finalized failed execution's recorded rollout annotations. The
// prior complete inventory remains authority for every UID and all other fields.
func recoverFailedPopulation(ctx context.Context, client dynamic.Interface, spec, failed Spec, identity startup.Identity) error {
	if failed.ExistingPopulation == nil || failed.Unloaded || failed.Competitor != nil {
		return errors.New("failed population recovery requires an existing loaded quiet population")
	}
	previous := failed.ExistingPopulation
	ancestor := failed
	accepted := map[string]string{}
	dir := spec.ExistingPopulation.Directory
	for depth := 0; ; depth++ {
		if depth >= 64 {
			return errors.New("failed population history too deep")
		}
		raw, err := os.ReadFile(filepath.Join(dir, "results.json"))
		if err != nil {
			return err
		}
		var results Results
		if err := json.Unmarshal(raw, &results); err != nil {
			return err
		}
		if results.Turnover == nil || results.Turnover.Outstanding != 0 {
			return errors.New("failed turnover not finalized")
		}
		for _, intent := range results.Turnover.Intents {
			key := intent.Namespace + "/" + intent.Deployment
			if _, ok := accepted[key]; !ok && !intent.AcceptedAt.IsZero() {
				accepted[key] = intent.RolloutID
			}
		}
		prior, err := populationPredecessor(ancestor, identity)
		if err != nil {
			return err
		}
		ancestor = prior
		if _, err := os.Stat(filepath.Join(previous.Directory, "population.json")); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return err
		}
		dir = previous.Directory
		if prior.ExistingPopulation == nil || prior.Unloaded || prior.Competitor != nil {
			return errors.New("missing complete ancestor")
		}
		previous = prior.ExistingPopulation
	}
	raw, err := os.ReadFile(filepath.Join(previous.Directory, "population.json"))
	if err != nil {
		return err
	}
	if fmt.Sprintf("%x", sha256.Sum256(raw)) != previous.InventorySHA256 {
		return errors.New("prior population hash mismatch")
	}
	var expected []populationObject
	if err := json.Unmarshal(raw, &expected); err != nil {
		return err
	}
	oldIDs := map[string]string{}
	directory := previous.Directory
	for depth := 0; directory != ""; depth++ {
		if depth >= 64 {
			return errors.New("population history too deep")
		}
		raw, err := os.ReadFile(filepath.Join(directory, "results.json"))
		if err != nil {
			return err
		}
		var r Results
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		if r.Turnover != nil {
			for _, intent := range r.Turnover.Intents {
				key := intent.Namespace + "/" + intent.Deployment
				if _, ok := oldIDs[key]; !ok && !intent.AcceptedAt.IsZero() {
					oldIDs[key] = intent.RolloutID
				}
			}
		}
		directory = ""
		if ancestor.ExistingPopulation != nil {
			directory = ancestor.ExistingPopulation.Directory
			ancestor, err = populationPredecessor(ancestor, identity)
			if err != nil {
				return err
			}
		}
	}
	actual, err := populationInventory(ctx, client, failed)
	if err != nil {
		return err
	}
	if len(actual) != len(expected) {
		return errors.New("failed recovery inventory count changed")
	}
	for i, entry := range actual {
		want := expected[i]
		if entry.Resource != want.Resource || entry.Namespace != want.Namespace || entry.Name != want.Name || entry.UID != want.UID {
			return errors.New("failed recovery identity changed")
		}
		if entry.SHA256 == want.SHA256 {
			continue
		}
		key := entry.Namespace + "/" + entry.Name
		rollout, ok := accepted[key]
		if entry.Resource.Resource != "deployments" || !ok {
			return errors.New("unrecorded population content change")
		}
		o, err := client.Resource(entry.Resource).Namespace(entry.Namespace).Get(ctx, entry.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if string(o.GetUID()) != entry.UID {
			return errors.New("deployment replaced during recovery")
		}
		annotation, _, err := unstructured.NestedString(o.Object, "spec", "template", "metadata", "annotations", workload.RolloutIDAnnotation)
		if err != nil || annotation != rollout {
			return errors.New("failed rollout marker mismatch")
		}
		if priorID := oldIDs[key]; priorID != "" {
			if err := unstructured.SetNestedField(o.Object, priorID, "spec", "template", "metadata", "annotations", workload.RolloutIDAnnotation); err != nil {
				return err
			}
		} else {
			unstructured.RemoveNestedField(o.Object, "spec", "template", "metadata", "annotations", workload.RolloutIDAnnotation)
			annotations, _, _ := unstructured.NestedStringMap(o.Object, "spec", "template", "metadata", "annotations")
			if len(annotations) == 0 {
				unstructured.RemoveNestedField(o.Object, "spec", "template", "metadata", "annotations")
			}
		}
		content := o.DeepCopy().Object
		delete(content, "status")
		delete(content, "metadata")
		content["labels"] = o.GetLabels()
		content["owners"] = o.GetOwnerReferences()
		raw, err := json.Marshal(content)
		if err != nil {
			return err
		}
		if fmt.Sprintf("%x", sha256.Sum256(raw)) != want.SHA256 {
			return errors.New("failed recovery changed more than recorded rollout marker")
		}
	}
	return nil
}

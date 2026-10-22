// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0.
package performance

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/seed"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/startup"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/workload"
)

func catalogSpec(digest string) Spec {
	s := testSpec()
	for _, name := range []string{"certified-operators-catalog", "community-operators-catalog", "redhat-operators-catalog"} {
		c := startup.Container{Name: "registry", Image: "quay.io/test/catalog@sha256:" + strings.Repeat(digest, 64), Resources: *s.Startup.Components[0].Containers[0].Resources.DeepCopy()}
		init := c
		init.Name = "extract-content"
		s.Startup.Components = append(s.Startup.Components, startup.Component{Kind: "Deployment", Name: name, Replicas: 1, Containers: []startup.Container{c}, InitContainers: []startup.Container{init}})
	}
	return s
}

func catalogBindings(old, next string) []PopulationImageBinding {
	var bindings []PopulationImageBinding
	for _, c := range catalogSpec(old).Startup.Components[4:] {
		for _, init := range []bool{false, true} {
			name := "registry"
			if init {
				name = "extract-content"
			}
			bindings = append(bindings, PopulationImageBinding{Kind: c.Kind, Component: c.Name, Container: name, Init: init, PreviousImage: c.Containers[0].Image, Image: "quay.io/test/catalog@sha256:" + strings.Repeat(next, 64)})
		}
	}
	return bindings
}

func TestPopulationImageBindings(t *testing.T) {
	for _, mode := range []string{"all", "community-only", "omitted-exact", "missing", "partial", "duplicate", "unchanged", "unknown", "kind", "container", "init", "previous", "mutable", "tag-and-digest", "previous-mutable", "unused", "kas", "release", "cpu", "env", "replicas", "runtime", "probe", "identity", "nil-empty"} {
		t.Run(mode, func(t *testing.T) {
			source, target := catalogSpec("a"), catalogSpec("b")
			bindings := catalogBindings("a", "b")
			switch mode {
			case "community-only":
				target = catalogSpec("a")
				target.Startup.Components[5] = catalogSpec("b").Startup.Components[5]
				bindings = bindings[2:4]
			case "omitted-exact":
				target, bindings = catalogSpec("a"), nil
			case "missing":
				bindings = nil
			case "partial":
				bindings = bindings[:5]
			case "duplicate":
				bindings[1] = bindings[0]
			case "unchanged":
				bindings[0].PreviousImage = bindings[0].Image
			case "unknown":
				bindings[0].Component = "unknown"
			case "kind":
				bindings[0].Kind = "StatefulSet"
			case "container":
				bindings[0].Container = "sidecar"
			case "init":
				bindings[0].Init = true
			case "previous":
				bindings[0].PreviousImage = strings.ReplaceAll(bindings[0].PreviousImage, strings.Repeat("a", 64), strings.Repeat("c", 64))
			case "mutable":
				bindings[0].Image = "quay.io/test/catalog:latest"
			case "tag-and-digest":
				bindings[0].Image = strings.Replace(bindings[0].Image, "@", ":latest@", 1)
			case "previous-mutable":
				bindings[0].PreviousImage = "quay.io/test/catalog:latest"
			case "unused":
				target.Startup.Components[4].Containers[0].Image = bindings[0].PreviousImage
			case "kas":
				bindings[0].Component, bindings[0].Container = "kube-apiserver", "kube-apiserver"
			case "release":
				target.Startup.Components[0].Containers[0].Image = bindings[0].Image
			case "cpu":
				target.Startup.Components[4].Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("200m")
			case "env":
				env := []corev1.EnvVar{{Name: "GOMAXPROCS", Value: "2"}}
				target.Startup.Components[4].Containers[0].Env = &env
			case "replicas":
				target.Startup.Components[4].Replicas++
			case "runtime":
				target.Startup.CandidateSpec = json.RawMessage(`{"fixed":false}`)
			case "probe":
				target.Startup.ProbeFailurePolicy = startup.Availability
			case "identity":
				target.Startup.ResourceID += "other"
			case "nil-empty":
				target.Startup.Components[0].InitContainers = []startup.Container{}
			}
			before, _ := json.Marshal(source)
			err := populationStartup(source.Startup, target.Startup, bindings)
			valid := mode == "all" || mode == "community-only" || mode == "omitted-exact"
			if (err == nil) != valid {
				t.Fatalf("binding validation: %v", err)
			}
			after, _ := json.Marshal(source)
			if !bytes.Equal(before, after) {
				t.Fatal("modified original startup proof")
			}
			target.ExistingPopulation = &PopulationReference{Directory: "/previous", OutcomeSHA256: strings.Repeat("a", 64), InventorySHA256: strings.Repeat("b", 64), ImageBindings: bindings}
			if valid {
				if err := target.ValidateRun(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func populationFixture(t *testing.T, spec Spec) *dynamicfake.FakeDynamicClient {
	t.Helper()
	lists := map[schema.GroupVersionResource]string{}
	var objects []runtime.Object
	for _, entry := range []struct {
		group, kind, resource string
		count                 int
	}{
		{"", "Namespace", "namespaces", 2}, {"", "Service", "services", 1}, {"", "ConfigMap", "configmaps", 1}, {"", "Secret", "secrets", 1},
		{"", "Pod", "pods", 0},
		{"", "ServiceAccount", "serviceaccounts", 0}, {"apps", "Deployment", "deployments", 2}, {"rbac.authorization.k8s.io", "Role", "roles", 0}, {"rbac.authorization.k8s.io", "RoleBinding", "rolebindings", 0},
		{"flowcontrol.apiserver.k8s.io", "FlowSchema", "flowschemas", 2}, {"flowcontrol.apiserver.k8s.io", "PriorityLevelConfiguration", "prioritylevelconfigurations", 2},
	} {
		gvr := schema.GroupVersionResource{Group: entry.group, Version: "v1", Resource: entry.resource}
		lists[gvr] = entry.kind + "List"
		for i := range entry.count {
			o := &unstructured.Unstructured{Object: map[string]any{}}
			o.SetAPIVersion(gvr.GroupVersion().String())
			o.SetKind(entry.kind)
			o.SetName(fmt.Sprintf("object-%d", i))
			o.SetNamespace("fresh-0")
			o.SetUID(types.UID(fmt.Sprintf("%s-%d", entry.kind, i)))
			o.SetLabels(map[string]string{seed.RunIDLabel: spec.Seed.ID})
			if entry.kind == "Deployment" {
				replicas := int64(5)
				if i == 1 {
					o.SetNamespace(spec.setup().CanaryNamespace())
					replicas = 0
				}
				o.SetGeneration(1)
				o.Object["spec"] = map[string]any{"replicas": replicas, "template": map[string]any{"metadata": map[string]any{}}}
				o.Object["status"] = map[string]any{"replicas": replicas, "readyReplicas": replicas, "updatedReplicas": replicas, "observedGeneration": int64(1)}
			}
			objects = append(objects, o)
		}
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists, objects...)
}

func populationReceipt(t *testing.T, dir, name string, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), raw, 0600); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func TestPopulationRebindingHistory(t *testing.T) {
	for _, mode := range []string{"completed", "failed", "failed-chain", "complete-chain", "legacy", "ancestor-image", "ancestor-cpu", "ancestor-outcome", "ancestor-node", "ancestor-admission", "uid", "content", "missing-binding"} {
		t.Run(mode, func(t *testing.T) {
			id := startup.Identity{ClusterID: "cluster", HCPUID: "hcp", NamespaceUID: "namespace"}
			source := catalogSpec("a")
			client := populationFixture(t, source)
			inventory, err := populationInventory(t.Context(), client, source)
			if err != nil {
				t.Fatal(err)
			}
			files := map[string][]byte{}
			write := func(s Spec, complete bool, marker string) *PopulationReference {
				dir := t.TempDir()
				populationReceipt(t, dir, "spec.json", s)
				out := Outcome{State: "completed", ExecutionStatus: "completed", CleanupStatus: "not-needed", Started: time.Now().Add(-time.Hour), Finished: time.Now(), Identity: id, Workload: Assessment{FullWindow: true}}
				if !complete {
					out.State, out.ExecutionStatus, out.Workload.FullWindow = "failed", "failed", false
				}
				r := &PopulationReference{Directory: dir, OutcomeSHA256: populationReceipt(t, dir, "outcome.json", out), InventorySHA256: strings.Repeat("a", 64), RecoverFailed: !complete}
				if complete {
					r.InventorySHA256 = populationReceipt(t, dir, "population.json", inventory)
				}
				result := Results{Turnover: &workload.TurnoverResult{}}
				if marker != "" {
					result.Turnover.Intents = []workload.TurnoverIntent{{Namespace: "fresh-0", Deployment: "object-0", RolloutID: marker, AcceptedAt: time.Now()}}
				}
				populationReceipt(t, dir, "results.json", result)
				for _, name := range []string{"spec.json", "outcome.json", "results.json"} {
					path := filepath.Join(dir, name)
					files[path], err = os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
				}
				return r
			}
			ref := write(source, true, "")
			if mode != "completed" && mode != "legacy" {
				middle := catalogSpec("b")
				middle.ExistingPopulation = ref
				ref.ImageBindings = catalogBindings("a", "b")
				switch mode {
				case "ancestor-image":
					ref.ImageBindings = nil
				case "ancestor-cpu":
					middle.Startup.Components[4].Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("999m")
				case "ancestor-outcome":
					ref.OutcomeSHA256 = strings.Repeat("0", 64)
				case "ancestor-node":
					middle.ExpectedGuestNodes[0].UID = "replacement"
				case "ancestor-admission":
					middle.Admission = &workload.AdmissionPolicy{}
				}
				ref = write(middle, mode == "complete-chain", "")
				if mode == "failed-chain" || mode == "complete-chain" {
					next := catalogSpec("c")
					next.ExistingPopulation = ref
					ref.ImageBindings = catalogBindings("b", "c")
					ref = write(next, false, "rollout-new")
				}
			}
			target := catalogSpec("d")
			target.ExistingPopulation = ref
			old := "b"
			if mode == "completed" {
				old = "a"
			}
			if mode == "failed-chain" || mode == "complete-chain" {
				old = "c"
			}
			ref.ImageBindings = catalogBindings(old, "d")
			if mode == "legacy" {
				target = catalogSpec("a")
				target.ExistingPopulation = ref
				ref.ImageBindings = nil
			}
			if mode == "missing-binding" {
				ref.ImageBindings = nil
			}
			gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
			o, err := client.Resource(gvr).Namespace("fresh-0").Get(t.Context(), "object-0", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if mode == "failed-chain" || mode == "complete-chain" {
				if err := unstructured.SetNestedField(o.Object, "rollout-new", "spec", "template", "metadata", "annotations", workload.RolloutIDAnnotation); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "uid" {
				o.SetUID("replacement")
			}
			if mode == "content" {
				o.Object["spec"].(map[string]any)["other"] = true
			}
			if _, err := client.Resource(gvr).Namespace("fresh-0").Update(t.Context(), o, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			client.ClearActions()
			err = verifyPopulation(t.Context(), client, target, id)
			valid := mode == "completed" || mode == "failed" || mode == "failed-chain" || mode == "complete-chain" || mode == "legacy"
			if (err == nil) != valid {
				t.Fatalf("reuse: %v", err)
			}
			for _, action := range client.Actions() {
				if action.GetVerb() != "get" && action.GetVerb() != "list" {
					t.Fatalf("reuse mutated population: %s", action.GetVerb())
				}
			}
			for path, before := range files {
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("original receipt changed: %s: %v", path, err)
				}
			}
			if valid {
				dir := t.TempDir()
				if err := publish(dir, "spec.json", target); err != nil {
					t.Fatal(err)
				}
				raw, err := os.ReadFile(filepath.Join(dir, "spec.json"))
				if err != nil {
					t.Fatal(err)
				}
				var frozen Spec
				if err := json.Unmarshal(raw, &frozen); err != nil {
					t.Fatal(err)
				}
				if err := verifyPopulation(t.Context(), client, frozen, id); err != nil {
					t.Fatalf("frozen provenance did not round trip: %v", err)
				}
			}
		})
	}
}

func TestPopulationBindingStrictDecode(t *testing.T) {
	ref := PopulationReference{Directory: "/previous", OutcomeSHA256: strings.Repeat("a", 64), InventorySHA256: strings.Repeat("b", 64), ImageBindings: catalogBindings("a", "b")}
	raw, err := json.Marshal(ref)
	if err != nil {
		t.Fatal(err)
	}
	for _, unknown := range []bool{false, true} {
		data := raw
		if unknown {
			data = bytes.ReplaceAll(raw, []byte(`"previousImage"`), []byte(`"previousTypo"`))
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		var decoded PopulationReference
		if err := decoder.Decode(&decoded); (err != nil) != unknown {
			t.Fatalf("strict binding decode: %v", err)
		}
	}
}

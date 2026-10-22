// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package startup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
)

func TestImageEnvAndIgnoredResourceDrift(t *testing.T) {
	image := "registry/image@sha256:" + strings.Repeat("a", 64)
	env := []corev1.EnvVar{{Name: "GOMEMLIMIT", Value: "128MiB"}}
	profile := Component{Scope: "ignored", Reason: "operator managed", Containers: []Container{{Name: "worker", Image: image, Env: &env}}}
	pod := corev1.PodSpec{Containers: []corev1.Container{{Name: "worker", Image: image, Env: env}}}
	if !matchesProfile(pod, profile) {
		t.Fatal("explicit absent resources did not match")
	}
	for _, change := range []func(*corev1.PodSpec){
		func(p *corev1.PodSpec) { p.Containers[0].Image = "registry/image:latest" },
		func(p *corev1.PodSpec) { p.Containers[0].Env[0].Value = "256MiB" },
		func(p *corev1.PodSpec) {
			p.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")}
		},
	} {
		changed := pod.DeepCopy()
		change(changed)
		if matchesProfile(*changed, profile) {
			t.Fatal("ignored scope hid image/env/resource drift")
		}
	}
	empty := []corev1.EnvVar{}
	profile.Containers[0].Env = &empty
	data, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	var frozen Component
	if err := json.Unmarshal(data, &frozen); err != nil {
		t.Fatal(err)
	}
	if frozen.Containers[0].Env == nil || matchesProfile(pod, frozen) {
		t.Fatal("explicit empty env lost while freezing")
	}
}

func TestDisabledDeploymentInventory(t *testing.T) {
	for _, scenario := range []string{"zero", "active-pod", "absent", "allowed-absent"} {
		t.Run(scenario, func(t *testing.T) {
			profile := Component{Kind: "Deployment", Name: "cluster-autoscaler", Scope: "ignored", Reason: "disabled autoscaling", Lifecycle: &Lifecycle{Mode: "disabled", Reason: "no node pools", AllowAbsent: scenario == "allowed-absent"}, Containers: []Container{{Name: "autoscaler"}}}
			zero := int32(0)
			deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: profile.Name, Namespace: "cp", UID: "deployment"}, Spec: appsv1.DeploymentSpec{Replicas: &zero, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "autoscaler"}}, Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "autoscaler"}}}}}}
			objects := []runtime.Object{}
			if scenario == "zero" || scenario == "active-pod" {
				objects = append(objects, deployment)
			}
			if scenario == "active-pod" {
				controller := true
				replicaSet := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: "cp", UID: "rs", OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", UID: deployment.UID, Controller: &controller}}}}
				pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "cp", UID: "pod", Labels: map[string]string{"app": "autoscaler"}, OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", UID: replicaSet.UID, Controller: &controller}}}, Spec: deployment.Spec.Template.Spec, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
				objects = append(objects, replicaSet, pod)
			}
			clients := Clients{Kubernetes: kubefake.NewClientset(objects...), Dynamic: dynamicClient(candidateObject())}
			spec := Spec{CandidateName: "cluster", CandidateSpec: json.RawMessage(`{"value":"old"}`), Components: []Component{profile}}
			sample := Sample{}
			history := healthHistory{restarts: map[string]int32{}, ooms: map[string]string{}, probeEvents: map[string]int32{}}
			if err := inspect(t.Context(), clients, spec, Identity{HCPNamespace: "cp"}, &sample, &history, false); err != nil {
				t.Fatal(err)
			}
			wantIssues := scenario == "active-pod" || scenario == "absent"
			if (len(sample.Issues) > 0) != wantIssues {
				t.Fatalf("incorrect disabled assessment: %+v", sample)
			}
		})
	}
}

func TestTrustedExecCredentialsOptIn(t *testing.T) {
	for _, scenario := range []string{"default-reject", "trusted", "relative-command", "interactive", "relative-azure-dir"} {
		t.Run(scenario, func(t *testing.T) {
			command, mode, azureDir := "/trusted/kubelogin", "Never", "/external/azure-redhat"
			if scenario == "relative-command" {
				command = "kubelogin"
			}
			if scenario == "interactive" {
				mode = "IfAvailable"
			}
			if scenario == "relative-azure-dir" {
				azureDir = "relative"
			}
			config := map[string]any{"apiVersion": "v1", "kind": "Config", "current-context": "test", "clusters": []any{map[string]any{"name": "cluster", "cluster": map[string]any{"server": "https://kubernetes.invalid"}}}, "contexts": []any{map[string]any{"name": "test", "context": map[string]any{"cluster": "cluster", "user": "user"}}}, "users": []any{map[string]any{"name": "user", "user": map[string]any{"exec": map[string]any{"apiVersion": "client.authentication.k8s.io/v1", "command": command, "args": []string{"get-token"}, "interactiveMode": mode, "env": []any{map[string]string{"name": "AZURE_CONFIG_DIR", "value": azureDir}}}}}}}
			data, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "kubeconfig")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			_, err = NewClients(path, scenario != "default-reject")
			if (err == nil) != (scenario == "trusted") {
				t.Fatalf("unexpected auth opt-in outcome: %v", err)
			}
		})
	}
}

func TestCompleteInventoryValidation(t *testing.T) {
	spec := Spec{Kubeconfig: "/external/kube", ResourceID: testResourceID, CandidateName: "cluster", CandidateSpec: json.RawMessage(`{"value":"old"}`), MaxDuration: 55 * time.Minute, PollInterval: 15 * time.Second, HealthyWindow: 5 * time.Minute}
	for _, name := range []string{"kube-apiserver", "openshift-apiserver", "packageserver", "etcd"} {
		kind := "Deployment"
		if name == "etcd" {
			kind = "StatefulSet"
		}
		spec.Components = append(spec.Components, Component{Kind: kind, Name: name, Replicas: 3, Endpoints: name, ReadyEndpoints: 3, Containers: []Container{{Name: name, Resources: testResources()}}})
	}
	spec.Components = append(spec.Components, Component{Kind: "Deployment", Name: "cluster-autoscaler", Scope: "ignored", Reason: "disabled", Lifecycle: &Lifecycle{Mode: "disabled", Reason: "no autoscaling"}, Containers: []Container{{Name: "autoscaler"}}})
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	spec.Components[4].Reason = ""
	if spec.Validate() == nil {
		t.Fatal("ignored resource validation exemption did not require a reason")
	}
}

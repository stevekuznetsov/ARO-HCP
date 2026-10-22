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

package workload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	flowcontrolv1 "k8s.io/api/flowcontrol/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/seed"
)

func setupTestSpec() SetupSpec {
	return SetupSpec{Version: "v1", ID: "run-1", NamespacePrefix: "seed-1", GuestNodes: 1, Applications: 21, Image: CanaryImage}
}

func setupTestClient(s SetupSpec) *fake.Clientset {
	client := fake.NewSimpleClientset()
	for n := int64(0); n < (s.Applications+19)/20; n++ {
		ns := fmt.Sprintf("%s-%d", s.NamespacePrefix, n)
		_ = client.Tracker().Add(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: map[string]string{seed.RunIDLabel: s.ID, "kube-burner-uuid": s.ID, "kube-burner-job": s.NamespacePrefix}}})
		_ = client.Tracker().Add(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: seed.IdentityName, Namespace: ns, Labels: map[string]string{seed.RunIDLabel: s.ID}}, AutomountServiceAccountToken: ptr.To(false)})
	}
	client.PrependReactor("create", "*", func(action ktesting.Action) (bool, runtime.Object, error) {
		m, _ := meta.Accessor(action.(ktesting.CreateAction).GetObject())
		m.SetUID(types.UID("uid-" + m.GetName()))
		m.SetResourceVersion("1")
		return false, nil, nil
	})
	return client
}

func TestConfigureProfileAndFixture(t *testing.T) {
	if setupRunIDLabel != seed.RunIDLabel || setupBenchmarkIdentity != seed.IdentityName {
		t.Fatal("setup identity contract drifted from seed")
	}
	ctx := context.Background()
	spec := setupTestSpec()
	spec.NodeSelector = map[string]string{"node-role.kubernetes.io/worker": ""}
	client := setupTestClient(spec)
	system := &flowcontrolv1.PriorityLevelConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "exempt"}, Spec: flowcontrolv1.PriorityLevelConfigurationSpec{Type: flowcontrolv1.PriorityLevelEnablementExempt}}
	if err := client.Tracker().Add(system); err != nil {
		t.Fatal(err)
	}
	result, err := Configure(ctx, client, spec)
	if err != nil || result.State != "completed" || len(result.Created) != 10 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	wantAdmission := AdmissionPolicy{
		Benchmark: AdmissionProfile{NominalConcurrencyShares: 50},
		Canary:    AdmissionProfile{NominalConcurrencyShares: 5, Queue: &QueuingConfig{QueueLengthLimit: 10}},
	}
	if !reflect.DeepEqual(result.Admission, wantAdmission) || spec.Admission != nil {
		t.Fatalf("legacy effective policy missing or input defaulted in place: %+v", result.Admission)
	}
	for _, r := range result.Created {
		if r.UID == "" || r.ResourceVersion != "1" || r.APIVersion == "" || r.Kind == "" {
			t.Fatalf("missing confirmed metadata: %+v", r)
		}
	}
	for _, action := range client.Actions() {
		if action.GetVerb() != "get" && action.GetVerb() != "create" {
			t.Fatalf("unexpected mutation: %v", action)
		}
		if action.GetSubresource() == "token" || action.GetResource().Resource == "clusterroles" || action.GetResource().Resource == "clusterrolebindings" {
			t.Fatalf("unexpected privilege: %v", action)
		}
		if get, ok := action.(ktesting.GetAction); ok && get.GetName() == "exempt" {
			t.Fatal("accessed system APF")
		}
		if create, ok := action.(ktesting.CreateAction); ok {
			m, _ := meta.Accessor(create.GetObject())
			if m.GetLabels()[setupManagedBy] != "hcp-perf" || m.GetLabels()[seed.RunIDLabel] != spec.ID {
				t.Fatalf("missing ownership: %v", m)
			}
		}
	}
	unchanged, _ := client.FlowcontrolV1().PriorityLevelConfigurations().Get(ctx, "exempt", metav1.GetOptions{})
	if !reflect.DeepEqual(system, unchanged) {
		t.Fatal("modified system APF")
	}
	for _, name := range []string{result.BenchmarkPriorityLevel, result.CanaryPriorityLevel} {
		pl, err := client.FlowcontrolV1().PriorityLevelConfigurations().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		l := pl.Spec.Limited
		if pl.Spec.Type != flowcontrolv1.PriorityLevelEnablementLimited || pl.Spec.Exempt != nil || l == nil || l.LendablePercent == nil || *l.LendablePercent != 0 || l.BorrowingLimitPercent != nil {
			t.Fatalf("bad APF: %+v", pl.Spec)
		}
		if name == result.BenchmarkPriorityLevel {
			if *l.NominalConcurrencyShares != 50 || l.LimitResponse.Type != flowcontrolv1.LimitResponseTypeReject || l.LimitResponse.Queuing != nil {
				t.Fatalf("benchmark profile: %+v", l)
			}
		} else if *l.NominalConcurrencyShares != 5 || l.LimitResponse.Type != flowcontrolv1.LimitResponseTypeQueue || !reflect.DeepEqual(l.LimitResponse.Queuing, &flowcontrolv1.QueuingConfiguration{Queues: 8, HandSize: 2, QueueLengthLimit: 10}) {
			t.Fatalf("canary profile: %+v", l)
		}
	}
	flows, _ := client.FlowcontrolV1().FlowSchemas().List(ctx, metav1.ListOptions{})
	if len(flows.Items) != 3 {
		t.Fatal("expected one flow per namespace")
	}
	for _, fs := range flows.Items {
		profile, ns, identity, precedence := result.CanaryPriorityLevel, spec.CanaryNamespace(), CanaryName, int32(200)
		if strings.Contains(fs.Name, "benchmark") {
			profile, identity, precedence = result.BenchmarkPriorityLevel, seed.IdentityName, 300
			ns = spec.NamespacePrefix + fs.Name[len(fs.Name)-2:]
		}
		if fs.Spec.PriorityLevelConfiguration.Name != profile || fs.Spec.MatchingPrecedence != precedence || fs.Spec.DistinguisherMethod.Type != flowcontrolv1.FlowDistinguisherMethodByUserType || len(fs.Spec.Rules) != 1 {
			t.Fatalf("bad flow: %+v", fs.Spec)
		}
		rule := fs.Spec.Rules[0]
		if !reflect.DeepEqual(rule.Subjects, []flowcontrolv1.Subject{{Kind: flowcontrolv1.SubjectKindServiceAccount, ServiceAccount: &flowcontrolv1.ServiceAccountSubject{Namespace: ns, Name: identity}}}) || len(rule.NonResourceRules) != 0 || len(rule.ResourceRules) != 2 {
			t.Fatalf("broad subjects: %+v", rule)
		}
		for _, rr := range rule.ResourceRules {
			if rr.ClusterScope || !reflect.DeepEqual(rr.Namespaces, []string{ns}) {
				t.Fatalf("broad resource scope: %+v", rr)
			}
			for _, values := range [][]string{rr.Resources, rr.Verbs, rr.APIGroups} {
				for _, value := range values {
					if value == "*" {
						t.Fatal("wildcard APF match")
					}
				}
			}
		}
	}
	ns := spec.CanaryNamespace()
	deployment, _ := client.AppsV1().Deployments(ns).Get(ctx, CanaryName, metav1.GetOptions{})
	d := deployment.Spec
	p := d.Template.Spec
	if *d.Replicas != 0 || *d.RevisionHistoryLimit != 1 || d.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType || d.Strategy.RollingUpdate.MaxSurge.IntVal != 1 || d.Strategy.RollingUpdate.MaxUnavailable.IntVal != 0 {
		t.Fatalf("bad rollout: %+v", d)
	}
	if !reflect.DeepEqual(d.Selector.MatchLabels, d.Template.Labels) || p.NodeName != "" || p.NodeSelector[corev1.LabelOSStable] != "linux" || len(p.NodeSelector) != 2 || p.ServiceAccountName != CanaryName || *p.AutomountServiceAccountToken || len(p.InitContainers) != 0 || len(p.Volumes) != 0 {
		t.Fatalf("bad pod: %+v", p)
	}
	c := p.Containers[0]
	if len(p.Containers) != 1 || c.Image != CanaryImage || c.ImagePullPolicy != corev1.PullIfNotPresent || len(c.Command) != 0 || len(c.Args) != 0 || c.ReadinessProbe != nil || c.LivenessProbe != nil || c.StartupProbe != nil {
		t.Fatalf("unexpected process: %+v", c)
	}
	if !*p.SecurityContext.RunAsNonRoot || p.SecurityContext.RunAsUser != nil || p.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault || c.SecurityContext.RunAsUser != nil || *c.SecurityContext.AllowPrivilegeEscalation || !*c.SecurityContext.ReadOnlyRootFilesystem || !reflect.DeepEqual(c.SecurityContext.Capabilities.Drop, []corev1.Capability{"ALL"}) {
		t.Fatal("not restricted SCC compatible")
	}
	if c.Resources.Requests.Cpu().String() != "5m" || c.Resources.Requests.Memory().String() != "8Mi" || c.Resources.Limits.Cpu().String() != "50m" || c.Resources.Limits.Memory().String() != "64Mi" {
		t.Fatalf("unexpected resources: %+v", c.Resources)
	}
	role, _ := client.RbacV1().Roles(ns).Get(ctx, CanaryName, metav1.GetOptions{})
	wantRules := []rbacv1.PolicyRule{
		{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, ResourceNames: []string{CanaryName}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{"apps"}, Resources: []string{"deployments/scale"}, ResourceNames: []string{CanaryName}, Verbs: []string{"update"}},
		{APIGroups: []string{"apps"}, Resources: []string{"replicasets"}, Verbs: []string{"list", "watch"}},
		{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"list", "watch"}},
	}
	if !reflect.DeepEqual(role.Rules, wantRules) {
		t.Fatalf("excessive canary RBAC: %+v", role.Rules)
	}
	if err := VerifyCanaryIdentity(ctx, client, spec); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../../examples/seed.json")
	if err != nil {
		t.Fatal(err)
	}
	var seedSpec seed.Spec
	if err := json.Unmarshal(data, &seedSpec); err != nil || seedSpec.Image != CanaryImage {
		t.Fatal("canary image drifted from seed pin")
	}
}

func TestConfigureValidationAndOwnership(t *testing.T) {
	for _, mutate := range []func(*SetupSpec){
		func(s *SetupSpec) { s.Version = "v2" }, func(s *SetupSpec) { s.GuestNodes = 0 }, func(s *SetupSpec) { s.GuestNodes = 101 },
		func(s *SetupSpec) { s.Applications = -1 }, func(s *SetupSpec) { s.Applications = 40001 },
		func(s *SetupSpec) { s.ID = strings.Repeat("a", 48) }, func(s *SetupSpec) { s.ID = "../bad" },
		func(s *SetupSpec) { s.NamespacePrefix = "Bad" }, func(s *SetupSpec) { s.NamespacePrefix = strings.Repeat("a", 60) },
		func(s *SetupSpec) { s.Image = "registry.k8s.io/pause:latest" },
		func(s *SetupSpec) { s.Image = strings.Replace(CanaryMirror, "7031", "0000", 1) },
		func(s *SetupSpec) { s.NodeSelector = map[string]string{"kubernetes.io/hostname": "node"} },
		func(s *SetupSpec) { s.NodeSelector = map[string]string{corev1.LabelOSStable: "windows"} },
		func(s *SetupSpec) { s.NodeSelector = map[string]string{"bad key": "node"} },
		func(s *SetupSpec) { s.ID = "run-0"; s.NamespacePrefix = "hcp-perf-canary-run" },
	} {
		spec := setupTestSpec()
		mutate(&spec)
		client := fake.NewSimpleClientset()
		if _, err := Configure(context.Background(), client, spec); err == nil || len(client.Actions()) != 0 {
			t.Fatalf("invalid spec reached client: %+v", spec)
		}
	}
	if _, err := Configure(context.Background(), nil, setupTestSpec()); err == nil {
		t.Fatal("nil client accepted")
	}
	for _, mode := range []string{"missing", "wrong-run", "wrong-uuid", "wrong-job", "terminating", "wrong-sa", "missing-sa", "automount"} {
		t.Run(mode, func(t *testing.T) {
			spec := setupTestSpec()
			client := setupTestClient(spec)
			ns := spec.NamespacePrefix + "-1"
			namespace, _ := client.CoreV1().Namespaces().Get(context.Background(), ns, metav1.GetOptions{})
			sa, _ := client.CoreV1().ServiceAccounts(ns).Get(context.Background(), seed.IdentityName, metav1.GetOptions{})
			switch mode {
			case "missing":
				_ = client.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("namespaces"), "", ns)
			case "wrong-run":
				namespace.Labels[seed.RunIDLabel] = "other"
			case "wrong-uuid":
				namespace.Labels["kube-burner-uuid"] = "other"
			case "wrong-job":
				namespace.Labels["kube-burner-job"] = "other"
			case "terminating":
				now := metav1.Now()
				namespace.DeletionTimestamp = &now
			case "wrong-sa":
				sa.Labels[seed.RunIDLabel] = "other"
			case "missing-sa":
				_ = client.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("serviceaccounts"), ns, seed.IdentityName)
			case "automount":
				sa.AutomountServiceAccountToken = ptr.To(true)
			}
			if mode != "missing" {
				_ = client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("namespaces"), namespace, "")
			}
			if mode != "missing-sa" {
				_ = client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("serviceaccounts"), sa, ns)
			}
			client.ClearActions()
			result, err := Configure(context.Background(), client, spec)
			if err == nil || len(result.Created) != 0 {
				t.Fatalf("adopted seed: %+v %v", result, err)
			}
			for _, a := range client.Actions() {
				if a.GetVerb() != "get" {
					t.Fatal("mutated before ownership preflight")
				}
			}
		})
	}
}

func TestSetupAdmissionSerializationAndValidation(t *testing.T) {
	for _, tc := range []struct {
		name, admission string
		valid           bool
	}{
		{"omitted", "", true},
		{"null", `null`, true},
		{"reject", `{"benchmark":{"nominalConcurrencyShares":25},"canary":{"nominalConcurrencyShares":5,"queue":{"queueLengthLimit":10}}}`, true},
		{"queue", `{"benchmark":{"nominalConcurrencyShares":25,"queue":{"queueLengthLimit":1}},"canary":{"nominalConcurrencyShares":5,"queue":{"queueLengthLimit":20}}}`, true},
		{"zero benchmark shares", `{"benchmark":{"nominalConcurrencyShares":0},"canary":{"nominalConcurrencyShares":5,"queue":{"queueLengthLimit":10}}}`, true},
		{"zero canary shares", `{"benchmark":{"nominalConcurrencyShares":50},"canary":{"nominalConcurrencyShares":0,"queue":{"queueLengthLimit":10}}}`, true},
		{"int32 bounds", `{"benchmark":{"nominalConcurrencyShares":2147483647},"canary":{"nominalConcurrencyShares":2147483647,"queue":{"queueLengthLimit":2147483647}}}`, true},
		{"empty policy", `{}`, false},
		{"negative benchmark shares", `{"benchmark":{"nominalConcurrencyShares":-1},"canary":{"nominalConcurrencyShares":5,"queue":{"queueLengthLimit":10}}}`, false},
		{"negative canary shares", `{"benchmark":{"nominalConcurrencyShares":50},"canary":{"nominalConcurrencyShares":-1,"queue":{"queueLengthLimit":10}}}`, false},
		{"zero total shares", `{"benchmark":{"nominalConcurrencyShares":0},"canary":{"nominalConcurrencyShares":0,"queue":{"queueLengthLimit":10}}}`, false},
		{"zero benchmark queue", `{"benchmark":{"nominalConcurrencyShares":50,"queue":{"queueLengthLimit":0}},"canary":{"nominalConcurrencyShares":5,"queue":{"queueLengthLimit":10}}}`, false},
		{"empty canary queue", `{"benchmark":{"nominalConcurrencyShares":50},"canary":{"nominalConcurrencyShares":5,"queue":{}}}`, false},
		{"negative queue", `{"benchmark":{"nominalConcurrencyShares":50},"canary":{"nominalConcurrencyShares":5,"queue":{"queueLengthLimit":-1}}}`, false},
		{"canary reject", `{"benchmark":{"nominalConcurrencyShares":50},"canary":{"nominalConcurrencyShares":5}}`, false},
		{"shares overflow", `{"benchmark":{"nominalConcurrencyShares":2147483648},"canary":{"nominalConcurrencyShares":5,"queue":{"queueLengthLimit":10}}}`, false},
		{"queue overflow", `{"benchmark":{"nominalConcurrencyShares":50},"canary":{"nominalConcurrencyShares":5,"queue":{"queueLengthLimit":2147483648}}}`, false},
		{"exempt", `{"benchmark":{"nominalConcurrencyShares":50},"canary":{"type":"Exempt","nominalConcurrencyShares":5,"queue":{"queueLengthLimit":10}}}`, false},
		{"borrowing knob", `{"benchmark":{"nominalConcurrencyShares":50,"borrowingLimitPercent":0},"canary":{"nominalConcurrencyShares":5,"queue":{"queueLengthLimit":10}}}`, false},
		{"lending knob", `{"benchmark":{"nominalConcurrencyShares":50,"lendablePercent":0},"canary":{"nominalConcurrencyShares":5,"queue":{"queueLengthLimit":10}}}`, false},
		{"unknown", `{"rbacAllow":true}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := setupTestSpec()
			data, err := json.Marshal(spec)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), `"admission"`) {
				t.Fatal("omitted policy changed legacy input serialization")
			}
			if tc.admission != "" {
				data = []byte(strings.TrimSuffix(string(data), "}") + `,"admission":` + tc.admission + "}")
			}
			decoder := json.NewDecoder(strings.NewReader(string(data)))
			decoder.DisallowUnknownFields() // Same strict decoding as the CLI.
			err = decoder.Decode(&spec)
			client := setupTestClient(spec)
			if err == nil {
				_, err = Configure(t.Context(), client, spec)
			}
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
			if !tc.valid && len(client.Actions()) != 0 {
				t.Fatal("invalid admission reached the API")
			}
		})
	}
}

func TestConfigureAdmission(t *testing.T) {
	legacy := setupObjects(setupTestSpec())
	for _, tc := range []struct {
		name                          string
		benchmarkShares, canaryShares int32
		benchmarkQueue, canaryQueue   int32
	}{
		{"explicit legacy", 50, 5, 0, 10},
		{"reject", 25, 10, 0, 20},
		{"queue", 25, 10, 1, 20},
		{"zero shares", 0, 10, 2, 20},
		{"upper bounds", math.MaxInt32, math.MaxInt32, math.MaxInt32, math.MaxInt32},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := setupTestSpec()
			spec.Admission = &AdmissionPolicy{
				Benchmark: AdmissionProfile{NominalConcurrencyShares: tc.benchmarkShares},
				Canary:    AdmissionProfile{NominalConcurrencyShares: tc.canaryShares, Queue: &QueuingConfig{QueueLengthLimit: tc.canaryQueue}},
			}
			if tc.benchmarkQueue > 0 {
				spec.Admission.Benchmark.Queue = &QueuingConfig{QueueLengthLimit: tc.benchmarkQueue}
			}
			before, _ := json.Marshal(spec)
			objects := setupObjects(spec)
			for i, object := range objects {
				if tc.name == "explicit legacy" || (i != 5 && i != 6) {
					if !reflect.DeepEqual(object, legacy[i]) {
						t.Fatalf("unexpected change to object %d: %+v", i, object)
					}
				}
			}
			client := setupTestClient(spec)
			result, err := Configure(t.Context(), client, spec)
			if err != nil || !reflect.DeepEqual(result.Admission, *spec.Admission) {
				t.Fatalf("effective policy missing: %+v err=%v", result, err)
			}
			for i, shares := range []int32{tc.benchmarkShares, tc.canaryShares} {
				name := []string{result.BenchmarkPriorityLevel, result.CanaryPriorityLevel}[i]
				queue := []int32{tc.benchmarkQueue, tc.canaryQueue}[i]
				pl, err := client.FlowcontrolV1().PriorityLevelConfigurations().Get(t.Context(), name, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				response := flowcontrolv1.LimitResponse{Type: flowcontrolv1.LimitResponseTypeReject}
				if queue > 0 {
					response = flowcontrolv1.LimitResponse{Type: flowcontrolv1.LimitResponseTypeQueue, Queuing: &flowcontrolv1.QueuingConfiguration{Queues: 8, HandSize: 2, QueueLengthLimit: queue}}
				}
				want := flowcontrolv1.PriorityLevelConfigurationSpec{Type: flowcontrolv1.PriorityLevelEnablementLimited, Limited: &flowcontrolv1.LimitedPriorityLevelConfiguration{
					NominalConcurrencyShares: ptr.To(shares), LendablePercent: ptr.To(int32(0)), LimitResponse: response,
				}}
				if !reflect.DeepEqual(pl.Spec, want) {
					t.Fatalf("unexpected PLC: %+v", pl.Spec)
				}
			}
			data, err := json.Marshal(result)
			var decoded SetupResult
			if err != nil || json.Unmarshal(data, &decoded) != nil || !reflect.DeepEqual(decoded.Admission, result.Admission) {
				t.Fatalf("effective policy did not round trip: %s, %v", data, err)
			}
			// Credentials depend on exact RBAC, not the admission treatment.
			if err := VerifyCanaryIdentity(t.Context(), client, setupTestSpec()); err != nil {
				t.Fatal(err)
			}
			if err := VerifyCanaryIdentity(t.Context(), client, spec); err != nil {
				t.Fatal(err)
			}
			after, _ := json.Marshal(spec)
			if string(before) != string(after) {
				t.Fatal("setup mutated caller configuration")
			}
			result.Admission.Canary.Queue.QueueLengthLimit++
			objects[6].(*flowcontrolv1.PriorityLevelConfiguration).Spec.Limited.LimitResponse.Queuing.QueueLengthLimit++
			if spec.Admission.Canary.Queue.QueueLengthLimit != tc.canaryQueue {
				t.Fatal("result or rendered object aliases input policy")
			}
			spec.Admission.Canary.NominalConcurrencyShares = 15
			client.ClearActions()
			if retry, err := Configure(t.Context(), client, spec); err == nil || len(retry.Created) != 0 {
				t.Fatal("changed policy adopted an existing run")
			}
			for _, action := range client.Actions() {
				if action.GetVerb() != "get" {
					t.Fatal("changed policy mutated an existing run")
				}
			}
		})
	}
}

func TestConfigureApplicationNamespaces(t *testing.T) {
	for _, applications := range []int64{0, 1, 20, 21, 40, 41} {
		t.Run(fmt.Sprint(applications), func(t *testing.T) {
			spec := setupTestSpec()
			spec.GuestNodes, spec.Applications = 3, applications
			client := setupTestClient(spec)
			result, err := Configure(t.Context(), client, spec)
			namespaces := (applications + 19) / 20
			if err != nil || result.State != "completed" || len(result.Created) != 8+int(namespaces) {
				t.Fatalf("wrong setup count: %+v %v", result, err)
			}
			if err := VerifyCanaryIdentity(t.Context(), client, spec); err != nil {
				t.Fatalf("canary fixture missing: %v", err)
			}
			flows, err := client.FlowcontrolV1().FlowSchemas().List(t.Context(), metav1.ListOptions{})
			if err != nil || len(flows.Items) != int(namespaces)+1 {
				t.Fatalf("wrong namespace flow count: %+v %v", flows, err)
			}
			for n := int64(0); n < namespaces; n++ {
				flow, err := client.FlowcontrolV1().FlowSchemas().Get(t.Context(), fmt.Sprintf("hcp-perf-benchmark-%s-%d", spec.ID, n), metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				ns, _ := TurnoverApplication(spec.NamespacePrefix, n*20)
				rule := flow.Spec.Rules[0]
				if len(rule.Subjects) != 1 || rule.Subjects[0].ServiceAccount.Namespace != ns || rule.Subjects[0].ServiceAccount.Name != seed.IdentityName {
					t.Fatalf("namespace identity drift: %+v", rule)
				}
				for _, resource := range rule.ResourceRules {
					if !reflect.DeepEqual(resource.Namespaces, []string{ns}) {
						t.Fatalf("namespace scope drift: %+v", resource)
					}
				}
			}
			if applications == 0 {
				for _, action := range client.Actions() {
					if action.GetNamespace() != "" && action.GetNamespace() != spec.CanaryNamespace() {
						t.Fatalf("empty setup accessed benchmark namespace: %v", action)
					}
					if get, ok := action.(ktesting.GetAction); ok && action.GetResource().Resource == "namespaces" && get.GetName() != spec.CanaryNamespace() {
						t.Fatalf("empty setup required seed namespace: %v", action)
					}
				}
				levels, err := client.FlowcontrolV1().PriorityLevelConfigurations().List(t.Context(), metav1.ListOptions{})
				if err != nil || len(levels.Items) != 2 {
					t.Fatalf("empty setup omitted APF: %+v %v", levels, err)
				}
				for _, level := range levels.Items {
					limited := level.Spec.Limited
					if level.Spec.Type != flowcontrolv1.PriorityLevelEnablementLimited || limited == nil || *limited.LendablePercent != 0 {
						t.Fatalf("empty setup has unprotected APF: %+v", level)
					}
					if level.Name == result.CanaryPriorityLevel && (*limited.NominalConcurrencyShares != 5 || !reflect.DeepEqual(limited.LimitResponse.Queuing, &flowcontrolv1.QueuingConfiguration{Queues: 8, HandSize: 2, QueueLengthLimit: 10})) {
						t.Fatalf("empty setup has unbounded canary APF: %+v", limited)
					}
				}
			}
		})
	}
}

func TestConfigurePauseMirror(t *testing.T) {
	spec := setupTestSpec()
	spec.Image = CanaryMirror
	client := setupTestClient(spec)
	if _, err := Configure(context.Background(), client, spec); err != nil {
		t.Fatal(err)
	}
	deployment, err := client.AppsV1().Deployments(spec.CanaryNamespace()).Get(context.Background(), CanaryName, metav1.GetOptions{})
	if err != nil || deployment.Spec.Template.Spec.Containers[0].Image != CanaryMirror {
		t.Fatalf("mirror not used: deployment=%+v err=%v", deployment, err)
	}
}

func TestConfigureCollisionsAndPartialFailures(t *testing.T) {
	spec := setupTestSpec()
	for i, object := range setupObjects(spec) {
		t.Run(fmt.Sprintf("collision-%d", i), func(t *testing.T) {
			client := setupTestClient(spec)
			if err := client.Tracker().Add(object); err != nil {
				t.Fatal(err)
			}
			result, err := Configure(context.Background(), client, spec)
			if err == nil || len(result.Created) != 0 {
				t.Fatal("adopted existing object")
			}
			for _, a := range client.Actions() {
				if a.GetVerb() != "get" {
					t.Fatal("created before full collision preflight")
				}
			}
		})
		t.Run(fmt.Sprintf("failure-%d", i), func(t *testing.T) {
			client := setupTestClient(spec)
			creates := 0
			client.PrependReactor("create", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
				creates++
				if creates == i+1 {
					return true, nil, errors.New("injected")
				}
				return false, nil, nil
			})
			result, err := Configure(context.Background(), client, spec)
			if err == nil || result.State != "failed" || len(result.Created) != i || creates != i+1 {
				t.Fatalf("bad partial result: %+v %v", result, err)
			}
			for _, a := range client.Actions() {
				if a.GetVerb() != "get" && a.GetVerb() != "create" {
					t.Fatal("rollback or mutation")
				}
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := setupTestClient(spec)
	if _, err := Configure(ctx, client, spec); !errors.Is(err, context.Canceled) || len(client.Actions()) != 0 {
		t.Fatal("canceled setup reached API")
	}
	spec.GuestNodes, spec.Applications = 3, 40000
	client = setupTestClient(spec)
	result, err := Configure(context.Background(), client, spec)
	if err != nil || len(result.Created) != 2008 {
		t.Fatalf("maximum profile failed: %v", err)
	}
	flows, _ := client.FlowcontrolV1().FlowSchemas().List(context.Background(), metav1.ListOptions{})
	for _, fs := range flows.Items {
		if len(fs.Spec.Rules[0].Subjects) != 1 {
			t.Fatal("subject limit risk")
		}
	}
}

func TestVerifyCanaryIdentityRejectsDrift(t *testing.T) {
	for _, mode := range []string{"namespace", "sa", "role", "binding", "deployment", "automount", "broad-role", "broad-binding", "deleting"} {
		t.Run(mode, func(t *testing.T) {
			spec := setupTestSpec()
			client := setupTestClient(spec)
			objects := setupObjects(spec)[:5]
			index := map[string]int{"namespace": 0, "sa": 1, "role": 2, "binding": 3, "deployment": 4}[mode]
			switch mode {
			case "automount":
				objects[1].(*corev1.ServiceAccount).AutomountServiceAccountToken = ptr.To(true)
			case "broad-role":
				objects[2].(*rbacv1.Role).Rules[0].Verbs = []string{"*"}
			case "broad-binding":
				objects[3].(*rbacv1.RoleBinding).Subjects = append(objects[3].(*rbacv1.RoleBinding).Subjects, rbacv1.Subject{Kind: "Group", Name: "system:masters"})
			case "deleting":
				m, _ := meta.Accessor(objects[0])
				now := metav1.Now()
				m.SetDeletionTimestamp(&now)
			default:
				m, _ := meta.Accessor(objects[index])
				m.SetLabels(map[string]string{seed.RunIDLabel: "other"})
			}
			for _, obj := range objects {
				if err := client.Tracker().Add(obj); err != nil {
					t.Fatal(err)
				}
			}
			if err := VerifyCanaryIdentity(context.Background(), client, spec); err == nil {
				t.Fatal("accepted drifted identity")
			}
		})
	}
}

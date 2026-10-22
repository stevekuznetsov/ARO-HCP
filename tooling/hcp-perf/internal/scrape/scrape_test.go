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

package scrape

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"sigs.k8s.io/yaml"
)

// Matches HyperShift v0.1.76's AROSwift kube-apiserver ServiceMonitor fixture,
// including targetPort, mTLS references, allowlist and cluster-ID relabeling.
const kasMonitor = `
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: kube-apiserver
  namespace: hcp-one
  uid: kas-uid
  resourceVersion: "7"
  annotations:
    hypershift.openshift.io/metrics-job: apiserver
spec:
  endpoints:
  - metricRelabelings:
    - action: keep
      regex: (apiserver_storage_objects|apiserver_request_total|apiserver_current_inflight_requests)
      sourceLabels: [__name__]
    - action: replace
      replacement: cluster-one
      targetLabel: _id
    relabelings:
    - action: replace
      replacement: cluster-one
      targetLabel: _id
    scheme: https
    targetPort: client
    tlsConfig:
      ca:
        configMap:
          key: ca.crt
          name: root-ca
      cert:
        secret:
          key: tls.crt
          name: metrics-client
      keySecret:
        key: tls.key
        name: metrics-client
      serverName: kube-apiserver
  namespaceSelector:
    matchNames: [hcp-one]
  selector:
    matchLabels:
      app: kube-apiserver
`

const serviceMonitor = `
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: kube-state-metrics-hcp
  namespace: hcp-one
spec:
  sampleLimit: 200000
  selector:
    matchLabels:
      app.kubernetes.io/name: kube-state-metrics-hcp
  namespaceSelector:
    any: true
  endpoints:
  - interval: 30s
    scrapeTimeout: 20s
    port: http-metrics
    path: /custom-metrics
    scheme: https
    bearerTokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token
    tlsConfig:
      caFile: /certs/component-ca.crt
      serverName: kube-state-metrics-hcp
    relabelings:
    - targetLabel: _id
      replacement: cluster-one
    metricRelabelings:
    - action: keep
      sourceLabels: [__name__]
      regex: kube_node_info
    - action: replace
      sourceLabels: [exported_namespace]
      targetLabel: namespace
      regex: (.+)
`

const podMonitor = `
apiVersion: monitoring.coreos.com/v1
kind: PodMonitor
metadata:
  name: maestro-agent
  namespace: hcp-one
spec:
  selector:
    matchLabels:
      app: maestro-agent
  namespaceSelector:
    matchNames: [hcp-one, outside]
  podMetricsEndpoints:
  - path: /metrics
    port: metrics
    scheme: http
`

func fixture(t *testing.T, text string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(text), &u.Object); err != nil {
		t.Fatal(err)
	}
	return u
}

func testClients(objects ...runtime.Object) (*fake.Clientset, *dynamicfake.FakeDynamicClient) {
	kube := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "hcp-one"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "outside"}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "node-exporter", Namespace: "prometheus"}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "metrics", Port: 9100, Protocol: corev1.ProtocolTCP}}}},
	)
	dynamic := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		{Group: "monitoring.coreos.com", Version: "v1", Resource: "servicemonitors"}: "ServiceMonitorList",
		{Group: "monitoring.coreos.com", Version: "v1", Resource: "podmonitors"}:     "PodMonitorList",
	}, objects...)
	return kube, dynamic
}

func testSpec() Spec {
	return Spec{TargetNamespaces: []string{"hcp-one"}, ClusterID: "management-one", PrometheusNamespace: "prometheus", NodeExporterServiceNamespace: "prometheus", NodeExporterServiceName: "node-exporter", NodeExporterPort: "metrics"}
}

func configJobs(t *testing.T, bundle Bundle) map[string]map[string]any {
	t.Helper()
	var config struct {
		Jobs []map[string]any `json:"scrape_configs"`
	}
	if err := yaml.UnmarshalStrict([]byte(bundle.Config), &map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte(bundle.Config), &config); err != nil {
		t.Fatal(err)
	}
	jobs := map[string]map[string]any{}
	for _, job := range config.Jobs {
		jobs[job["job_name"].(string)] = job
	}
	return jobs
}

func TestGenerate(t *testing.T) {
	sm, pm := fixture(t, serviceMonitor), fixture(t, podMonitor)
	before := sm.DeepCopy()
	kube, dynamic := testClients(sm, pm)
	b, err := Generate(t.Context(), kube, dynamic, testSpec())
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Gaps) != 0 {
		t.Fatalf("unexpected gaps: %+v", b.Gaps)
	}
	if !reflect.DeepEqual(before, sm) {
		t.Fatal("source monitor was mutated")
	}
	if len(b.ExpectedJobs) != 5 {
		t.Fatalf("expected 5 jobs, got %v", b.ExpectedJobs)
	}
	jobs := configJobs(t, b)
	for _, name := range []string{"cadvisor", "kubelet"} {
		job := jobs[name]
		if job["scheme"] != "https" || job["bearer_token_file"] != ServiceAccountTokenFile {
			t.Fatalf("missing kubelet HTTPS/auth: %v", job)
		}
		tls := job["tls_config"].(map[string]any)
		if tls["insecure_skip_verify"] != false || tls["ca_file"] != ServiceAccountCAFile {
			t.Fatalf("unsafe kubelet TLS: %v", tls)
		}
		if !strings.Contains(fmt.Sprint(job["relabel_configs"]), "aro_hcp_azure_com_role") {
			t.Fatal("missing worker role filter")
		}
	}
	if jobs["cadvisor"]["metrics_path"] != "/metrics/cadvisor" || jobs["kubelet"]["metrics_path"] != "/metrics" {
		t.Fatal("wrong node metric paths")
	}
	smJob := jobs["ServiceMonitor/hcp-one/kube-state-metrics-hcp/0"]
	if smJob["sample_limit"] != float64(200000) || smJob["scrape_interval"] != "5s" || smJob["scrape_timeout"] != "5000ms" {
		t.Fatalf("limits/interval: %v", smJob)
	}
	if smJob["metrics_path"] != "/custom-metrics" || smJob["bearer_token_file"] != ServiceAccountTokenFile {
		t.Fatal("endpoint settings not preserved")
	}
	if strings.Contains(fmt.Sprint(smJob["metric_relabel_configs"]), "action:keep") || len(smJob["metric_relabel_configs"].([]any)) != 1 {
		t.Fatal("metric allowlist retained or label transformation lost")
	}
	if !strings.Contains(fmt.Sprint(smJob["relabel_configs"]), "__meta_kubernetes_endpointslice_port_name") {
		t.Fatal("ServiceMonitor not selecting named EndpointSlice port")
	}
	pmJob := jobs["PodMonitor/hcp-one/maestro-agent/0"]
	if !strings.Contains(fmt.Sprint(pmJob["relabel_configs"]), "__meta_kubernetes_pod_container_port_name") {
		t.Fatal("PodMonitor not selecting container port")
	}
	if strings.Contains(b.Config, "outside") || strings.Contains(b.Config, "nodes/proxy") {
		t.Fatal("config escaped namespace scope or proxies kubelet")
	}
	for _, requirements := range b.RBAC {
		for _, rule := range requirements.Rules {
			if strings.Contains(fmt.Sprint(rule), "nodes/proxy") || strings.Contains(fmt.Sprint(rule), "secrets") {
				t.Fatalf("excessive RBAC: %v", rule)
			}
		}
	}
	for _, action := range dynamic.Actions() {
		if action.GetVerb() != "list" || action.GetNamespace() != "hcp-one" {
			t.Fatalf("unexpected monitor API action: %v", action)
		}
	}
	for _, action := range kube.Actions() {
		if action.GetVerb() != "get" || action.GetResource().Resource != "services" {
			t.Fatalf("unexpected Kubernetes API action: %v", action)
		}
	}
	second, err := Generate(t.Context(), kube, dynamic, testSpec())
	if err != nil || !reflect.DeepEqual(b, second) {
		t.Fatalf("generation is not deterministic: %v", err)
	}
}

func TestPromtoolConfig(t *testing.T) {
	promtool, err := exec.LookPath("promtool")
	if err != nil {
		t.Skip("promtool not installed")
	}
	kube, dynamic := testClients(fixture(t, serviceMonitor), fixture(t, podMonitor), fixture(t, kasMonitor))
	b, err := Generate(t.Context(), kube, dynamic, testSpec())
	if err != nil || len(b.Gaps) != 0 {
		t.Fatalf("generate: %v %+v", err, b.Gaps)
	}
	path := filepath.Join(t.TempDir(), "prometheus.yaml")
	if err := os.WriteFile(path, []byte(b.Config), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), promtool, "check", "config", "--syntax-only", path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("promtool rejected generated config: %v\n%s", err, output)
	}
}

func TestNamespaceScope(t *testing.T) {
	for _, tc := range []struct {
		name                string
		explicit            []string
		regex               string
		wantError, wantList bool
	}{
		{"regex snapshot", nil, "hcp-.*", false, true},
		{"intersection", []string{"hcp-one", "outside"}, "hcp-.*", false, false},
		{"future namespace", nil, "future-.*", true, true},
		{"anchored regex", nil, "hcp", true, true},
		{"empty scope", nil, "", true, false},
		{"invalid regex", nil, "[", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kube, dynamic := testClients(fixture(t, serviceMonitor))
			spec := testSpec()
			spec.TargetNamespaces, spec.NamespaceRegex = tc.explicit, tc.regex
			b, err := Generate(t.Context(), kube, dynamic, spec)
			if (err != nil) != tc.wantError {
				t.Fatalf("unexpected error: %v", err)
			}
			listed := false
			for _, action := range kube.Actions() {
				if action.GetResource().Resource == "namespaces" {
					listed = true
				}
			}
			if listed != tc.wantList {
				t.Fatalf("namespace list=%v, want %v", listed, tc.wantList)
			}
			for _, action := range dynamic.Actions() {
				if action.GetNamespace() != "hcp-one" {
					t.Fatalf("out-of-scope monitor list: %v", action)
				}
			}
			if strings.Contains(b.Config, "outside") {
				t.Fatal("out-of-scope SD namespace")
			}
		})
	}
}

func TestInvalidSpecAndMissingMonitors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Spec)
	}{
		{"interval", func(s *Spec) { s.ScrapeInterval = "30s" }},
		{"timeout too long", func(s *Spec) { s.ScrapeTimeout = "6s" }},
		{"negative timeout", func(s *Spec) { s.ScrapeTimeout = "-1s" }},
		{"submillisecond timeout", func(s *Spec) { s.ScrapeTimeout = "1us" }},
		{"namespace", func(s *Spec) { s.TargetNamespaces = []string{""} }},
		{"cluster ID", func(s *Spec) { s.ClusterID = "" }},
		{"prometheus namespace", func(s *Spec) { s.PrometheusNamespace = "" }},
		{"invalid TLS allowlist", func(s *Spec) { s.TLSMounts = []TLSReference{{Kind: "Secret"}} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kube, dynamic := testClients(fixture(t, serviceMonitor))
			spec := testSpec()
			tc.change(&spec)
			if _, err := Generate(t.Context(), kube, dynamic, spec); err == nil {
				t.Fatal("expected invalid spec error")
			}
		})
	}
	kube, dynamic := testClients()
	b, err := Generate(t.Context(), kube, dynamic, testSpec())
	if err == nil || len(b.Gaps) == 0 {
		t.Fatal("empty monitors must fail")
	}
	dynamic.PrependReactor("list", "servicemonitors", func(clienttesting.Action) (bool, runtime.Object, error) { return true, nil, fmt.Errorf("forbidden") })
	if _, err := Generate(t.Context(), kube, dynamic, testSpec()); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("API failure not propagated: %v", err)
	}
}

func TestPartialEndpointsAndFifteenSecondInterval(t *testing.T) {
	monitor := fixture(t, kasMonitor)
	endpoints := monitor.Object["spec"].(map[string]any)["endpoints"].([]any)
	monitor.Object["spec"].(map[string]any)["endpoints"] = append(endpoints, map[string]any{"port": "other", "basicAuth": map[string]any{}})
	kube, dynamic := testClients(monitor)
	spec := testSpec()
	spec.ScrapeInterval, spec.ScrapeTimeout = "15s", "2.5s"
	b, err := Generate(t.Context(), kube, dynamic, spec)
	if err != nil || len(b.Gaps) != 1 || len(b.ExpectedJobs) != 4 || len(b.TLSMounts) != 3 {
		t.Fatalf("partial generation: %v %+v", err, b)
	}
	job := configJobs(t, b)["ServiceMonitor/hcp-one/kube-apiserver/0"]
	if job["scrape_interval"] != "15s" || job["scrape_timeout"] != "2500ms" {
		t.Fatalf("wrong normalized durations: %v", job)
	}
	if !strings.Contains(b.Gaps[0].Message, "endpoint 1") {
		t.Fatalf("gap lacks endpoint identity: %+v", b.Gaps)
	}
}

func TestNodeExporterGaps(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Spec)
	}{
		{"unspecified", func(s *Spec) { s.NodeExporterServiceName = "" }},
		{"absent service", func(s *Spec) { s.NodeExporterServiceName = "absent" }},
		{"absent port", func(s *Spec) { s.NodeExporterPort = "absent" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kube, dynamic := testClients(fixture(t, serviceMonitor))
			spec := testSpec()
			tc.change(&spec)
			b, err := Generate(t.Context(), kube, dynamic, spec)
			if err != nil || len(b.Gaps) != 1 || b.Gaps[0].Source != "node-exporter" {
				t.Fatalf("missing exporter gap: %v %+v", err, b.Gaps)
			}
			if _, exists := configJobs(t, b)["node-exporter"]; exists {
				t.Fatal("unresolved exporter job emitted")
			}
		})
	}
}

func TestUnsupportedMonitorFields(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		value       any
		topLevel    bool
	}{
		{"incomplete secret token", "bearerTokenSecret", map[string]any{"name": "token"}, false},
		{"authorization", "authorization", map[string]any{"type": "Bearer"}, false},
		{"basic auth", "basicAuth", map[string]any{}, false},
		{"oauth", "oauth2", map[string]any{}, false},
		{"proxy", "proxyUrl", "http://proxy", false},
		{"unknown monitor setting", "scrapeClass", "custom", true},
		{"unknown endpoint setting", "unknown", true, false},
		{"bad relabel action", "relabelings", []any{map[string]any{"action": "bad"}}, false},
		{"TLS downgrade relabel", "relabelings", []any{map[string]any{"targetLabel": "__scheme__", "replacement": "http"}}, false},
		{"unsupported TLS", "tlsConfig", map[string]any{"minVersion": "bad"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			monitor := fixture(t, kasMonitor)
			if tc.topLevel {
				monitor.Object["spec"].(map[string]any)[tc.field] = tc.value
			} else {
				monitor.Object["spec"].(map[string]any)["endpoints"].([]any)[0].(map[string]any)[tc.field] = tc.value
			}
			kube, dynamic := testClients(monitor)
			b, err := Generate(t.Context(), kube, dynamic, testSpec())
			if err != nil || len(b.Gaps) != 1 {
				t.Fatalf("expected one gap: %v %+v", err, b.Gaps)
			}
			if len(b.ExpectedJobs) != 3 || len(b.TLSMounts) != 0 {
				t.Fatalf("unsupported job or its TLS mounts emitted: %v %+v", b.ExpectedJobs, b.TLSMounts)
			}
		})
	}
}

func TestSelectorSemantics(t *testing.T) {
	for _, tc := range []struct {
		name     string
		selector metav1.LabelSelector
		labels   map[string]string
		want     bool
	}{
		{"literal dot", metav1.LabelSelector{MatchLabels: map[string]string{"app": "a.b"}}, map[string]string{"app": "axb"}, false},
		{"literal match", metav1.LabelSelector{MatchLabels: map[string]string{"app": "a.b"}}, map[string]string{"app": "a.b"}, true},
		{"empty requires presence", metav1.LabelSelector{MatchLabels: map[string]string{"app": ""}}, nil, false},
		{"empty present", metav1.LabelSelector{MatchLabels: map[string]string{"app": ""}}, map[string]string{"app": ""}, true},
		{"in requires presence", metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{""}}}}, nil, false},
		{"notin missing", metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "app", Operator: metav1.LabelSelectorOpNotIn, Values: []string{""}}}}, nil, true},
		{"notin empty present", metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "app", Operator: metav1.LabelSelectorOpNotIn, Values: []string{""}}}}, map[string]string{"app": ""}, false},
		{"exists empty", metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "app", Operator: metav1.LabelSelectorOpExists}}}, map[string]string{"app": ""}, true},
		{"doesnotexist", metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "app", Operator: metav1.LabelSelectorOpDoesNotExist}}}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			labels := map[string]string{}
			for key, value := range tc.labels {
				labels["__meta_kubernetes_service_label_"+key] = value
				labels["__meta_kubernetes_service_labelpresent_"+key] = "true"
			}
			matches := true
			for _, rule := range selectorRelabelings(&tc.selector, "__meta_kubernetes_service_label_") {
				values := []string{}
				for _, label := range rule["source_labels"].([]string) {
					values = append(values, labels[label])
				}
				match := regexp.MustCompile("^(?:" + rule["regex"].(string) + ")$").MatchString(strings.Join(values, ";"))
				if (rule["action"] == "keep" && !match) || (rule["action"] == "drop" && match) {
					matches = false
					break
				}
			}
			if matches != tc.want {
				t.Fatalf("selector matched=%v want=%v", matches, tc.want)
			}
		})
	}
}

func TestGenerateCancellation(t *testing.T) {
	kube, dynamic := testClients(fixture(t, serviceMonitor))
	dynamic.PrependReactor("list", "*", func(clienttesting.Action) (bool, runtime.Object, error) { return true, nil, context.Canceled })
	if _, err := Generate(t.Context(), kube, dynamic, testSpec()); err == nil {
		t.Fatal("expected cancellation failure")
	}
}

func TestBundleJSONRoundTrip(t *testing.T) {
	kube, dynamic := testClients(fixture(t, kasMonitor))
	b, err := Generate(t.Context(), kube, dynamic, testSpec())
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Bundle
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(b.TLSMounts, decoded.TLSMounts) || b.Config != decoded.Config {
		t.Fatal("mount requirements or config lost on artifact round trip")
	}
}

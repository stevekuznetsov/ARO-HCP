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
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// TLS and port settings from HyperShift v0.1.76's etcd ServiceMonitor asset.
const etcdMonitor = `
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: etcd
  namespace: hcp-one
spec:
  endpoints:
  - port: metrics
    scheme: https
    tlsConfig:
      ca:
        configMap:
          key: ca.crt
          name: etcd-ca
      cert:
        secret:
          key: etcd-client.crt
          name: etcd-metrics-client-tls
      keySecret:
        key: etcd-client.key
        name: etcd-metrics-client-tls
      serverName: etcd-client
  selector:
    matchLabels:
      app: etcd
`

func TestActualKASAndEtcdTLSMountRequirements(t *testing.T) {
	for _, tc := range []struct{ name, monitor, server string }{
		{"kube-apiserver", kasMonitor, "kube-apiserver"},
		{"etcd", etcdMonitor, "etcd-client"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			monitor := fixture(t, tc.monitor)
			before := monitor.DeepCopy()
			kube, dynamic := testClients(monitor)
			b, err := Generate(t.Context(), kube, dynamic, testSpec())
			if err != nil || len(b.Gaps) > 0 {
				t.Fatalf("actual monitor rejected: %v %+v", err, b.Gaps)
			}
			if len(b.TLSMounts) != 3 {
				t.Fatalf("want CA/cert/key mounts: %+v", b.TLSMounts)
			}
			jobName := "ServiceMonitor/hcp-one/" + tc.name + "/0"
			job := configJobs(t, b)[jobName]
			tls := job["tls_config"].(map[string]any)
			if tls["server_name"] != tc.server || tls["insecure_skip_verify"] != false {
				t.Fatalf("TLS not preserved: %v", tls)
			}
			for i, field := range []string{"ca_file", "cert_file", "key_file"} {
				mount := b.TLSMounts[i]
				if tls[field] != mount.File || mount.File != mount.VolumeMount.MountPath+"/value" {
					t.Fatalf("file/volume mismatch: %v %+v", tls, mount)
				}
				if !mount.CopyRequired || mount.Source.Namespace != "hcp-one" || mount.DestinationNamespace != "prometheus" || mount.DestinationName == mount.Source.Name {
					t.Fatalf("cross-namespace copy not declared: %+v", mount)
				}
				if mount.Volume.Name != mount.VolumeMount.Name || !mount.VolumeMount.ReadOnly || mount.VolumeMount.SubPath != "" || !reflect.DeepEqual(mount.Jobs, []string{jobName}) {
					t.Fatalf("invalid mount: %+v", mount)
				}
				if i == 0 {
					source := mount.Volume.ConfigMap
					if source == nil || source.Name != mount.DestinationName || len(source.Items) != 1 || source.Items[0].Key != mount.Source.Key || source.Items[0].Path != "value" {
						t.Fatalf("wrong CA projection: %+v", mount)
					}
				} else {
					source := mount.Volume.Secret
					if source == nil || source.SecretName != mount.DestinationName || len(source.Items) != 1 || source.Items[0].Key != mount.Source.Key || source.Items[0].Path != "value" || *source.DefaultMode != 0440 || *source.Optional {
						t.Fatalf("wrong secret projection: %+v", mount)
					}
				}
			}
			for _, action := range kube.Actions() {
				if action.GetResource().Resource != "services" || action.GetVerb() != "get" {
					t.Fatalf("generator accessed credential data: %v", action)
				}
			}
			for _, action := range dynamic.Actions() {
				if action.GetVerb() != "list" {
					t.Fatalf("generator mutated resource: %v", action)
				}
			}
			if !reflect.DeepEqual(before, monitor) {
				t.Fatal("monitor was changed")
			}
			materializer := 0
			for _, req := range b.RBAC {
				if req.Purpose != "tls-materializer" {
					continue
				}
				materializer++
				if !reflect.DeepEqual(req.Namespaces, []string{"hcp-one"}) || len(req.Rules) != 1 || len(req.Rules[0].ResourceNames) != 1 || !reflect.DeepEqual(req.Rules[0].Verbs, []string{"get"}) {
					t.Fatalf("unscoped credential permission: %+v", req)
				}
			}
			if materializer != 3 {
				t.Fatalf("missing declared credential permissions: %d", materializer)
			}
			if tc.name == "kube-apiserver" {
				if !strings.Contains(fmt.Sprint(job["relabel_configs"]), "__meta_kubernetes_pod_container_port_name") {
					t.Fatal("KAS targetPort incorrectly translated")
				}
				source := b.Sources[len(b.Sources)-1]
				if source.UID != "kas-uid" || source.ResourceVersion != "7" || source.Annotations["hypershift.openshift.io/metrics-job"] != "apiserver" {
					t.Fatalf("source metadata lost: %+v", source)
				}
			}
		})
	}
}

func TestTLSAllowlistAndDeduplication(t *testing.T) {
	monitor := fixture(t, kasMonitor)
	second := monitor.DeepCopy()
	second.SetName("second-monitor")
	kube, dynamic := testClients(monitor, second)
	b, err := Generate(t.Context(), kube, dynamic, testSpec())
	if err != nil || len(b.Gaps) != 0 || len(b.TLSMounts) != 3 {
		t.Fatalf("deduplication: %v %+v", err, b)
	}
	for _, mount := range b.TLSMounts {
		if len(mount.Jobs) != 2 {
			t.Fatalf("missing source jobs: %+v", mount)
		}
	}
	spec := testSpec()
	for _, mount := range b.TLSMounts {
		spec.TLSMounts = append(spec.TLSMounts, mount.Source)
	}
	allowed, err := Generate(t.Context(), kube, dynamic, spec)
	if err != nil || len(allowed.Gaps) != 0 {
		t.Fatalf("allowlisted TLS failed: %v %+v", err, allowed.Gaps)
	}
	spec.TLSMounts = spec.TLSMounts[:1]
	blocked, err := Generate(t.Context(), kube, dynamic, spec)
	if err != nil || len(blocked.Gaps) != 2 || len(blocked.TLSMounts) != 0 || len(blocked.ExpectedJobs) != 3 {
		t.Fatalf("allowlist failed closed: %v %+v", err, blocked)
	}
	if !strings.Contains(blocked.Gaps[0].Message, "not allowed by tlsMounts") {
		t.Fatalf("missing policy diagnostic: %+v", blocked.Gaps)
	}
}

func TestFileBackedKASMonitor(t *testing.T) {
	monitor := fixture(t, kasMonitor)
	endpoints := monitor.Object["spec"].(map[string]any)["endpoints"].([]any)
	endpoints[0].(map[string]any)["tlsConfig"] = map[string]any{
		"caFile": "/certs/root-ca.crt", "certFile": "/certs/client.crt",
		"keyFile": "/certs/client.key", "serverName": "kube-apiserver",
	}
	kube, dynamic := testClients(monitor)
	b, err := Generate(t.Context(), kube, dynamic, testSpec())
	if err != nil || len(b.Gaps) > 0 {
		t.Fatalf("file-backed KAS failed: %v %+v", err, b.Gaps)
	}
	job := configJobs(t, b)["ServiceMonitor/hcp-one/kube-apiserver/0"]
	if !strings.Contains(fmt.Sprint(job["relabel_configs"]), "__meta_kubernetes_pod_container_port_name") {
		t.Fatal("targetPort translated as service port rather than container port")
	}
	tls := job["tls_config"].(map[string]any)
	if tls["cert_file"] != "/certs/client.crt" || tls["server_name"] != "kube-apiserver" || tls["insecure_skip_verify"] != false {
		t.Fatalf("mTLS not preserved: %v", tls)
	}
	if len(b.TLSMounts) != 0 {
		t.Fatal("file-backed TLS must not generate secret mount requirements")
	}
}

func TestTLSReferenceNamespaceAndLocalMount(t *testing.T) {
	monitor := fixture(t, kasMonitor)
	monitor.Object["spec"].(map[string]any)["namespaceSelector"] = map[string]any{"matchNames": []any{"outside"}}
	kube, dynamic := testClients(monitor)
	spec := testSpec()
	spec.TargetNamespaces = []string{"hcp-one", "outside"}
	spec.PrometheusNamespace = "hcp-one"
	b, err := Generate(t.Context(), kube, dynamic, spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, mount := range b.TLSMounts {
		if mount.Source.Namespace != "hcp-one" || mount.CopyRequired || mount.DestinationName != mount.Source.Name {
			t.Fatalf("reference resolved against target rather than monitor namespace: %+v", mount)
		}
	}
	if len(b.TLSMounts) != 3 {
		t.Fatalf("missing TLS requirements: %+v", b.TLSMounts)
	}
}

func TestTLSValidation(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		spec      Spec
		wantError bool
		mounts    int
	}{
		{"insecure rejected", `{"insecureSkipVerify":true}`, Spec{}, true, 0},
		{"insecure explicit trust", `{"insecureSkipVerify":true}`, Spec{ComponentCAFile: "/ca", ComponentServerName: "metrics"}, false, 0},
		{"CA reference overridden", `{"ca":{"configMap":{"name":"ca","key":"ca.crt"}}}`, Spec{ComponentCAFile: "/ca"}, false, 0},
		{"secret CA", `{"ca":{"secret":{"name":"ca","key":"ca.crt"}}}`, Spec{}, false, 1},
		{"incomplete cert pair", `{"certFile":"/cert"}`, Spec{}, true, 0},
		{"files", `{"caFile":"/ca","certFile":"/cert","keyFile":"/key"}`, Spec{}, false, 0},
		{"empty selector", `{"ca":{}}`, Spec{}, true, 0},
		{"ambiguous selector", `{"ca":{"secret":{"name":"ca","key":"ca"},"configMap":{"name":"ca","key":"ca"}}}`, Spec{}, true, 0},
		{"conflicting file", `{"caFile":"/ca","ca":{"secret":{"name":"ca","key":"ca"}}}`, Spec{}, true, 0},
		{"path traversal", `{"ca":{"secret":{"name":"ca","key":"../key"}}}`, Spec{}, true, 0},
		{"namespace override", `{"ca":{"secret":{"name":"ca","key":"ca","namespace":"outside"}}}`, Spec{}, true, 0},
		{"optional key", `{"ca":{"secret":{"name":"ca","key":"ca","optional":true}}}`, Spec{}, true, 0},
		{"unknown field", `{"ca":{"secret":{"name":"ca","key":"ca","unknown":true}}}`, Spec{}, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tls, mounts, err := convertTLS(json.RawMessage(tc.raw), tc.spec, "hcp-one", "job")
			if (err != nil) != tc.wantError || len(mounts) != tc.mounts {
				t.Fatalf("unexpected result: %v %v %+v", err, tls, mounts)
			}
			if err == nil && tls["insecure_skip_verify"] != false {
				t.Fatal("TLS verification not enforced")
			}
		})
	}
}

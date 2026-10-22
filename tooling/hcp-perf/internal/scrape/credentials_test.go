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
	"strings"
	"testing"
)

func TestCredentialSelectors(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		wantError bool
		mounts    int
	}{
		{"bearer", `{"scheme":"https","bearerTokenSecret":{"name":"guest-token","key":"token"}}`, false, 1},
		{"authorization", `{"scheme":"https","authorization":{"credentials":{"name":"guest-token","key":"token"}}}`, false, 1},
		{"authorization file", `{"scheme":"https","authorization":{"type":"Bearer","credentialsFile":"/token"}}`, false, 0},
		{"legacy file", `{"scheme":"https","bearerTokenFile":"/token"}`, false, 0},
		{"empty legacy selector", `{"scheme":"http","bearerTokenSecret":{"name":"","key":""}}`, false, 0},
		{"empty authorization", `{"scheme":"https","authorization":{}}`, true, 0},
		{"missing key", `{"scheme":"https","bearerTokenSecret":{"name":"guest-token"}}`, true, 0},
		{"optional", `{"scheme":"https","bearerTokenSecret":{"name":"guest-token","key":"token","optional":true}}`, true, 0},
		{"HTTP", `{"scheme":"http","bearerTokenSecret":{"name":"guest-token","key":"token"}}`, true, 0},
		{"redirect", `{"scheme":"https","bearerTokenFile":"/token","followRedirects":true}`, true, 0},
		{"conflict", `{"scheme":"https","bearerTokenFile":"/token","authorization":{"credentialsFile":"/token"}}`, true, 0},
		{"selector and file", `{"scheme":"https","authorization":{"credentials":{"name":"guest-token","key":"token"},"credentialsFile":"/token"}}`, true, 0},
		{"basic", `{"scheme":"https","authorization":{"type":"basic","credentialsFile":"/token"}}`, true, 0},
		{"traversal", `{"scheme":"https","bearerTokenSecret":{"name":"guest-token","key":"../token"}}`, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var e endpointSpec
			if err := json.Unmarshal([]byte(tc.raw), &e); err != nil {
				t.Fatal(err)
			}
			auth, mounts, _, err := convertCredentials(e, testSpec(), "hcp-one", "job")
			if (err != nil) != tc.wantError || len(mounts) != tc.mounts {
				t.Fatalf("auth=%v mounts=%v err=%v", auth, mounts, err)
			}
			if len(mounts) > 0 && (mounts[0].Source.Namespace != "hcp-one" || !mounts[0].CopyRequired || !mounts[0].VolumeMount.ReadOnly) {
				t.Fatal("incorrect credential projection")
			}
		})
	}
}

func TestExactEndpointCredentials(t *testing.T) {
	monitor := fixture(t, kasMonitor)
	second := monitor.DeepCopy()
	second.SetName("management-component")
	kube, dynamic := testClients(monitor, second)
	spec := testSpec()
	override := EndpointCredentials{MonitorKind: "ServiceMonitor", Namespace: "hcp-one", Name: "kube-apiserver", EndpointIndex: 0, BearerTokenSecret: FileReference{Kind: "Secret", Namespace: "observer", Name: "guest-token", Key: "token"}}
	spec.EndpointCredentials = []EndpointCredentials{override}
	b, err := Generate(t.Context(), kube, dynamic, spec)
	if err != nil || len(b.Gaps) != 0 {
		t.Fatalf("generate: %v %v", err, b.Gaps)
	}
	jobs := configJobs(t, b)
	kas := jobs[override.job()]
	tls := kas["tls_config"].(map[string]any)
	if tls["cert_file"] != nil || tls["key_file"] != nil || tls["ca_file"] == nil || tls["server_name"] != "kube-apiserver" || tls["insecure_skip_verify"] != false || kas["authorization"] == nil || kas["bearer_token_file"] != nil {
		t.Fatalf("incorrect override: %v", kas)
	}
	management := jobs["ServiceMonitor/hcp-one/management-component/0"]
	if management["authorization"] != nil || management["tls_config"].(map[string]any)["cert_file"] == nil {
		t.Fatal("override escaped exact endpoint")
	}
	if jobs["kubelet"]["bearer_token_file"] != ServiceAccountTokenFile {
		t.Fatal("management token changed")
	}
	for _, action := range kube.Actions() {
		if action.GetResource().Resource == "secrets" {
			t.Fatal("generator read secrets")
		}
	}
	for _, tc := range []struct {
		name    string
		modify  func(*Spec)
		invalid bool
	}{
		{"unmatched", func(s *Spec) { s.EndpointCredentials[0].EndpointIndex = 99 }, false},
		{"out of scope", func(s *Spec) { s.EndpointCredentials[0].Namespace = "other" }, false},
		{"duplicate", func(s *Spec) { s.EndpointCredentials = append(s.EndpointCredentials, override) }, true},
		{"negative", func(s *Spec) { s.EndpointCredentials[0].EndpointIndex = -1 }, true},
		{"allowlist", func(s *Spec) {
			s.TLSMounts = []TLSReference{{Kind: "Secret", Namespace: "other", Name: "other", Key: "token"}}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec.EndpointCredentials = []EndpointCredentials{override}
			spec.TLSMounts = nil
			tc.modify(&spec)
			b, err := Generate(t.Context(), kube, dynamic, spec)
			if tc.invalid && err == nil || !tc.invalid && (err != nil || len(b.Gaps) == 0) {
				t.Fatalf("failed open: %v %v", err, b.Gaps)
			}
		})
	}
}

func TestLiveEmptyCredentialDefaults(t *testing.T) {
	monitor := fixture(t, kasMonitor)
	e := monitor.Object["spec"].(map[string]any)["endpoints"].([]any)[0].(map[string]any)
	e["tlsConfig"] = map[string]any{"cert": map[string]any{}, "ca": map[string]any{"configMap": map[string]any{"name": "root-ca", "key": "ca.crt"}}}
	e["bearerTokenSecret"] = map[string]any{"name": "", "key": ""}
	kube, dynamic := testClients(monitor)
	b, err := Generate(t.Context(), kube, dynamic, testSpec())
	if err != nil || len(b.Gaps) != 0 || len(b.TLSMounts) != 1 || strings.Contains(b.Config, "cert_file") {
		t.Fatalf("empty defaults: %v %v", err, b.Gaps)
	}
}

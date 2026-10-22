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
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/startup"
)

func TestGuestIdentityBeforeMutation(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	for _, mode := range []string{"match", "wrong", "missing", "status-only", "unavailable", "invalid-expected", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			version := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "config.openshift.io/v1", "kind": "ClusterVersion", "metadata": map[string]any{"name": "version"}, "spec": map[string]any{"clusterID": id}}}
			expected := id
			switch mode {
			case "wrong":
				version.Object["spec"] = map[string]any{"clusterID": "22222222-2222-4222-8222-222222222222"}
			case "status-only":
				delete(version.Object, "spec")
				version.Object["status"] = map[string]any{"clusterID": id}
			case "invalid-expected":
				expected = "cluster-name"
			}
			client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), version)
			if mode == "missing" {
				client = dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
			}
			if mode == "unavailable" || mode == "canceled" {
				client.PrependReactor("get", "clusterversions", func(clienttesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("unavailable") })
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "canceled" {
				cancel()
			}
			err := verifyGuestIdentity(ctx, client, expected)
			if (err == nil) != (mode == "match") {
				t.Fatalf("identity gate: %v", err)
			}
			for _, action := range client.Actions() {
				if action.GetVerb() != "get" || action.GetResource() != clusterVersions {
					t.Fatal("identity gate mutated or queried unrelated resources")
				}
			}
		})
	}
}

func TestRunWrongGuestCannotReachSeedOrSetup(t *testing.T) {
	for _, mode := range []string{"wrong", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			spec := testSpec()
			const clusterID = "11111111-1111-4111-8111-111111111111"
			mutations, guestReads, exports := 0, 0, 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					mutations++
					http.Error(w, "unexpected mutation", 500)
					return
				}
				var response any
				switch r.URL.Path {
				case "/apis/hypershift.openshift.io/v1beta1/hostedclusters":
					response = map[string]any{"apiVersion": "hypershift.openshift.io/v1beta1", "kind": "HostedClusterList", "items": []any{map[string]any{"metadata": map[string]any{"name": "hc", "namespace": "clusters", "uid": "hc-uid", "annotations": map[string]string{startup.ResourceIDAnnotation: spec.Startup.ResourceID}}, "spec": map[string]any{"clusterID": clusterID}}}}
				case "/apis/hypershift.openshift.io/v1beta1/hostedcontrolplanes":
					response = map[string]any{"apiVersion": "hypershift.openshift.io/v1beta1", "kind": "HostedControlPlaneList", "items": []any{map[string]any{"metadata": map[string]any{"name": "hcp", "namespace": "control", "uid": "hcp-uid", "annotations": map[string]string{startup.HostedClusterAnnotation: "clusters/hc"}}, "spec": map[string]any{"clusterID": clusterID}}}}
				case "/api/v1/namespaces/control":
					response = map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "control", "uid": "ns-uid"}}
				case "/apis/config.openshift.io/v1/clusterversions/version":
					guestReads++
					if mode == "unavailable" {
						http.Error(w, "denied", http.StatusForbidden)
						return
					}
					response = map[string]any{"apiVersion": "config.openshift.io/v1", "kind": "ClusterVersion", "metadata": map[string]any{"name": "version"}, "spec": map[string]any{"clusterID": "22222222-2222-4222-8222-222222222222"}}
				case "/api/v1/query_range":
					exports++
					http.Error(w, "no capture", http.StatusServiceUnavailable)
					return
				default:
					t.Errorf("identity gate allowed unexpected request: %s", r.URL.Path)
					http.Error(w, "unexpected", 500)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(response)
			}))
			defer server.Close()
			config := clientcmdapi.NewConfig()
			config.Clusters["guest"] = &clientcmdapi.Cluster{Server: server.URL, CertificateAuthorityData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})}
			config.AuthInfos["admin"] = &clientcmdapi.AuthInfo{Token: "test-admin"}
			config.Contexts["guest"] = &clientcmdapi.Context{Cluster: "guest", AuthInfo: "admin"}
			config.CurrentContext = "guest"
			data, err := clientcmd.Write(*config)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "guest.kubeconfig")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			spec.Startup.Kubeconfig, spec.GuestKubeconfig, spec.Capture.Endpoint = path, path, server.URL
			dir := filepath.Join(t.TempDir(), "case")
			_, err = Run(t.Context(), spec, dir, server.Client())
			if err == nil || !strings.Contains(err.Error(), "guest ClusterVersion") || mutations != 0 || guestReads != 1 || exports != 1 {
				t.Fatalf("identity fail-closed: err=%v mutations=%d guest=%d exports=%d", err, mutations, guestReads, exports)
			}
			if _, err := os.Stat(filepath.Join(dir, "seed")); !os.IsNotExist(err) {
				t.Fatal("seed reached before identity gate")
			}
		})
	}
}

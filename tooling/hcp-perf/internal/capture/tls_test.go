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

package capture

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/scrape"
)

func tlsRequirement(kind, key string) scrape.TLSMountRequirement {
	source := scrape.TLSReference{Kind: kind, Namespace: "source", Name: "metrics-client", Key: key}
	digest := sha256.Sum256([]byte(kind + "/source/metrics-client/" + key))
	name := fmt.Sprintf("hcp-perf-tls-%x", digest[:16])
	volume := corev1.Volume{Name: name}
	items := []corev1.KeyToPath{{Key: key, Path: "value"}}
	if kind == "Secret" {
		volume.Secret = &corev1.SecretVolumeSource{SecretName: name, Items: items, DefaultMode: ptr(int32(0440)), Optional: ptr(false)}
	} else {
		volume.ConfigMap = &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Items: items, DefaultMode: ptr(int32(0440)), Optional: ptr(false)}
	}
	return scrape.TLSMountRequirement{Source: source, DestinationNamespace: "hcp-perf-test-1", DestinationName: name,
		CopyRequired: true, File: "/etc/prometheus/tls/" + name + "/value", Volume: volume,
		VolumeMount: corev1.VolumeMount{Name: name, MountPath: "/etc/prometheus/tls/" + name, ReadOnly: true}, Jobs: []string{"metrics"}}
}

func tlsClient(t *testing.T) *fake.Clientset {
	t.Helper()
	client := testClient()
	if _, err := client.CoreV1().Secrets("source").Create(t.Context(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "metrics-client"}, Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{"tls.crt": []byte("CERT_BYTES"), "tls.key": []byte("PRIVATE_KEY_BYTES"), "token": []byte("TOKEN_BYTES"), "unrelated": []byte("UNRELATED_SECRET")},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().ConfigMaps("source").Create(t.Context(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "metrics-client"}, Data: map[string]string{"ca.crt": "CA_BYTES", "unrelated": "UNRELATED_CA"},
		BinaryData: map[string][]byte{"binary.crt": []byte("BINARY_CA_BYTES")},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	client.ClearActions()
	return client
}

func TestTLSSnapshots(t *testing.T) {
	client := tlsClient(t)
	spec := testSpec()
	spec.TLSMounts = []scrape.TLSMountRequirement{tlsRequirement("Secret", "tls.crt"), tlsRequirement("Secret", "tls.key"), tlsRequirement("ConfigMap", "ca.crt"), tlsRequirement("ConfigMap", "binary.crt"), tlsRequirement("Secret", "tls.key")}
	s, err := Start(t.Context(), client, spec, testConfig)
	if err != nil || len(s.Inventory) != 10 || s.TLSCredentialPolicy != tlsCredentialPolicy {
		t.Fatalf("TLS start failed: %+v %v", s, err)
	}
	sourceGets := 0
	for i, action := range client.Actions() {
		if action.GetNamespace() == "source" {
			if action.GetVerb() != "get" || action.(clienttesting.GetAction).GetName() != "metrics-client" || i > 1 {
				t.Fatalf("source access not exact GET before creates: %+v", action)
			}
			sourceGets++
		}
		if action.GetVerb() == "list" || action.GetResource().Group == "rbac.authorization.k8s.io" {
			t.Fatalf("TLS start should not list credentials or add scraper RBAC: %+v", action)
		}
	}
	if sourceGets != 2 {
		t.Fatalf("shared sources must be read once: %d", sourceGets)
	}
	for i, want := range []string{"CERT_BYTES", "PRIVATE_KEY_BYTES", "CA_BYTES", "BINARY_CA_BYTES"} {
		mount := spec.TLSMounts[i]
		if mount.Source.Kind == "Secret" {
			object, err := client.CoreV1().Secrets(s.Namespace).Get(t.Context(), mount.DestinationName, metav1.GetOptions{})
			if err != nil || !*object.Immutable || !owned(object, s.ID) || object.Type != corev1.SecretTypeOpaque || len(object.Data) != 1 || string(object.Data[mount.Source.Key]) != want {
				t.Fatalf("incorrect Secret snapshot metadata/selected key: %v", err)
			}
		} else {
			object, err := client.CoreV1().ConfigMaps(s.Namespace).Get(t.Context(), mount.DestinationName, metav1.GetOptions{})
			if err != nil || !*object.Immutable || !owned(object, s.ID) || len(object.Data) != 0 || len(object.BinaryData) != 1 || string(object.BinaryData[mount.Source.Key]) != want {
				t.Fatalf("incorrect ConfigMap snapshot metadata/selected key: %v", err)
			}
		}
	}
	sts, err := client.AppsV1().StatefulSets(s.Namespace).Get(t.Context(), resourceName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sts.Spec.Template.Spec.Volumes) != 7 || len(sts.Spec.Template.Spec.Containers[0].VolumeMounts) != 7 {
		t.Fatal("duplicate requirements created duplicate volumes")
	}
	for i, mount := range spec.TLSMounts[:4] {
		if !reflect.DeepEqual(sts.Spec.Template.Spec.Volumes[i+3], mount.Volume) || !reflect.DeepEqual(sts.Spec.Template.Spec.Containers[0].VolumeMounts[i+3], mount.VolumeMount) {
			t.Fatal("generated projection or read-only mount changed")
		}
	}
	cm, err := client.CoreV1().ConfigMaps(s.Namespace).Get(t.Context(), resourceName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	client.ClearActions()
	observed, err := Status(t.Context(), client, s.ID)
	if err != nil || len(observed.Inventory) != 10 || observed.TLSCredentialPolicy != tlsCredentialPolicy {
		t.Fatalf("TLS snapshots missing from status: %+v %v", observed, err)
	}
	for _, action := range client.Actions() {
		if action.GetNamespace() == "source" || (action.GetResource().Resource == "secrets" && action.GetVerb() != "get") {
			t.Fatalf("Status must only GET destination credentials: %+v", action)
		}
	}
	encoded, _ := json.Marshal(observed)
	for _, sensitive := range []string{"CERT_BYTES", "PRIVATE_KEY_BYTES", "CA_BYTES", "UNRELATED_SECRET", "UNRELATED_CA"} {
		if strings.Contains(string(encoded), sensitive) || strings.Contains(cm.Data["spec.json"], sensitive) || strings.Contains(cm.Data["prometheus.yml"], sensitive) {
			t.Fatal("credential bytes exported in status or configuration")
		}
	}
	if _, err := Stop(t.Context(), client, s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().Secrets(s.Namespace).Get(t.Context(), spec.TLSMounts[1].DestinationName, metav1.GetOptions{}); err != nil {
		t.Fatal("Stop removed credential snapshot")
	}
}

func TestTLSRejectsUnsafeRequirements(t *testing.T) {
	for name, change := range map[string]func(*scrape.TLSMountRequirement){
		"foreign destination": func(m *scrape.TLSMountRequirement) { m.DestinationNamespace = "default" },
		"same namespace":      func(m *scrape.TLSMountRequirement) { m.Source.Namespace = m.DestinationNamespace },
		"no copy":             func(m *scrape.TLSMountRequirement) { m.CopyRequired = false },
		"kind":                func(m *scrape.TLSMountRequirement) { m.Source.Kind = "Pod" },
		"invalid key":         func(m *scrape.TLSMountRequirement) { m.Source.Key = "../key" },
		"invalid namespace":   func(m *scrape.TLSMountRequirement) { m.Source.Namespace = "../source" },
		"config overwrite":    func(m *scrape.TLSMountRequirement) { m.DestinationName = resourceName },
		"traversal":           func(m *scrape.TLSMountRequirement) { m.VolumeMount.MountPath += "/../../.." },
		"token overwrite": func(m *scrape.TLSMountRequirement) {
			m.VolumeMount.MountPath = "/var/run/secrets/kubernetes.io/serviceaccount"
		},
		"writable":         func(m *scrape.TLSMountRequirement) { m.VolumeMount.ReadOnly = false },
		"subpath":          func(m *scrape.TLSMountRequirement) { m.VolumeMount.SubPath = "value" },
		"host path":        func(m *scrape.TLSMountRequirement) { m.Volume.HostPath = &corev1.HostPathVolumeSource{Path: "/"} },
		"wrong projection": func(m *scrape.TLSMountRequirement) { m.Volume.Secret.Items[0].Path = "../value" },
		"extra key": func(m *scrape.TLSMountRequirement) {
			m.Volume.Secret.Items = append(m.Volume.Secret.Items, corev1.KeyToPath{Key: "other", Path: "other"})
		},
		"mode":          func(m *scrape.TLSMountRequirement) { m.Volume.Secret.DefaultMode = ptr(int32(0777)) },
		"optional":      func(m *scrape.TLSMountRequirement) { m.Volume.Secret.Optional = ptr(true) },
		"file mismatch": func(m *scrape.TLSMountRequirement) { m.File += ".bad" },
	} {
		t.Run(name, func(t *testing.T) {
			client := testClient()
			mount := tlsRequirement("Secret", "tls.key")
			change(&mount)
			spec := testSpec()
			spec.TLSMounts = []scrape.TLSMountRequirement{mount}
			if _, err := Start(t.Context(), client, spec, testConfig); err == nil || len(client.Actions()) != 0 {
				t.Fatal("unsafe requirement caused API access")
			}
		})
	}
}

func TestTLSSourceFailureCreatesNothing(t *testing.T) {
	for _, failure := range []string{"missing object", "missing key", "empty key", "oversize", "forbidden", "canceled"} {
		t.Run(failure, func(t *testing.T) {
			client := tlsClient(t)
			client.PrependReactor("get", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
				switch failure {
				case "missing object":
					return true, nil, &apierrors.StatusError{ErrStatus: metav1.Status{Reason: metav1.StatusReasonNotFound}}
				case "forbidden":
					return true, nil, &apierrors.StatusError{ErrStatus: metav1.Status{Reason: "PRIVATE_KEY_BYTES", Code: 403, Message: "PRIVATE_KEY_BYTES"}}
				case "canceled":
					return true, nil, fmt.Errorf("PRIVATE_KEY_BYTES: %w", context.Canceled)
				case "oversize":
					return true, &corev1.Secret{Data: map[string][]byte{"tls.key": make([]byte, 1024*1024)}}, nil
				case "empty key":
					return true, &corev1.Secret{Data: map[string][]byte{"tls.key": {}}}, nil
				default:
					return true, &corev1.Secret{}, nil
				}
			})
			spec := testSpec()
			spec.TLSMounts = []scrape.TLSMountRequirement{tlsRequirement("Secret", "tls.key")}
			s, err := Start(t.Context(), client, spec, testConfig)
			if err == nil || strings.Contains(err.Error(), "PRIVATE_KEY_BYTES") || len(s.Inventory) != 0 {
				t.Fatalf("source failure leaked or created inventory: %v", err)
			}
			if failure == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatal("source cancellation lost")
			}
			for _, action := range client.Actions() {
				if action.GetVerb() != "get" {
					t.Fatal("source failure created resources")
				}
			}
		})
	}
}

func TestTLSPartialCreationAndMissingInventory(t *testing.T) {
	client := tlsClient(t)
	client.PrependReactor("create", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("PRIVATE_KEY_BYTES")
	})
	spec := testSpec()
	spec.TLSMounts = []scrape.TLSMountRequirement{tlsRequirement("ConfigMap", "ca.crt"), tlsRequirement("Secret", "tls.key")}
	s, err := Start(t.Context(), client, spec, testConfig)
	if err == nil || strings.Contains(err.Error(), "PRIVATE_KEY_BYTES") || len(s.Inventory) != 4 {
		t.Fatalf("snapshot failure inventory incorrect: %+v %v", s, err)
	}
	observed, err := Status(t.Context(), client, s.ID)
	if err != nil || observed.Phase != PhaseFailed || len(observed.Inventory) != 4 {
		t.Fatalf("partial snapshot status incorrect: %+v %v", observed, err)
	}
	if !strings.Contains(strings.Join(observed.Missing, " "), "Secret/"+spec.TLSMounts[1].DestinationName) {
		t.Fatal("missing TLS snapshot not inventoried by generated name")
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "delete" || action.GetVerb() == "update" || action.GetVerb() == "patch" {
			t.Fatal("partial start cleaned up or overwrote evidence")
		}
	}
}

func TestGeneratedBundleTLSMounts(t *testing.T) {
	client := tlsClient(t)
	monitor := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "monitoring.coreos.com/v1", "kind": "ServiceMonitor",
		"metadata": map[string]any{"name": "metrics", "namespace": "source"},
		"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app": "metrics"}},
			"endpoints": []any{map[string]any{"port": "metrics", "scheme": "https", "tlsConfig": map[string]any{
				"ca":        map[string]any{"configMap": map[string]any{"name": "metrics-client", "key": "ca.crt"}},
				"cert":      map[string]any{"secret": map[string]any{"name": "metrics-client", "key": "tls.crt"}},
				"keySecret": map[string]any{"name": "metrics-client", "key": "tls.key"},
			}}},
		},
	}}
	dynamic := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		{Group: "monitoring.coreos.com", Version: "v1", Resource: "servicemonitors"}: "ServiceMonitorList",
		{Group: "monitoring.coreos.com", Version: "v1", Resource: "podmonitors"}:     "PodMonitorList",
	}, monitor)
	bundle, err := scrape.Generate(t.Context(), client, dynamic, scrape.Spec{
		TargetNamespaces: []string{"source"}, ClusterID: "test", PrometheusNamespace: "hcp-perf-test-1", PrometheusServiceAccount: resourceName,
	})
	if err != nil || len(bundle.TLSMounts) != 3 {
		t.Fatalf("generator did not produce TLS requirements: %v %+v", err, bundle.Gaps)
	}
	spec := testSpec()
	spec.KubernetesDiscovery, spec.TLSMounts = true, bundle.TLSMounts
	s, err := Start(t.Context(), client, spec, bundle.Config)
	if err != nil || len(s.Inventory) != 11 {
		t.Fatalf("generated bundle not accepted: %+v %v", s, err)
	}
	sts, err := client.AppsV1().StatefulSets(s.Namespace).Get(t.Context(), resourceName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i, requirement := range bundle.TLSMounts {
		if !strings.Contains(bundle.Config, requirement.File) || !reflect.DeepEqual(sts.Spec.Template.Spec.Containers[0].VolumeMounts[3+i], requirement.VolumeMount) {
			t.Fatal("bundle credential path is not mounted verbatim")
		}
	}
}

func TestGeneratedBearerSnapshot(t *testing.T) {
	for _, authorization := range []bool{false, true} {
		client := tlsClient(t)
		endpoint := map[string]any{"port": "metrics", "scheme": "https"}
		selector := map[string]any{"name": "metrics-client", "key": "token"}
		if authorization {
			endpoint["authorization"] = map[string]any{"credentials": selector}
		} else {
			endpoint["bearerTokenSecret"] = selector
		}
		monitor := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "monitoring.coreos.com/v1", "kind": "ServiceMonitor",
			"metadata": map[string]any{"name": "metrics", "namespace": "source"},
			"spec":     map[string]any{"selector": map[string]any{}, "endpoints": []any{endpoint}},
		}}
		dynamic := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
			{Group: "monitoring.coreos.com", Version: "v1", Resource: "servicemonitors"}: "ServiceMonitorList",
			{Group: "monitoring.coreos.com", Version: "v1", Resource: "podmonitors"}:     "PodMonitorList",
		}, monitor)
		bundle, err := scrape.Generate(t.Context(), client, dynamic, scrape.Spec{TargetNamespaces: []string{"source"}, ClusterID: "test", PrometheusNamespace: "hcp-perf-test-1"})
		if err != nil || len(bundle.TLSMounts) != 1 {
			t.Fatalf("generate: %v", err)
		}
		spec := testSpec()
		spec.TLSMounts = bundle.TLSMounts
		status, err := Start(t.Context(), client, spec, bundle.Config)
		if err != nil {
			t.Fatal(err)
		}
		mount := bundle.TLSMounts[0]
		snapshot, err := client.CoreV1().Secrets(status.Namespace).Get(t.Context(), mount.DestinationName, metav1.GetOptions{})
		if err != nil || len(snapshot.Data) != 1 || string(snapshot.Data["token"]) != "TOKEN_BYTES" || !*snapshot.Immutable {
			t.Fatal("token snapshot did not preserve only selected key")
		}
		sts, err := client.AppsV1().StatefulSets(status.Namespace).Get(t.Context(), resourceName, metav1.GetOptions{})
		if err != nil || !reflect.DeepEqual(sts.Spec.Template.Spec.Containers[0].VolumeMounts[3], mount.VolumeMount) || !strings.Contains(bundle.Config, mount.File) {
			t.Fatal("config and token mount disagree")
		}
		cm, err := client.CoreV1().ConfigMaps(status.Namespace).Get(t.Context(), resourceName, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal([]any{bundle, status, cm})
		for _, value := range []string{"TOKEN_BYTES", "UNRELATED_SECRET", "PRIVATE_KEY_BYTES"} {
			if strings.Contains(string(encoded), value) {
				t.Fatal("credential leaked into artifact/config")
			}
		}
	}
}

func TestTLSSnapshotRotationAndOwnership(t *testing.T) {
	client := tlsClient(t)
	spec := testSpec()
	spec.TLSMounts = []scrape.TLSMountRequirement{tlsRequirement("Secret", "tls.key")}
	s, err := Start(t.Context(), client, spec, testConfig)
	if err != nil {
		t.Fatal(err)
	}
	source, err := client.CoreV1().Secrets("source").Get(t.Context(), "metrics-client", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	source.Data["tls.key"] = []byte("ROTATED_PRIVATE_KEY")
	if _, err := client.CoreV1().Secrets("source").Update(t.Context(), source, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Status(t.Context(), client, s.ID); err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.CoreV1().Secrets(s.Namespace).Get(t.Context(), spec.TLSMounts[0].DestinationName, metav1.GetOptions{})
	if err != nil || string(snapshot.Data["tls.key"]) != "PRIVATE_KEY_BYTES" {
		t.Fatal("snapshot silently refreshed from source")
	}
	snapshot.Labels = labels("other")
	if _, err := client.CoreV1().Secrets(s.Namespace).Update(t.Context(), snapshot, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Status(t.Context(), client, s.ID); err == nil {
		t.Fatal("Status accepted unowned destination Secret")
	}
}

func TestTLSDestinationCollisionNeverOverwritten(t *testing.T) {
	client := tlsClient(t)
	spec := testSpec()
	mount := tlsRequirement("Secret", "tls.key")
	spec.TLSMounts = []scrape.TLSMountRequirement{mount}
	// Simulate a concurrent create after namespace creation, before snapshot copy.
	client.PrependReactor("create", "secrets", func(action clienttesting.Action) (bool, runtime.Object, error) {
		existing := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: mount.DestinationName, Namespace: mount.DestinationNamespace}, Data: map[string][]byte{"tls.key": []byte("OTHER_KEY")}}
		if err := client.Tracker().Create(action.GetResource(), existing, mount.DestinationNamespace); err != nil {
			return true, nil, err
		}
		return false, nil, nil
	})
	if _, err := Start(t.Context(), client, spec, testConfig); err == nil {
		t.Fatal("snapshot collision accepted")
	}
	existing, err := client.CoreV1().Secrets(mount.DestinationNamespace).Get(t.Context(), mount.DestinationName, metav1.GetOptions{})
	if err != nil || string(existing.Data["tls.key"]) != "OTHER_KEY" {
		t.Fatal("colliding Secret overwritten")
	}
}

func TestTLSLimitsAndConfigMapErrors(t *testing.T) {
	client := testClient()
	spec := testSpec()
	for range 65 {
		spec.TLSMounts = append(spec.TLSMounts, tlsRequirement("Secret", "tls.key"))
	}
	if _, err := Start(t.Context(), client, spec, testConfig); err == nil || len(client.Actions()) != 0 {
		t.Fatal("excessive requirements must fail before API reads")
	}
	spec.TLSMounts = nil
	data := map[string][]byte{}
	for i := range 18 {
		key := fmt.Sprintf("key-%d", i)
		spec.TLSMounts = append(spec.TLSMounts, tlsRequirement("Secret", key))
		data[key] = make([]byte, 1024*1024-256)
	}
	client.PrependReactor("get", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, &corev1.Secret{Data: data}, nil
	})
	if _, err := Start(t.Context(), client, spec, testConfig); err == nil || len(client.Actions()) != 1 {
		t.Fatal("aggregate snapshot bound did not reject before creation")
	}
	for _, ambiguous := range []bool{false, true} {
		client := testClient()
		client.PrependReactor("get", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
			if !ambiguous {
				return true, nil, errors.New("PRIVATE_KEY_BYTES")
			}
			return true, &corev1.ConfigMap{Data: map[string]string{"ca.crt": "CA"}, BinaryData: map[string][]byte{"ca.crt": []byte("OTHER_CA")}}, nil
		})
		spec := testSpec()
		spec.TLSMounts = []scrape.TLSMountRequirement{tlsRequirement("ConfigMap", "ca.crt")}
		if _, err := Start(t.Context(), client, spec, testConfig); err == nil || strings.Contains(err.Error(), "PRIVATE_KEY_BYTES") || len(client.Actions()) != 1 {
			t.Fatalf("ConfigMap error or ambiguous data must fail before creation: %v", err)
		}
	}
}

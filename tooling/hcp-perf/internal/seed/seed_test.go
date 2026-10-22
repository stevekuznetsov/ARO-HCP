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

package seed

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"text/template"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/workload"
)

func example(t *testing.T) Spec {
	t.Helper()
	data, err := os.ReadFile("../../examples/seed.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	return spec
}

func TestExplicitSeedRate(t *testing.T) {
	s := example(t)
	s.QPS, s.Burst = 2, 2
	s.GuestNodes, s.Applications = 3, 60
	s.TimeoutSeconds = 900
	b, err := Render(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"qps: 2\n", "burst: 2\n", "requestTimeout: 30s", "timeout: 900s"} {
		if !strings.Contains(b.Config, want) {
			t.Fatalf("missing %s", want)
		}
	}
	s.Burst = 0
	if s.Validate() == nil {
		t.Fatal("accepted half-configured pacing")
	}
}

func TestRender(t *testing.T) {
	for _, tc := range []struct{ nodes, apps, namespaces int64 }{
		{0, 0, 0}, {1, 40, 2}, {1000, 40000, 2000},
		{3, 0, 0}, {3, 12, 1}, {3, 120, 6}, {3, 21, 2},
		{1, 40000, 2000}, {1000, 1, 1},
	} {
		t.Run(fmt.Sprintf("nodes%d/apps%d", tc.nodes, tc.apps), func(t *testing.T) {
			spec := example(t)
			spec.GuestNodes = tc.nodes
			spec.Applications = tc.apps
			b, err := Render(spec)
			if err != nil {
				t.Fatal(err)
			}
			again, err := Render(spec)
			if err != nil || !reflect.DeepEqual(b, again) {
				t.Fatal("render is not deterministic", err)
			}
			if b.Applications != tc.apps || b.Namespaces != tc.namespaces || b.Pods != tc.apps*5 {
				t.Fatalf("incorrect counts: %+v", b)
			}
			if b.Warning != noPrePullWarning || !strings.Contains(b.Config, "No image pre-pull or automatic cluster cleanup") || !strings.Contains(b.Config, "warmup outside the scored window") {
				t.Fatal("missing no-pre-pull warning", b.Warning)
			}
			var config map[string]any
			if err := yaml.UnmarshalStrict([]byte(b.Config), &config); err != nil {
				t.Fatal(err)
			}
			global := config["global"].(map[string]any)
			if global["gc"] != false || global["waitWhenFinished"] != false || global["timeout"] != "3600s" {
				t.Fatal(global)
			}
			jobs := config["jobs"].([]any)
			if tc.apps == 0 {
				if len(jobs) != 0 || len(b.Templates) != 0 {
					t.Fatal("empty seed must have no jobs or templates")
				}
				return
			}
			if len(jobs) != 1 || len(b.Templates) != 4 {
				t.Fatal("expected one job and four templates")
			}
			job := jobs[0].(map[string]any)
			for key, want := range map[string]any{"name": spec.NamespacePrefix, "namespace": spec.NamespacePrefix, "jobType": "create", "jobIterations": float64(tc.apps), "iterationsPerNamespace": float64(20), "namespacedIterations": true, "preLoadImages": false, "verifyObjects": true, "errorOnVerify": true, "waitWhenFinished": true, "podWait": false, "cleanup": false, "gc": false, "churn": false, "maxWaitTimeout": "3600s"} {
				if job[key] != want {
					t.Errorf("%s: got %v want %v", key, job[key], want)
				}
			}
			for _, key := range []string{"preLoadPeriod", "beforeCleanup"} {
				if _, ok := job[key]; ok {
					t.Errorf("unexpected preload/cleanup option %s", key)
				}
			}
			for _, object := range job["objects"].([]any) {
				o := object.(map[string]any)
				if o["replicas"] != float64(1) || o["wait"] != true || o["runOnce"] != false || b.Templates[o["objectTemplate"].(string)] == "" {
					t.Fatal(o)
				}
			}
			for _, index := range []int64{0, 19, 20, 39, tc.apps - 1} {
				if index >= tc.apps {
					continue
				}
				namespace, name := workload.TurnoverApplication(spec.NamespacePrefix, index)
				if namespace != fmt.Sprintf("%s-%d", job["namespace"], index/int64(job["iterationsPerNamespace"].(float64))) {
					t.Fatal("namespace differs from turnover contract")
				}
				var deployment appsv1.Deployment
				var service corev1.Service
				for filename, text := range b.Templates {
					tmpl, err := template.New(filename).Option("missingkey=error").Parse(text)
					if err != nil {
						t.Fatal(err)
					}
					var rendered bytes.Buffer
					if err := tmpl.Execute(&rendered, map[string]any{"Iteration": int(index), "Replica": 1}); err != nil {
						t.Fatal(err)
					}
					var obj unstructured.Unstructured
					if err := yaml.UnmarshalStrict(rendered.Bytes(), &obj.Object); err != nil {
						t.Fatal(err)
					}
					if obj.GetName() != name || obj.GetNamespace() != "" || obj.GetLabels()[RunIDLabel] != spec.ID || obj.GetLabels()[ApplicationLabel] != fmt.Sprint(index) {
						t.Fatalf("bad metadata: %v", obj.Object)
					}
					switch filename {
					case "deployment.yaml":
						if err := yaml.UnmarshalStrict(rendered.Bytes(), &deployment); err != nil {
							t.Fatal(err)
						}
					case "service.yaml":
						if err := yaml.UnmarshalStrict(rendered.Bytes(), &service); err != nil {
							t.Fatal(err)
						}
					case "configmap.yaml", "secret.yaml":
						if rendered.Len() > 512 {
							t.Fatal("synthetic payload too large")
						}
					}
				}
				if *deployment.Spec.Replicas != 5 || deployment.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType {
					t.Fatal("bad deployment replicas/strategy")
				}
				if !reflect.DeepEqual(service.Spec.Selector, deployment.Spec.Selector.MatchLabels) || !reflect.DeepEqual(service.Spec.Selector, deployment.Spec.Template.Labels) {
					t.Fatal("selectors do not match")
				}
				container := deployment.Spec.Template.Spec.Containers[0]
				if container.Image != spec.Image || container.Resources.Requests.Cpu().String() != "10m" || container.Resources.Requests.Memory().String() != "16Mi" {
					t.Fatal(container)
				}
				if len(container.Command) != 0 || len(container.Env) != 0 || *deployment.Spec.Template.Spec.AutomountServiceAccountToken {
					t.Fatal("unexpected payload/credentials")
				}
			}
		})
	}
}

func TestValidate(t *testing.T) {
	for name, change := range map[string]func(*Spec){
		"version":            func(s *Spec) { s.Version = "v2" },
		"negative nodes":     func(s *Spec) { s.GuestNodes = -1 },
		"excess nodes":       func(s *Spec) { s.GuestNodes = 1001 },
		"negative apps":      func(s *Spec) { s.Applications = -1 },
		"excess apps":        func(s *Spec) { s.Applications = 40001 },
		"apps overflow":      func(s *Spec) { s.Applications = 1<<63 - 1 },
		"apps without nodes": func(s *Spec) { s.GuestNodes = 0; s.Applications = 1 },
		"id path":            func(s *Spec) { s.ID = "../secret" },
		"id template":        func(s *Spec) { s.ID = "{{ .HOME }}" },
		"prefix":             func(s *Spec) { s.NamespacePrefix = "Bad.Name" },
		"prefix length":      func(s *Spec) { s.NamespacePrefix = strings.Repeat("a", 59) },
		"mutable image":      func(s *Spec) { s.Image = "image:latest" },
		"image injection":    func(s *Spec) { s.Image = "{{ .HOME }}@sha256:" + strings.Repeat("a", 64) },
		"cpu absent":         func(s *Spec) { s.PerPodCPU = "" },
		"cpu negative":       func(s *Spec) { s.PerPodCPU = "-1m" },
		"memory zero":        func(s *Spec) { s.PerPodMemory = "0Mi" },
		"memory injection":   func(s *Spec) { s.PerPodMemory = "16Mi\njobs: []" },
		"timeout zero":       func(s *Spec) { s.TimeoutSeconds = 0 },
		"timeout overflow":   func(s *Spec) { s.TimeoutSeconds = 1 << 62 },
		"binary absent":      func(s *Spec) { s.Binary.Path = "" },
		"binary nul":         func(s *Spec) { s.Binary.Path = "bin\x00" },
		"hash absent":        func(s *Spec) { s.Binary.ExpectedSHA256 = "" },
		"hash short":         func(s *Spec) { s.Binary.ExpectedSHA256 = "abc" },
	} {
		t.Run(name, func(t *testing.T) {
			s := example(t)
			change(&s)
			if _, err := Render(s); err == nil {
				t.Fatal("invalid spec accepted")
			}
		})
	}
}

func TestWrite(t *testing.T) {
	spec := example(t)
	spec.Applications = 40
	spec.Binary.Path = "/does/not/exist"
	dir := filepath.Join(t.TempDir(), "render")
	if _, err := Write(spec, dir); err != nil {
		t.Fatal(err)
	}
	checkPrivate(t, dir)
	if _, err := Write(spec, dir); err == nil {
		t.Fatal("existing output accepted")
	}
	if _, err := Write(spec, filepath.Join(t.TempDir(), "absent", "output")); err == nil {
		t.Fatal("missing parent accepted")
	}
}

func checkPrivate(t *testing.T, dir string) {
	t.Helper()
	if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		want := os.FileMode(0600)
		if entry.IsDir() {
			want = 0700
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s permissions %o, want %o", path, info.Mode().Perm(), want)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

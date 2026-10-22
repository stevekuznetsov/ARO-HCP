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

// Package seed renders and executes the creation-only guest application seed.
// It does not install kube-burner, configure APF, mint credentials, or clean up
// seeded applications. It performs no image pre-pull; image pulls and warmup
// must finish outside the scored window. Callers must use a fresh namespace
// prefix and exclude concurrent seeders.
package seed

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	Version = "v1"
	// KubeBurnerVersion identifies the upstream config/CLI contract, inspected at
	// commit 917540ff45a89386bb25de45af9b96c9fc360e93. The executable's actual bytes
	// are pinned separately: release archive checksums are NOT executable hashes.
	KubeBurnerVersion = "v1.17.3"
	RunIDLabel        = "hcp-perf.azure.com/run-id"
	ApplicationLabel  = "hcp-perf.azure.com/app-index"
	IdentityName      = "benchmark"
	noPrePullWarning  = "No image pre-pull is performed; complete image pulls and warmup outside the scored window. No automatic cluster cleanup is performed."
)

var (
	pinnedImage = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]*@sha256:[a-f0-9]{64}$`)
	sha256Hex   = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// Binary is trusted operator configuration, not an untrusted workload input.
// ExpectedSHA256 must be the hash of the extracted kube-burner executable.
// Path is explicit (never searched on PATH); the parent CLI may override it.
// examples/seed.json pins the v1.17.3 Linux x86-64 release executable, not its
// tar.gz archive. Other architectures/builds need their own reviewed hash.
type Binary struct {
	Path           string `json:"path"`
	ExpectedSHA256 string `json:"expectedSHA256"`
}

type Spec struct {
	QPS             int    `json:"qps,omitempty"`
	Burst           int    `json:"burst,omitempty"`
	Version         string `json:"version"`
	ID              string `json:"id"`
	NamespacePrefix string `json:"namespacePrefix"`
	GuestNodes      int64  `json:"guestNodes"`
	Applications    int64  `json:"applications"`
	Image           string `json:"image"`
	PerPodCPU       string `json:"perPodCPU"`
	PerPodMemory    string `json:"perPodMemory"`
	TimeoutSeconds  int64  `json:"timeoutSeconds"`
	Binary          Binary `json:"binary"`
}

// Bundle contains kube-burner configuration and its four object templates,
// not an expanded manifest list. Counts describe intent, not observed objects.
// A zero-application bundle has jobs: [] and must not be executed with kube-burner.
type Bundle struct {
	Spec              Spec              `json:"spec"`
	KubeBurnerVersion string            `json:"kubeBurnerVersion"`
	Applications      int64             `json:"applications"`
	Namespaces        int64             `json:"namespaces"`
	Pods              int64             `json:"pods"`
	Config            string            `json:"config"`
	Templates         map[string]string `json:"templates"`
	Warning           string            `json:"warning"`
}

func (s Spec) Validate() error {
	if (s.QPS == 0) != (s.Burst == 0) || s.QPS < 0 || s.QPS > 100 || s.Burst < 0 || s.Burst > 100 {
		return errors.New("seed qps/burst must both be omitted or in 1..100")
	}
	if s.Version != Version || s.GuestNodes < 0 || s.GuestNodes > 1000 {
		return fmt.Errorf("require version v1 and guestNodes in [0,1000]")
	}
	if s.Applications < 0 || s.Applications > 40000 {
		return fmt.Errorf("applications must be in [0,40000]")
	}
	if s.GuestNodes == 0 && s.Applications != 0 {
		return fmt.Errorf("zero guestNodes requires zero applications")
	}
	if len(validation.IsDNS1123Label(s.ID)) != 0 {
		return fmt.Errorf("id must be a DNS label")
	}
	if len(validation.IsDNS1123Label(s.NamespacePrefix)) != 0 || len(validation.IsDNS1123Label(s.NamespacePrefix+"-1999")) != 0 {
		return fmt.Errorf("namespacePrefix must be a DNS label with room for -1999")
	}
	if !pinnedImage.MatchString(s.Image) {
		return fmt.Errorf("image must be pinned with @sha256 and 64 lowercase hex characters")
	}
	for _, field := range []struct{ name, value string }{{"perPodCPU", s.PerPodCPU}, {"perPodMemory", s.PerPodMemory}} {
		q, err := resource.ParseQuantity(field.value)
		if err != nil || q.Sign() <= 0 {
			return fmt.Errorf("%s must be an explicit positive Kubernetes quantity", field.name)
		}
	}
	if s.TimeoutSeconds < 1 || s.TimeoutSeconds > 86400 {
		return fmt.Errorf("timeoutSeconds must be in [1,86400]")
	}
	if s.Binary.Path == "" || strings.ContainsAny(s.Binary.Path, "\x00\r\n") || !sha256Hex.MatchString(s.Binary.ExpectedSHA256) {
		return fmt.Errorf("binary requires an explicit path and expectedSHA256 (64 lowercase hex characters)")
	}
	return nil
}

// Render performs no I/O. It uses the upstream zero-based global .Iteration,
// never .Replica (which is one-based) or namespace-local arithmetic. Namespace
// assignment is kube-burner's built-in iteration / iterationsPerNamespace.
func Render(spec Spec) (Bundle, error) {
	if err := spec.Validate(); err != nil {
		return Bundle{}, err
	}
	b := Bundle{Spec: spec, KubeBurnerVersion: KubeBurnerVersion, Applications: spec.Applications, Namespaces: (spec.Applications + 19) / 20, Pods: spec.Applications * 5, Templates: map[string]string{}, Warning: noPrePullWarning}
	qps, burst := spec.QPS, spec.Burst
	if qps == 0 {
		qps, burst = 20, 20
	}
	b.Config = fmt.Sprintf(`# kube-burner %s; creation only. No image pre-pull or automatic cluster cleanup.
# Complete image pulls and warmup outside the scored window.
global:
  gc: false
  waitWhenFinished: false
  requestTimeout: 30s
  timeout: %ds
  measurements: []
metricsEndpoints: []
`, KubeBurnerVersion, spec.TimeoutSeconds)
	if spec.Applications == 0 {
		b.Config += "jobs: []\n"
		return b, nil
	}
	b.Config += fmt.Sprintf(`jobs:
- name: %q
  jobType: create
  namespace: %q
  namespaceLabels:
    %s: %q
  jobIterations: %d
  iterationsPerNamespace: 20
  namespacedIterations: true
  qps: %d
  burst: %d
  preLoadImages: false
  verifyObjects: true
  errorOnVerify: true
  waitWhenFinished: true
  maxWaitTimeout: %ds
  podWait: false
  cleanup: false
  gc: false
  churn: false
  objects:
`, spec.NamespacePrefix, spec.NamespacePrefix, RunIDLabel, spec.ID, b.Applications, qps, burst, spec.TimeoutSeconds)
	metadata := fmt.Sprintf(`metadata:
  name: 'app-{{ printf "%%06d" .Iteration }}'
  labels:
    %s: %q
    %s: '{{ .Iteration }}'
`, RunIDLabel, spec.ID, ApplicationLabel)
	selector := fmt.Sprintf(`      %s: %q
      %s: '{{ .Iteration }}'
`, RunIDLabel, spec.ID, ApplicationLabel)
	b.Templates["deployment.yaml"] = "apiVersion: apps/v1\nkind: Deployment\n" + metadata + `spec:
  replicas: 5
  revisionHistoryLimit: 2
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
  selector:
    matchLabels:
` + selector + `  template:
    metadata:
      labels:
` + strings.ReplaceAll(selector, "      ", "        ") + fmt.Sprintf(`    spec:
      automountServiceAccountToken: false
      containers:
      - name: app
        image: %q
        imagePullPolicy: IfNotPresent
        resources:
          requests:
            cpu: %q
            memory: %q
`, spec.Image, spec.PerPodCPU, spec.PerPodMemory)
	b.Templates["service.yaml"] = "apiVersion: v1\nkind: Service\n" + metadata + `spec:
  type: ClusterIP
  selector:
` + strings.ReplaceAll(selector, "      ", "    ") + `  ports:
  - name: app
    port: 8080
    targetPort: 8080
    protocol: TCP
`
	b.Templates["configmap.yaml"] = "apiVersion: v1\nkind: ConfigMap\n" + metadata + "data:\n  synthetic: hcp-perf-seed\n"
	b.Templates["secret.yaml"] = "apiVersion: v1\nkind: Secret\n" + metadata + "type: Opaque\nstringData:\n  synthetic: not-a-credential\n"
	for _, name := range []string{"deployment.yaml", "service.yaml", "configmap.yaml", "secret.yaml"} {
		b.Config += fmt.Sprintf("  - objectTemplate: %s\n    replicas: 1\n    wait: true\n    runOnce: false\n", name)
	}
	return b, nil
}

// Write is render-only: materialize a validated bundle in a NEW private output
// directory whose parent exists, without opening credentials or the executable.
// Partial artifacts are retained on failure; existing directories are refused.
func Write(spec Spec, outputDir string) (Bundle, error) {
	b, err := Render(spec)
	if err != nil {
		return b, err
	}
	if err := os.Mkdir(outputDir, 0700); err != nil {
		return b, err
	}
	if err := writePrivate(filepath.Join(outputDir, "config.yaml"), []byte(b.Config)); err != nil {
		return b, err
	}
	for _, name := range []string{"deployment.yaml", "service.yaml", "configmap.yaml", "secret.yaml"} {
		if data, ok := b.Templates[name]; ok {
			if err := writePrivate(filepath.Join(outputDir, name), []byte(data)); err != nil {
				return b, err
			}
		}
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return b, err
	}
	return b, writePrivate(filepath.Join(outputDir, "bundle.json"), append(data, '\n'))
}

func writePrivate(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return errors.Join(err, f.Sync(), f.Close())
}

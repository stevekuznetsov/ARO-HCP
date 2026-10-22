// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package startup applies a guarded sizing candidate and observes an exact HCP.
// It does not create clusters, execute commands, or infer identity from names.
package startup

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const ResourceIDAnnotation = "azure.microsoft.com/hcp-cluster-azure-resource-id"
const HostedClusterAnnotation = "hypershift.openshift.io/cluster"

var HostedClusters = schema.GroupVersionResource{Group: "hypershift.openshift.io", Version: "v1beta1", Resource: "hostedclusters"}
var HostedControlPlanes = schema.GroupVersionResource{Group: "hypershift.openshift.io", Version: "v1beta1", Resource: "hostedcontrolplanes"}
var SizingConfigurations = schema.GroupVersionResource{Group: "scheduling.hypershift.openshift.io", Version: "v1alpha1", Resource: "clustersizingconfigurations"}

var clusterResourceID = regexp.MustCompile(`(?i)^/subscriptions/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/resourcegroups/[^/]+/providers/Microsoft.RedHatOpenShift/hcpOpenShiftClusters/[a-z][a-z0-9-]{0,53}$`)
var pinnedImage = regexp.MustCompile(`^[^\s]+@sha256:[0-9a-f]{64}$`)

type Candidate struct {
	AllowExecCredential bool            `json:"allowExecCredential,omitempty"`
	Kubeconfig          string          `json:"kubeconfig"`
	Name                string          `json:"name"`
	UID                 string          `json:"uid"`
	ResourceVersion     string          `json:"resourceVersion"`
	ExpectedSpec        json.RawMessage `json:"expectedSpec"`
	DesiredSpec         json.RawMessage `json:"desiredSpec"`
	// Retained for frozen inputs; must be empty because the CSC is management-wide.
	EmptyHostedClusterSelector string `json:"emptyHostedClusterSelector"`
}

type Spec struct {
	ProbeFailurePolicy  ProbeFailurePolicy `json:"probeFailurePolicy,omitempty"`
	AllowExecCredential bool               `json:"allowExecCredential,omitempty"`
	Kubeconfig          string             `json:"kubeconfig"`
	ResourceID          string             `json:"resourceID"`
	CandidateName       string             `json:"candidateName"`
	CandidateSpec       json.RawMessage    `json:"candidateSpec"`
	Components          []Component        `json:"components"`
	MaxDuration         time.Duration      `json:"maxDurationNanoseconds"`
	PollInterval        time.Duration      `json:"pollIntervalNanoseconds"`
	HealthyWindow       time.Duration      `json:"healthyWindowNanoseconds"`
}

type ProbeFailurePolicy string

const (
	StrictProbes ProbeFailurePolicy = "strict-probes"
	Availability ProbeFailurePolicy = "availability"
)

// Omitted policy preserves the behavior of existing frozen plans.
func (s Spec) EffectiveProbeFailurePolicy() ProbeFailurePolicy {
	if s.ProbeFailurePolicy == "" {
		return StrictProbes
	}
	return s.ProbeFailurePolicy
}

// Every workload and every regular/init container must be declared. The observer
// rejects undeclared inventory instead of treating four core services as complete.
type Component struct {
	ExpectedUID      string `json:"expectedUID,omitempty"`
	ExpectedOwnerUID string `json:"expectedOwnerUID,omitempty"`
	// Ignored means outside candidate sizing control, not outside verification.
	Scope          string      `json:"scope,omitempty"`
	Reason         string      `json:"reason,omitempty"`
	Kind           string      `json:"kind"`
	Name           string      `json:"name"`
	Replicas       int32       `json:"replicas,omitempty"`
	Lifecycle      *Lifecycle  `json:"lifecycle,omitempty"`
	Endpoints      string      `json:"endpoints,omitempty"`
	ReadyEndpoints int         `json:"readyEndpoints,omitempty"`
	Containers     []Container `json:"containers"`
	InitContainers []Container `json:"initContainers,omitempty"`
}

// Batch lifecycle modes do not imply serving readiness. Optional permits absence,
// not arbitrary names, resource drift or failed executions. Disabled requires an
// existing suspended CronJob, with no active child Jobs.
type Lifecycle struct {
	Mode        string `json:"mode"`
	Reason      string `json:"reason"`
	AllowAbsent bool   `json:"allowAbsent,omitempty"`
}

type Container struct {
	Name  string `json:"name"`
	Image string `json:"image,omitempty"`
	// Nil leaves Go env unchecked; an explicit empty array requires absence.
	Env       *[]corev1.EnvVar            `json:"env,omitempty"`
	Resources corev1.ResourceRequirements `json:"resources"`
}

type Identity struct {
	ResourceID   string `json:"resourceID"`
	ClusterID    string `json:"clusterID"`
	CSID         string `json:"csID"`
	HCNamespace  string `json:"hcNamespace"`
	HCName       string `json:"hcName"`
	HCUID        string `json:"hcUID"`
	HCPNamespace string `json:"hcpNamespace"`
	HCPName      string `json:"hcpName"`
	HCPUID       string `json:"hcpUID"`
	NamespaceUID string `json:"namespaceUID"`
}

type Sample struct {
	ProbeFailurePolicy ProbeFailurePolicy `json:"probeFailurePolicy,omitempty"`
	Warnings           []string           `json:"warnings,omitempty"`
	ProbeQuality       string             `json:"probeQuality,omitempty"`
	NewProbeKills      int                `json:"newProbeKills,omitempty"`
	Coverage           *Coverage          `json:"coverage,omitempty"`
	HealthySince       *time.Time         `json:"healthySince,omitempty"`
	LastSuccessfulPoll *time.Time         `json:"lastSuccessfulPoll,omitempty"`
	NewProbeFailures   int                `json:"newProbeFailures,omitempty"`
	At                 time.Time          `json:"at"`
	Identity           Identity           `json:"identity"`
	Healthy            bool               `json:"healthy"`
	HealthyWindow      bool               `json:"healthyWindow"`
	FreshQualified     bool               `json:"freshQualified"`
	Issues             []string           `json:"issues"`
	NewRestarts        int32              `json:"newRestarts"`
	NewOOMs            int                `json:"newOOMs"`
}

type Coverage struct {
	RequiredWorkloads int   `json:"requiredWorkloads"`
	ObservedWorkloads int   `json:"observedWorkloads"`
	ReadyPods         int32 `json:"readyPods"`
	RequiredPods      int32 `json:"requiredPods"`
	ReadyEndpoints    int   `json:"readyEndpoints"`
	RequiredEndpoints int   `json:"requiredEndpoints"`
	Complete          bool  `json:"complete"`
}

// Status is a bounded, sanitized view of durable evidence, not a live API probe.
type Status struct {
	ProbeFailurePolicy ProbeFailurePolicy `json:"probeFailurePolicy,omitempty"`
	WarningCodes       []string           `json:"warningCodes,omitempty"`
	ProbeQuality       string             `json:"probeQuality,omitempty"`
	NewProbeKills      int                `json:"newProbeKills"`
	Phase              string             `json:"phase"`
	ObservedAt         *time.Time         `json:"observedAt,omitempty"`
	LastSuccessfulPoll *time.Time         `json:"lastSuccessfulPoll,omitempty"`
	HealthySince       *time.Time         `json:"healthySince,omitempty"`
	LastError          string             `json:"lastError,omitempty"`
	SnapshotState      string             `json:"snapshotState"`
	Stale              bool               `json:"stale"`
	StaleAfterSeconds  int64              `json:"staleAfterSeconds"`
	Coverage           *Coverage          `json:"coverage,omitempty"`
	UnhealthyWorkloads []string           `json:"unhealthyWorkloads"`
	IssueCodes         []string           `json:"issueCodes"`
	NewRestarts        int32              `json:"newRestarts"`
	NewOOMs            int                `json:"newOOMs"`
	NewProbeFailures   int                `json:"newProbeFailures"`
	HealthyWindow      bool               `json:"healthyWindow"`
	FreshQualified     bool               `json:"freshQualified"`
}

type Clients struct {
	Kubernetes kubernetes.Interface
	Dynamic    dynamic.Interface
}

// Kubeconfigs and nested credential files remain external. Explicit opt-in allows
// client-go exec authentication only, never orchestration commands. The trusted
// executable, arguments and environment remain in the external kubeconfig.
func NewClients(path string, allowExecCredential ...bool) (Clients, error) {
	var clients Clients
	if !filepath.IsAbs(path) {
		return clients, errors.New("kubeconfig must be an absolute external path")
	}
	config, err := clientcmd.LoadFromFile(path)
	if err != nil {
		return clients, errors.New("load external kubeconfig failed")
	}
	for _, cluster := range config.Clusters {
		if cluster.CertificateAuthority != "" && !filepath.IsAbs(cluster.CertificateAuthority) {
			return clients, errors.New("nested certificate authority path must be absolute")
		}
	}
	for _, auth := range config.AuthInfos {
		if auth.AuthProvider != nil {
			return clients, errors.New("legacy kubeconfig auth-provider plugins are unsupported")
		}
		if auth.Exec != nil {
			if len(allowExecCredential) != 1 || !allowExecCredential[0] || !filepath.IsAbs(auth.Exec.Command) {
				return clients, errors.New("exec credentials require explicit opt-in and an absolute executable path")
			}
			if auth.Exec.InteractiveMode != clientcmdapi.NeverExecInteractiveMode {
				return clients, errors.New("detached exec credentials require interactiveMode Never")
			}
			for _, variable := range auth.Exec.Env {
				if variable.Name == "AZURE_CONFIG_DIR" && !filepath.IsAbs(variable.Value) {
					return clients, errors.New("exec AZURE_CONFIG_DIR must be absolute")
				}
			}
		}
		for _, nested := range []string{auth.ClientCertificate, auth.ClientKey, auth.TokenFile} {
			if nested != "" && !filepath.IsAbs(nested) {
				return clients, errors.New("nested credential paths must be absolute")
			}
		}
	}
	restConfig, err := clientcmd.NewDefaultClientConfig(*config, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return clients, errors.New("configure Kubernetes client failed")
	}
	restConfig.Timeout = 30 * time.Second
	clients.Kubernetes, err = kubernetes.NewForConfig(restConfig)
	if err != nil {
		return clients, err
	}
	clients.Dynamic, err = dynamic.NewForConfig(restConfig)
	return clients, err
}

func (c Candidate) Validate() error {
	if !filepath.IsAbs(c.Kubeconfig) || c.Name == "" || c.UID == "" || c.ResourceVersion == "" {
		return errors.New("candidate requires absolute kubeconfig, name, UID and resourceVersion")
	}
	if c.Name != "cluster" || c.EmptyHostedClusterSelector != "" {
		return errors.New("candidate requires singleton CSC cluster and unfiltered HostedCluster inventory; selectors do not isolate sizing")
	}
	for _, data := range []json.RawMessage{c.ExpectedSpec, c.DesiredSpec} {
		if _, err := DecodeCandidateSpec(data); err != nil {
			return err
		}
	}
	return nil
}

func (s Spec) Validate() error {
	if s.EffectiveProbeFailurePolicy() != StrictProbes && s.EffectiveProbeFailurePolicy() != Availability {
		return errors.New("probeFailurePolicy must be strict-probes or availability")
	}
	_, candidateErr := DecodeCandidateSpec(s.CandidateSpec)
	if !filepath.IsAbs(s.Kubeconfig) || !clusterResourceID.MatchString(s.ResourceID) || s.CandidateName == "" || candidateErr != nil {
		return errors.New("startup requires absolute kubeconfig, exact ARM resource ID and candidate")
	}
	if s.CandidateName != "cluster" {
		return errors.New("startup requires singleton CSC cluster; per-HostedCluster CSC references are unsupported")
	}
	if s.MaxDuration < 5*time.Minute || s.MaxDuration > 2*time.Hour || s.PollInterval < 5*time.Second || s.PollInterval > 30*time.Second || s.HealthyWindow < 5*time.Minute || s.HealthyWindow > s.MaxDuration {
		return errors.New("startup requires duration 5m..2h, poll 5s..30s and healthy window >=5m")
	}
	seen := map[string]bool{}
	for _, component := range s.Components {
		key := component.Kind + "/" + component.Name
		if component.ExpectedUID != "" || component.ExpectedOwnerUID != "" {
			if component.Kind != "Job" || component.ExpectedUID == "" || component.ExpectedOwnerUID == "" || component.Lifecycle == nil || component.Lifecycle.Mode != "completed" {
				return errors.New("historical identity requires completed Job with exact UID and CronJob owner UID")
			}
		}
		if seen[key] || component.Name == "" || len(component.Containers) == 0 {
			return errors.New("startup requires unique named inventory with explicit containers")
		}
		if component.Scope != "" && component.Scope != "controlled" && component.Scope != "ignored" {
			return errors.New("component scope must be controlled or ignored")
		}
		if component.Scope == "ignored" && strings.TrimSpace(component.Reason) == "" {
			return errors.New("ignored scope requires an explicit reason")
		}
		switch component.Kind {
		case "Deployment", "StatefulSet", "DaemonSet":
			if component.Lifecycle != nil {
				if component.Lifecycle.Mode != "disabled" || strings.TrimSpace(component.Lifecycle.Reason) == "" || component.Replicas != 0 || component.Endpoints != "" || component.ReadyEndpoints != 0 {
					return errors.New("disabled workload requires reason, zero replicas and no endpoints")
				}
			} else if component.Replicas < 1 {
				return errors.New("serving workloads require positive replicas")
			}
		case "Job", "CronJob", "Pod":
			if component.Lifecycle == nil || component.Lifecycle.Reason == "" || component.Replicas != 0 || component.Endpoints != "" || component.ReadyEndpoints != 0 {
				return errors.New("Job/CronJob/Pod require explicit lifecycle mode/reason, without replicas or endpoints")
			}
			if component.Lifecycle.AllowAbsent {
				return errors.New("allowAbsent is only supported for disabled serving workloads; use optional for Job/Pod")
			}
			mode := component.Lifecycle.Mode
			if component.Kind == "CronJob" {
				if mode != "present" && mode != "disabled" {
					return errors.New("CronJob lifecycle must be present or disabled")
				}
			} else if mode != "present" && mode != "completed" && mode != "optional" {
				return errors.New("Job/Pod lifecycle must be present, completed or optional")
			}
		default:
			return errors.New("unsupported startup inventory kind")
		}
		seen[key] = true
		if component.ReadyEndpoints < 0 || (component.Endpoints == "") != (component.ReadyEndpoints == 0) {
			return errors.New("endpoint requirements need a name and positive count together")
		}
		for _, containers := range [][]Container{component.Containers, component.InitContainers} {
			names := map[string]bool{}
			for _, container := range containers {
				if container.Name == "" || names[container.Name] {
					return errors.New("startup container names must be nonempty and unique")
				}
				names[container.Name] = true
				if container.Image != "" && !pinnedImage.MatchString(container.Image) {
					return errors.New("expected image must be digest-pinned")
				}
				envNames := map[string]bool{}
				var expectedEnv []corev1.EnvVar
				if container.Env != nil {
					expectedEnv = *container.Env
				}
				for _, variable := range expectedEnv {
					if (variable.Name != "GOMEMLIMIT" && variable.Name != "GOMAXPROCS") || variable.ValueFrom != nil || envNames[variable.Name] {
						return errors.New("expected env permits only unique literal GOMEMLIMIT/GOMAXPROCS entries")
					}
					envNames[variable.Name] = true
				}
				if component.Scope == "ignored" {
					continue
				}
				for _, resources := range []corev1.ResourceList{container.Resources.Requests, container.Resources.Limits} {
					for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
						quantity, ok := resources[name]
						if !ok || quantity.Sign() <= 0 {
							return errors.New("startup expected CPU/memory requests and limits must be positive and explicit")
						}
					}
				}
			}
		}
	}
	for _, core := range []string{"Deployment/kube-apiserver", "Deployment/openshift-apiserver", "Deployment/packageserver", "StatefulSet/etcd"} {
		if !seen[core] {
			return fmt.Errorf("required core component %s missing", core)
		}
		for _, component := range s.Components {
			if component.Kind+"/"+component.Name == core && (component.Replicas < 3 || component.ReadyEndpoints < 3) {
				return fmt.Errorf("core %s requires at least three replicas and ready endpoints", core)
			}
		}
	}
	return nil
}

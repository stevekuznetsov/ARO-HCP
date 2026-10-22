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

// Package scrape generates standalone Prometheus configuration without changing
// cluster resources. Monitor discovery is a snapshot: regenerate after creating
// namespaces or monitors. Endpoint and pod discovery remains live in Prometheus.
package scrape

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	"sigs.k8s.io/yaml"
)

const (
	ServiceAccountCAFile    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	ServiceAccountTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
)

// KubeClient is satisfied by both Kubernetes clientsets and their fakes.
type KubeClient interface {
	CoreV1() corev1client.CoreV1Interface
}

type Spec struct {
	TargetNamespaces []string `json:"targetNamespaces,omitempty"`
	// NamespaceRegex is fully anchored and intersects TargetNamespaces when set.
	// Without explicit names, only namespaces are listed cluster-wide, never monitors.
	NamespaceRegex           string `json:"namespaceRegex,omitempty"`
	ClusterID                string `json:"clusterID"`
	PrometheusNamespace      string `json:"prometheusNamespace"`
	PrometheusServiceAccount string `json:"prometheusServiceAccount,omitempty"`
	WorkerRole               string `json:"workerRole,omitempty"`
	ScrapeInterval           string `json:"scrapeInterval,omitempty"`
	ScrapeTimeout            string `json:"scrapeTimeout,omitempty"`
	KubeletCAFile            string `json:"kubeletCAFile,omitempty"`
	KubeletServerName        string `json:"kubeletServerName,omitempty"`
	// Component overrides are files/names in the Prometheus pod, not generator paths.
	ComponentCAFile     string                `json:"componentCAFile,omitempty"`
	ComponentServerName string                `json:"componentServerName,omitempty"`
	EndpointCredentials []EndpointCredentials `json:"endpointCredentials,omitempty"`
	// TLSMounts optionally restricts monitor references to exact source keys.
	// Empty allows all TLS/auth references from in-scope monitors and overrides.
	// This is a rendering policy, NOT permission to read or copy credentials.
	TLSMounts                    []TLSReference `json:"tlsMounts,omitempty"`
	NodeExporterServiceNamespace string         `json:"nodeExporterServiceNamespace,omitempty"`
	NodeExporterServiceName      string         `json:"nodeExporterServiceName,omitempty"`
	NodeExporterPort             string         `json:"nodeExporterPort,omitempty"`
}

// Gap is always a blocker to complete configuration of the declared scope.
type Gap struct {
	Source  string `json:"source"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

type Source struct {
	Kind            string            `json:"kind"`
	Namespace       string            `json:"namespace,omitempty"`
	Name            string            `json:"name"`
	UID             string            `json:"uid,omitempty"`
	ResourceVersion string            `json:"resourceVersion,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty"`
	Jobs            []string          `json:"jobs"`
	Notes           []string          `json:"notes,omitempty"`
}

// RBACRequirement describes permissions, not resources to apply. Empty namespaces
// means cluster scope. Generator permissions belong to the calling identity;
// scraper permissions belong to the service account identified in Bundle.Spec.
// tls-materializer permissions are declarative, for a separate opt-in consumer.
type RBACRequirement struct {
	Purpose    string              `json:"purpose"`
	Namespaces []string            `json:"namespaces,omitempty"`
	Rules      []rbacv1.PolicyRule `json:"rules"`
}

type Bundle struct {
	Spec         Spec                  `json:"spec"`
	Config       string                `json:"config"`
	Sources      []Source              `json:"sources"`
	ExpectedJobs []string              `json:"expectedJobs"`
	Gaps         []Gap                 `json:"gaps"`
	RBAC         []RBACRequirement     `json:"rbac"`
	TLSMounts    []TLSMountRequirement `json:"tlsMounts,omitempty"`
	Notes        []string              `json:"notes"`
}

// Generate returns a usable partial config alongside explicit gaps for unsupported
// monitors. Invalid input, API failures and an empty monitor inventory return an
// error. No secrets are fetched, files written, or Kubernetes resources mutated.
// A gap-free bundle only covers declared, currently discovered monitors; it is
// not proof of runtime reachability, metrics availability, or replica coverage.
// TLSMounts must be materialized separately before starting Prometheus.
func Generate(ctx context.Context, kubeclient KubeClient, dynamicclient dynamic.Interface, spec Spec) (Bundle, error) {
	b := Bundle{Gaps: []Gap{}, Sources: []Source{}, ExpectedJobs: []string{}}
	if spec.ScrapeInterval == "" {
		spec.ScrapeInterval = "5s"
	}
	if spec.ScrapeTimeout == "" {
		spec.ScrapeTimeout = spec.ScrapeInterval
	}
	interval, err := time.ParseDuration(spec.ScrapeInterval)
	if err != nil || (interval != 5*time.Second && interval != 15*time.Second) {
		return b, fmt.Errorf("scrapeInterval must be 5s or 15s")
	}
	timeout, err := time.ParseDuration(spec.ScrapeTimeout)
	if err != nil || timeout < time.Millisecond || timeout > interval || timeout%time.Millisecond != 0 {
		return b, fmt.Errorf("scrapeTimeout must be a positive whole number of milliseconds no greater than scrapeInterval")
	}
	// Normalize Go durations to Prometheus-compatible durations (no fractional seconds).
	spec.ScrapeInterval = fmt.Sprintf("%ds", int64(interval/time.Second))
	spec.ScrapeTimeout = fmt.Sprintf("%dms", int64(timeout/time.Millisecond))
	if spec.ClusterID == "" {
		return b, fmt.Errorf("clusterID is required")
	}
	if spec.PrometheusServiceAccount == "" {
		spec.PrometheusServiceAccount = "hcp-perf"
	}
	if len(validation.IsDNS1123Label(spec.PrometheusNamespace)) != 0 || len(validation.IsDNS1123Subdomain(spec.PrometheusServiceAccount)) != 0 {
		return b, fmt.Errorf("valid prometheusNamespace and prometheusServiceAccount are required")
	}
	if spec.WorkerRole == "" {
		spec.WorkerRole = "worker"
	}
	if len(validation.IsValidLabelValue(spec.WorkerRole)) != 0 {
		return b, fmt.Errorf("invalid workerRole")
	}
	if spec.KubeletCAFile == "" {
		spec.KubeletCAFile = ServiceAccountCAFile
	}
	for _, reference := range spec.TLSMounts {
		if err := reference.validate(); err != nil {
			return b, fmt.Errorf("tlsMounts: %w", err)
		}
	}
	credentialJobs := map[string]bool{}
	for _, override := range spec.EndpointCredentials {
		if err := override.validate(); err != nil {
			return b, err
		}
		if credentialJobs[override.job()] {
			return b, fmt.Errorf("duplicate endpointCredentials for %s", override.job())
		}
		credentialJobs[override.job()] = true
	}
	if kubeclient == nil || dynamicclient == nil {
		return b, fmt.Errorf("Kubernetes and dynamic clients are required")
	}
	if len(spec.TargetNamespaces) == 0 && spec.NamespaceRegex == "" {
		return b, fmt.Errorf("targetNamespaces or namespaceRegex is required; unscoped discovery is not allowed")
	}
	namespaces := slices.Clone(spec.TargetNamespaces)
	for _, ns := range namespaces {
		if len(validation.IsDNS1123Label(ns)) != 0 {
			return b, fmt.Errorf("invalid target namespace %q", ns)
		}
	}
	if spec.NamespaceRegex != "" {
		r, err := regexp.Compile("^(?:" + spec.NamespaceRegex + ")$")
		if err != nil {
			return b, fmt.Errorf("namespaceRegex: %w", err)
		}
		if len(namespaces) == 0 {
			b.RBAC = append(b.RBAC, RBACRequirement{Purpose: "generator", Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"namespaces"}, Verbs: []string{"list"}}}})
			list, err := kubeclient.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
			if err != nil {
				return b, fmt.Errorf("list namespaces: %w", err)
			}
			for _, ns := range list.Items {
				namespaces = append(namespaces, ns.Name)
			}
		}
		namespaces = slices.DeleteFunc(namespaces, func(ns string) bool { return !r.MatchString(ns) })
	}
	slices.Sort(namespaces)
	namespaces = slices.Compact(namespaces)
	b.Spec = spec
	b.Notes = []string{
		"All metrics requested: inherited metric keep/drop filters are intentionally removed; label transformations and sampleLimit are retained.",
		"Monitor intervals and timeouts are overridden by Spec. Regenerate for new namespaces, monitors, or changed monitor specs; Kubernetes SD only discovers targets for this snapshot.",
		"Mount referenced CA, certificate, key and token files in Prometheus. TLS verification is always enabled; the service-account CA may not trust kubelet serving certificates.",
		"TLSMounts are declarative requirements, not applied resources: materialize only their listed keys and mount their volumes before starting Prometheus. Cross-namespace references require explicit copying into destinationNamespace and a credential rotation strategy; no secret contents are included or read by Generate.",
		"Allow network access to node kubelet ports and component endpoints. HCP network policy requires the Prometheus namespace label network.openshift.io/policy-group=monitoring.",
		"Node metrics describe worker-role nodes of the connected cluster, not tenant workers in another cluster. Node exporter uses the explicitly supplied HTTP service endpoint.",
		"CheckTargets checks active scrape pools, not expected pod/node identities or replica counts; it cannot prove full component coverage.",
	}
	if len(namespaces) == 0 {
		b.Gaps = append(b.Gaps, Gap{Source: "monitors", Message: "no namespaces in scope; regenerate after the target namespace and monitors exist"})
		return b, fmt.Errorf("no target namespaces")
	}
	b.RBAC = append(b.RBAC,
		RBACRequirement{Purpose: "generator", Namespaces: namespaces, Rules: []rbacv1.PolicyRule{{APIGroups: []string{"monitoring.coreos.com"}, Resources: []string{"servicemonitors", "podmonitors"}, Verbs: []string{"list"}}}},
		RBACRequirement{Purpose: "scraper", Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{""}, Resources: []string{"nodes/metrics"}, Verbs: []string{"get"}},
			{NonResourceURLs: []string{"/metrics"}, Verbs: []string{"get"}},
		}},
	)
	jobs := []map[string]any{}
	for _, item := range []struct{ name, path string }{{"cadvisor", "/metrics/cadvisor"}, {"kubelet", "/metrics"}} {
		job := baseJob(item.name, "node", nil, spec)
		job["scheme"], job["metrics_path"] = "https", item.path
		job["bearer_token_file"] = ServiceAccountTokenFile
		job["tls_config"] = map[string]any{"ca_file": spec.KubeletCAFile, "server_name": spec.KubeletServerName, "insecure_skip_verify": false}
		job["relabel_configs"] = []map[string]any{
			keep("__meta_kubernetes_node_label_aro_hcp_azure_com_role", regexp.QuoteMeta(spec.WorkerRole)),
			label("__meta_kubernetes_node_name", "node"),
		}
		jobs = append(jobs, job)
		b.ExpectedJobs = append(b.ExpectedJobs, item.name)
		b.Sources = append(b.Sources, Source{Kind: "Node", Name: item.name, Jobs: []string{item.name}})
	}
	sdNamespaces := slices.Clone(namespaces)
	if spec.NodeExporterServiceNamespace == "" || spec.NodeExporterServiceName == "" || spec.NodeExporterPort == "" {
		b.Gaps = append(b.Gaps, Gap{Source: "node-exporter", Message: "nodeExporterServiceNamespace, nodeExporterServiceName and nodeExporterPort are required; no service is assumed to exist"})
	} else {
		if len(validation.IsDNS1123Label(spec.NodeExporterServiceNamespace)) != 0 || len(validation.IsDNS1035Label(spec.NodeExporterServiceName)) != 0 || len(validation.IsValidPortName(spec.NodeExporterPort)) != 0 {
			return b, fmt.Errorf("invalid node exporter service namespace, name or port")
		}
		b.RBAC = append(b.RBAC, RBACRequirement{Purpose: "generator", Namespaces: []string{spec.NodeExporterServiceNamespace}, Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"services"}, ResourceNames: []string{spec.NodeExporterServiceName}, Verbs: []string{"get"}}}})
		svc, err := kubeclient.CoreV1().Services(spec.NodeExporterServiceNamespace).Get(ctx, spec.NodeExporterServiceName, metav1.GetOptions{})
		if err != nil {
			b.Gaps = append(b.Gaps, Gap{Source: "node-exporter", Message: fmt.Sprintf("get service: %v", err)})
		} else {
			found := false
			for _, port := range svc.Spec.Ports {
				if port.Name == spec.NodeExporterPort && (port.Protocol == "TCP" || port.Protocol == "") {
					found = true
				}
			}
			if !found {
				b.Gaps = append(b.Gaps, Gap{Source: "node-exporter", Message: "service has no matching named TCP port"})
			} else {
				sdNamespaces = append(sdNamespaces, svc.Namespace)
				job := baseJob("node-exporter", "endpointslice", []string{svc.Namespace}, spec)
				job["kubernetes_sd_configs"].([]any)[0].(map[string]any)["attach_metadata"] = map[string]any{"node": true}
				job["relabel_configs"] = []map[string]any{
					keep("__meta_kubernetes_service_name", regexp.QuoteMeta(svc.Name)),
					keep("__meta_kubernetes_endpointslice_port_name", regexp.QuoteMeta(spec.NodeExporterPort)),
					keep("__meta_kubernetes_node_label_aro_hcp_azure_com_role", regexp.QuoteMeta(spec.WorkerRole)),
					label("__meta_kubernetes_endpointslice_endpoint_node_name", "node"),
					label("__meta_kubernetes_namespace", "namespace"),
				}
				jobs = append(jobs, job)
				b.ExpectedJobs = append(b.ExpectedJobs, "node-exporter")
				b.Sources = append(b.Sources, Source{Kind: "Service", Namespace: svc.Namespace, Name: svc.Name, UID: string(svc.UID), ResourceVersion: svc.ResourceVersion, Labels: svc.Labels, Jobs: []string{"node-exporter"}})
			}
		}
	}
	slices.Sort(sdNamespaces)
	sdNamespaces = slices.Compact(sdNamespaces)
	b.RBAC = append(b.RBAC, RBACRequirement{Purpose: "scraper", Namespaces: sdNamespaces, Rules: []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"services", "pods"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{"discovery.k8s.io"}, Resources: []string{"endpointslices"}, Verbs: []string{"get", "list", "watch"}},
	}})
	monitorCount := 0
	for _, ns := range namespaces {
		count := 0
		for _, kind := range []struct{ kind, resource string }{{"ServiceMonitor", "servicemonitors"}, {"PodMonitor", "podmonitors"}} {
			list, err := dynamicclient.Resource(schema.GroupVersionResource{Group: "monitoring.coreos.com", Version: "v1", Resource: kind.resource}).Namespace(ns).List(ctx, metav1.ListOptions{})
			if err != nil {
				return b, fmt.Errorf("list %s in %s (monitor CRDs and list permission are required): %w", kind.resource, ns, err)
			}
			slices.SortFunc(list.Items, func(a, b unstructured.Unstructured) int { return strings.Compare(a.GetName(), b.GetName()) })
			for _, monitor := range list.Items {
				count++
				source := Source{Kind: kind.kind, Namespace: ns, Name: monitor.GetName(), UID: string(monitor.GetUID()), ResourceVersion: monitor.GetResourceVersion(), Labels: monitor.GetLabels(), Annotations: monitor.GetAnnotations(), Jobs: []string{}}
				id := kind.kind + "/" + ns + "/" + monitor.GetName()
				monitorJobs, notes, mounts, err := convertMonitor(monitor.Object["spec"], kind.kind, ns, id, namespaces, spec)
				source.Notes = notes
				if err != nil {
					b.Gaps = append(b.Gaps, Gap{Source: id, Field: "spec", Message: err.Error()})
				}
				for _, mount := range mounts {
					index := slices.IndexFunc(b.TLSMounts, func(existing TLSMountRequirement) bool { return existing.Source == mount.Source })
					if index >= 0 {
						for _, job := range mount.Jobs {
							if !slices.Contains(b.TLSMounts[index].Jobs, job) {
								b.TLSMounts[index].Jobs = append(b.TLSMounts[index].Jobs, job)
							}
						}
						continue
					}
					b.TLSMounts = append(b.TLSMounts, mount)
					resource := "secrets"
					if mount.Source.Kind == "ConfigMap" {
						resource = "configmaps"
					}
					b.RBAC = append(b.RBAC, RBACRequirement{Purpose: "tls-materializer", Namespaces: []string{mount.Source.Namespace}, Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{resource}, ResourceNames: []string{mount.Source.Name}, Verbs: []string{"get"}}}})
				}
				for _, job := range monitorJobs {
					name := job["job_name"].(string)
					source.Jobs = append(source.Jobs, name)
					b.ExpectedJobs = append(b.ExpectedJobs, name)
					jobs = append(jobs, job)
				}
				b.Sources = append(b.Sources, source)
			}
		}
		monitorCount += count
		if count == 0 {
			b.Gaps = append(b.Gaps, Gap{Source: ns, Message: "no ServiceMonitors or PodMonitors; regenerate after components create their monitors"})
		}
	}
	for _, override := range spec.EndpointCredentials {
		if !slices.Contains(b.ExpectedJobs, override.job()) {
			b.Gaps = append(b.Gaps, Gap{Source: override.job(), Field: "endpointCredentials", Message: "override did not match a supported endpoint in scope"})
		}
	}
	config, err := yaml.Marshal(map[string]any{
		"global":         map[string]any{"scrape_interval": spec.ScrapeInterval, "scrape_timeout": spec.ScrapeTimeout, "external_labels": map[string]string{"cluster": spec.ClusterID, "prometheus": spec.PrometheusNamespace + "/" + spec.PrometheusServiceAccount}},
		"scrape_configs": jobs,
	})
	if err != nil {
		return b, fmt.Errorf("marshal config: %w", err)
	}
	b.Config = string(config)
	if monitorCount == 0 {
		return b, fmt.Errorf("no ServiceMonitors or PodMonitors found in target namespaces")
	}
	return b, nil
}

func baseJob(name, role string, namespaces []string, spec Spec) map[string]any {
	sd := map[string]any{"role": role}
	if len(namespaces) > 0 {
		sd["namespaces"] = map[string]any{"names": namespaces}
	}
	return map[string]any{"job_name": name, "scrape_interval": spec.ScrapeInterval, "scrape_timeout": spec.ScrapeTimeout, "kubernetes_sd_configs": []any{sd}}
}

func keep(source, regex string) map[string]any {
	return map[string]any{"action": "keep", "source_labels": []string{source}, "regex": regex}
}
func label(source, target string) map[string]any {
	return map[string]any{"action": "replace", "source_labels": []string{source}, "target_label": target}
}

func decodeStrict(value any, target any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	return d.Decode(target)
}

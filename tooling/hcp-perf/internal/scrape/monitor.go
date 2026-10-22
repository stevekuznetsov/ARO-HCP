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
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Strict decoding is deliberate: new Operator fields must not silently change
// discovery, authentication or limits when translated to standalone Prometheus.
type monitorSpec struct {
	Selector          *metav1.LabelSelector `json:"selector"`
	NamespaceSelector struct {
		Any        bool     `json:"any"`
		MatchNames []string `json:"matchNames"`
	} `json:"namespaceSelector"`
	Endpoints           []json.RawMessage `json:"endpoints"`
	PodMetricsEndpoints []json.RawMessage `json:"podMetricsEndpoints"`
	JobLabel            string            `json:"jobLabel"`
	TargetLabels        []string          `json:"targetLabels"`
	PodTargetLabels     []string          `json:"podTargetLabels"`
	SampleLimit         *uint64           `json:"sampleLimit"`
}

type endpointSpec struct {
	Port              string              `json:"port"`
	TargetPort        *intstr.IntOrString `json:"targetPort"`
	Path              string              `json:"path"`
	Scheme            string              `json:"scheme"`
	Interval          string              `json:"interval"`
	ScrapeTimeout     string              `json:"scrapeTimeout"`
	Params            map[string][]string `json:"params"`
	BearerTokenFile   string              `json:"bearerTokenFile"`
	BearerTokenSecret *tlsKeySelector     `json:"bearerTokenSecret"`
	Authorization     *authorizationSpec  `json:"authorization"`
	TLSConfig         json.RawMessage     `json:"tlsConfig"`
	HonorLabels       *bool               `json:"honorLabels"`
	HonorTimestamps   *bool               `json:"honorTimestamps"`
	FollowRedirects   *bool               `json:"followRedirects"`
	EnableHTTP2       *bool               `json:"enableHttp2"`
	FilterRunning     *bool               `json:"filterRunning"`
	Relabelings       []relabeling        `json:"relabelings"`
	MetricRelabelings []relabeling        `json:"metricRelabelings"`
}

type relabeling struct {
	SourceLabels []string `json:"sourceLabels"`
	Separator    *string  `json:"separator"`
	TargetLabel  string   `json:"targetLabel"`
	Regex        *string  `json:"regex"`
	Modulus      *uint64  `json:"modulus"`
	Replacement  *string  `json:"replacement"`
	Action       string   `json:"action"`
}

func convertMonitor(raw any, kind, namespace, id string, allowed []string, spec Spec) ([]map[string]any, []string, []TLSMountRequirement, error) {
	var m monitorSpec
	if err := decodeStrict(raw, &m); err != nil {
		return nil, nil, nil, err
	}
	if m.Selector == nil {
		return nil, nil, nil, fmt.Errorf("selector is required")
	}
	if _, err := metav1.LabelSelectorAsSelector(m.Selector); err != nil {
		return nil, nil, nil, fmt.Errorf("selector: %w", err)
	}
	namespaces := []string{}
	for _, ns := range allowed {
		if m.NamespaceSelector.Any || slices.Contains(m.NamespaceSelector.MatchNames, ns) || (len(m.NamespaceSelector.MatchNames) == 0 && ns == namespace) {
			namespaces = append(namespaces, ns)
		}
	}
	if len(namespaces) == 0 {
		return nil, nil, nil, fmt.Errorf("namespaceSelector has no intersection with declared scope")
	}
	role, prefix, portLabel := "endpointslice", "__meta_kubernetes_service_label_", "__meta_kubernetes_endpointslice_port_name"
	endpoints := m.Endpoints
	if kind == "PodMonitor" {
		if len(m.Endpoints) > 0 || len(m.TargetLabels) > 0 {
			return nil, nil, nil, fmt.Errorf("ServiceMonitor fields on PodMonitor")
		}
		role, prefix, portLabel = "pod", "__meta_kubernetes_pod_label_", "__meta_kubernetes_pod_container_port_name"
		endpoints = m.PodMetricsEndpoints
	} else if len(m.PodMetricsEndpoints) > 0 {
		return nil, nil, nil, fmt.Errorf("podMetricsEndpoints on ServiceMonitor")
	}
	if len(endpoints) == 0 {
		return nil, nil, nil, fmt.Errorf("no metrics endpoints")
	}
	selector := selectorRelabelings(m.Selector, prefix)
	jobs := []map[string]any{}
	notes := []string{}
	var mounts []TLSMountRequirement
	var blockers []error
	for i, rawEndpoint := range endpoints {
		jobName := fmt.Sprintf("%s/%d", id, i)
		job, endpointNotes, err := convertEndpoint(rawEndpoint, kind, role, prefix, portLabel, jobName, namespaces, spec, m, selector)
		for _, note := range endpointNotes {
			notes = append(notes, fmt.Sprintf("endpoint %d: %s", i, note))
		}
		if err != nil {
			blockers = append(blockers, fmt.Errorf("endpoint %d: %w", i, err))
			continue
		}
		// Convert TLS only for otherwise supported endpoints so rejected jobs do
		// not authorize or require unused secret materialization.
		var e endpointSpec
		if err := decodeStrict(rawEndpoint, &e); err != nil {
			return nil, nil, nil, err
		}
		auth, requirements, overridden, err := convertCredentials(e, spec, namespace, jobName)
		if err != nil {
			blockers = append(blockers, fmt.Errorf("endpoint %d: %w", i, err))
			continue
		}
		for key, value := range auth {
			job[key] = value
		}
		if e.Scheme == "https" {
			if overridden {
				// X509 authentication takes precedence over bearer tokens in KAS.
				// An explicit override replaces client auth, not server verification.
				var tls map[string]any
				if len(e.TLSConfig) > 0 {
					if err := json.Unmarshal(e.TLSConfig, &tls); err != nil {
						blockers = append(blockers, fmt.Errorf("endpoint %d: invalid tlsConfig", i))
						continue
					}
				}
				for _, field := range []string{"cert", "certFile", "keySecret", "keyFile"} {
					delete(tls, field)
				}
				e.TLSConfig, _ = json.Marshal(tls)
				notes = append(notes, fmt.Sprintf("endpoint %d: client authentication replaced by explicit bearer token reference; server TLS trust retained", i))
			}
			tls, tlsRequirements, err := convertTLS(e.TLSConfig, spec, namespace, jobName)
			if err != nil {
				blockers = append(blockers, fmt.Errorf("endpoint %d: %w", i, err))
				continue
			}
			job["tls_config"] = tls
			requirements = append(requirements, tlsRequirements...)
		}
		mounts = append(mounts, requirements...)
		jobs = append(jobs, job)
	}
	return jobs, notes, mounts, errors.Join(blockers...)
}

func convertEndpoint(raw json.RawMessage, kind, role, prefix, portLabel, jobName string, namespaces []string, spec Spec, monitor monitorSpec, selector []map[string]any) (map[string]any, []string, error) {
	var e endpointSpec
	if err := decodeStrict(raw, &e); err != nil {
		return nil, nil, err
	}
	job := baseJob(jobName, role, namespaces, spec)
	notes := []string{}
	if e.Interval != "" || e.ScrapeTimeout != "" {
		notes = append(notes, "monitor interval/timeout overridden by Spec")
	}
	relabels := slices.Clone(selector)
	if e.Port != "" {
		relabels = append(relabels, keep(portLabel, regexp.QuoteMeta(e.Port)))
	} else if e.TargetPort != nil {
		// Operator targetPort refers to a backing pod's container port, NOT the
		// Service port name. EndpointSlice SD includes these pod metadata labels.
		if e.TargetPort.Type == intstr.String && e.TargetPort.StrVal != "" {
			relabels = append(relabels, keep("__meta_kubernetes_pod_container_port_name", regexp.QuoteMeta(e.TargetPort.StrVal)))
		} else if e.TargetPort.Type == intstr.Int && e.TargetPort.IntVal > 0 && e.TargetPort.IntVal <= 65535 {
			relabels = append(relabels, keep("__meta_kubernetes_pod_container_port_number", e.TargetPort.String()))
		} else {
			return nil, notes, fmt.Errorf("invalid targetPort")
		}
	} else {
		return nil, notes, fmt.Errorf("named port or targetPort is required")
	}
	if e.Scheme == "" {
		e.Scheme = "http"
	}
	if e.Scheme != "http" && e.Scheme != "https" {
		return nil, notes, fmt.Errorf("unsupported scheme %q", e.Scheme)
	}
	if e.Path == "" {
		e.Path = "/metrics"
	}
	if !strings.HasPrefix(e.Path, "/") {
		return nil, notes, fmt.Errorf("metrics path must start with /")
	}
	job["scheme"], job["metrics_path"] = e.Scheme, e.Path
	if len(e.Params) > 0 {
		job["params"] = e.Params
	}
	if len(e.TLSConfig) > 0 && e.Scheme != "https" {
		return nil, notes, fmt.Errorf("tlsConfig requires HTTPS")
	}
	if e.Scheme == "https" && (spec.ComponentCAFile != "" || spec.ComponentServerName != "") {
		notes = append(notes, "component TLS trust/name explicitly overridden by Spec")
	}
	for name, value := range map[string]*bool{"honor_labels": e.HonorLabels, "honor_timestamps": e.HonorTimestamps, "follow_redirects": e.FollowRedirects, "enable_http2": e.EnableHTTP2} {
		if value != nil {
			job[name] = *value
		}
	}
	if monitor.SampleLimit != nil {
		job["sample_limit"] = *monitor.SampleLimit
	}
	if kind == "PodMonitor" && (e.FilterRunning == nil || *e.FilterRunning) {
		relabels = append(relabels, map[string]any{"action": "drop", "source_labels": []string{"__meta_kubernetes_pod_phase"}, "regex": "(Failed|Succeeded)"})
	}
	if kind == "ServiceMonitor" && e.FilterRunning != nil {
		return nil, notes, fmt.Errorf("filterRunning is only supported on PodMonitor")
	}
	relabels = append(relabels, label("__meta_kubernetes_namespace", "namespace"), label("__meta_kubernetes_pod_name", "pod"), label("__meta_kubernetes_pod_container_name", "container"))
	if kind == "ServiceMonitor" {
		relabels = append(relabels, label("__meta_kubernetes_service_name", "service"), label("__meta_kubernetes_service_name", "job"))
	} else {
		relabels = append(relabels, map[string]any{"action": "replace", "target_label": "job", "replacement": strings.Join(strings.Split(jobName, "/")[1:3], "/")})
	}
	if monitor.JobLabel != "" {
		r := label(prefix+sanitizeLabel(monitor.JobLabel), "job")
		r["regex"] = "(.+)"
		relabels = append(relabels, r)
	}
	for _, name := range monitor.TargetLabels {
		relabels = append(relabels, label("__meta_kubernetes_service_label_"+sanitizeLabel(name), sanitizeLabel(name)))
	}
	for _, name := range monitor.PodTargetLabels {
		relabels = append(relabels, label("__meta_kubernetes_pod_label_"+sanitizeLabel(name), sanitizeLabel(name)))
	}
	endpointName := e.Port
	if endpointName == "" {
		endpointName = e.TargetPort.String()
	}
	relabels = append(relabels, map[string]any{"action": "replace", "target_label": "endpoint", "replacement": endpointName})
	for _, r := range e.Relabelings {
		if r.TargetLabel == "__scheme__" || r.Action == "labelmap" {
			return nil, notes, fmt.Errorf("relabelings that can rewrite __scheme__ are unsupported; TLS must not be downgraded")
		}
		converted, err := convertRelabeling(r)
		if err != nil {
			return nil, notes, fmt.Errorf("relabelings: %w", err)
		}
		relabels = append(relabels, converted)
	}
	job["relabel_configs"] = relabels
	metrics := []map[string]any{}
	for _, r := range e.MetricRelabelings {
		converted, err := convertRelabeling(r)
		if err != nil {
			return nil, notes, fmt.Errorf("metricRelabelings: %w", err)
		}
		if r.Action == "keep" || r.Action == "drop" || r.Action == "keepequal" || r.Action == "dropequal" {
			notes = append(notes, "removed metricRelabelings "+r.Action+" filter to request all metrics")
			continue
		}
		metrics = append(metrics, converted)
	}
	if len(metrics) > 0 {
		job["metric_relabel_configs"] = metrics
	}
	return job, notes, nil
}

var invalidLabelCharacter = regexp.MustCompile("[^a-zA-Z0-9_]")

func sanitizeLabel(name string) string { return invalidLabelCharacter.ReplaceAllString(name, "_") }

func selectorRelabelings(selector *metav1.LabelSelector, prefix string) []map[string]any {
	rules := []map[string]any{}
	present := strings.TrimSuffix(prefix, "label_") + "labelpresent_"
	keys := []string{}
	for key := range selector.MatchLabels {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		rules = append(rules, keep(present+sanitizeLabel(key), "true"), keep(prefix+sanitizeLabel(key), regexp.QuoteMeta(selector.MatchLabels[key])))
	}
	for _, expression := range selector.MatchExpressions {
		name := sanitizeLabel(expression.Key)
		values := make([]string, len(expression.Values))
		for i, v := range expression.Values {
			values[i] = regexp.QuoteMeta(v)
		}
		switch expression.Operator {
		case metav1.LabelSelectorOpIn:
			rules = append(rules, keep(present+name, "true"), keep(prefix+name, strings.Join(values, "|")))
		case metav1.LabelSelectorOpNotIn:
			// Missing labels match NotIn, including when the forbidden set contains "".
			rules = append(rules, map[string]any{"action": "drop", "source_labels": []string{present + name, prefix + name}, "regex": "true;(" + strings.Join(values, "|") + ")"})
		case metav1.LabelSelectorOpExists:
			rules = append(rules, keep(present+name, "true"))
		case metav1.LabelSelectorOpDoesNotExist:
			rules = append(rules, map[string]any{"action": "drop", "source_labels": []string{present + name}, "regex": "true"})
		}
	}
	return rules
}

func convertRelabeling(r relabeling) (map[string]any, error) {
	if r.Action == "" {
		r.Action = "replace"
	}
	if !slices.Contains([]string{"replace", "keep", "drop", "hashmod", "labelmap", "labeldrop", "labelkeep", "lowercase", "uppercase", "keepequal", "dropequal"}, r.Action) {
		return nil, fmt.Errorf("unsupported action %q", r.Action)
	}
	if r.Regex != nil {
		if _, err := regexp.Compile(*r.Regex); err != nil {
			return nil, err
		}
	}
	if slices.Contains([]string{"replace", "hashmod", "lowercase", "uppercase", "keepequal", "dropequal"}, r.Action) && r.TargetLabel == "" {
		return nil, fmt.Errorf("%s requires targetLabel", r.Action)
	}
	if r.Action == "hashmod" && (r.Modulus == nil || *r.Modulus == 0) {
		return nil, fmt.Errorf("hashmod requires positive modulus")
	}
	if (r.Action == "keepequal" || r.Action == "dropequal") && (r.Regex != nil || r.Modulus != nil || r.Separator != nil || r.Replacement != nil) {
		return nil, fmt.Errorf("%s only supports sourceLabels and targetLabel", r.Action)
	}
	if (r.Action == "labeldrop" || r.Action == "labelkeep") && (len(r.SourceLabels) > 0 || r.TargetLabel != "" || r.Modulus != nil || r.Separator != nil || r.Replacement != nil) {
		return nil, fmt.Errorf("%s only supports regex", r.Action)
	}
	out := map[string]any{"action": r.Action}
	if len(r.SourceLabels) > 0 {
		out["source_labels"] = r.SourceLabels
	}
	if r.TargetLabel != "" {
		out["target_label"] = r.TargetLabel
	}
	if r.Regex != nil {
		out["regex"] = *r.Regex
	}
	if r.Modulus != nil {
		out["modulus"] = *r.Modulus
	}
	if r.Replacement != nil {
		out["replacement"] = *r.Replacement
	}
	if r.Separator != nil {
		out["separator"] = *r.Separator
	}
	return out, nil
}

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
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// FileReference identifies one selected credential or trust key.
type FileReference struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Key       string `json:"key"`
}

// FileMountRequirement is a declarative input to capture integration, never a
// Secret/ConfigMap payload. Volumes can only reference objects in the pod's own
// namespace. When CopyRequired, a separate opt-in materializer must copy ONLY
// Source.Key into DestinationName in DestinationNamespace with the same key.
// It must also handle rotation and cleanup; Generate does none of these things.
// Volume and VolumeMount can then be added to the Prometheus pod unchanged.
// The pod must set an fsGroup readable by Prometheus for the 0440 projections.
type FileMountRequirement struct {
	Source               FileReference      `json:"source"`
	DestinationNamespace string             `json:"destinationNamespace"`
	DestinationName      string             `json:"destinationName"`
	CopyRequired         bool               `json:"copyRequired"`
	File                 string             `json:"file"`
	Volume               corev1.Volume      `json:"volume"`
	VolumeMount          corev1.VolumeMount `json:"volumeMount"`
	Jobs                 []string           `json:"jobs"`
}

// Preserve the encoding and names used by persisted capture sessions and bundles.
type TLSReference = FileReference
type TLSMountRequirement = FileMountRequirement

func (r FileReference) validate() error {
	if r.Kind != "Secret" && r.Kind != "ConfigMap" {
		return fmt.Errorf("file reference kind must be Secret or ConfigMap")
	}
	if len(validation.IsDNS1123Label(r.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(r.Name)) != 0 || r.Key == "" || len(validation.IsConfigMapKey(r.Key)) != 0 {
		return fmt.Errorf("file reference requires a valid namespace, name and key")
	}
	return nil
}

type tlsKeySelector struct {
	Name     string `json:"name"`
	Key      string `json:"key"`
	Optional *bool  `json:"optional"`
}

type tlsSecretOrConfigMap struct {
	Secret    *tlsKeySelector `json:"secret"`
	ConfigMap *tlsKeySelector `json:"configMap"`
}

func convertTLS(raw json.RawMessage, spec Spec, namespace, job string) (map[string]any, []TLSMountRequirement, error) {
	var tls struct {
		CAFile             string                `json:"caFile"`
		CertFile           string                `json:"certFile"`
		KeyFile            string                `json:"keyFile"`
		ServerName         string                `json:"serverName"`
		InsecureSkipVerify bool                  `json:"insecureSkipVerify"`
		CA                 *tlsSecretOrConfigMap `json:"ca"`
		Cert               *tlsSecretOrConfigMap `json:"cert"`
		KeySecret          *tlsKeySelector       `json:"keySecret"`
	}
	if len(raw) > 0 {
		if err := decodeStrict(raw, &tls); err != nil {
			return nil, nil, fmt.Errorf("tlsConfig: %w", err)
		}
	}
	// SafeTLSConfig serializes its zero-value cert field as {} in live monitors.
	if tls.Cert != nil && tls.Cert.Secret == nil && tls.Cert.ConfigMap == nil {
		tls.Cert = nil
	}
	if tls.InsecureSkipVerify && spec.ComponentCAFile == "" {
		return nil, nil, fmt.Errorf("tlsConfig.insecureSkipVerify: refusing insecure TLS; supply componentCAFile and, if necessary, componentServerName")
	}
	if (tls.CA != nil && tls.CAFile != "") || (tls.Cert != nil && tls.CertFile != "") || (tls.KeySecret != nil && tls.KeyFile != "") {
		return nil, nil, fmt.Errorf("tlsConfig cannot specify both a reference and a file for the same credential")
	}
	var keyReference *tlsSecretOrConfigMap
	if tls.KeySecret != nil {
		keyReference = &tlsSecretOrConfigMap{Secret: tls.KeySecret}
	}
	var mounts []TLSMountRequirement
	for _, field := range []struct {
		name      string
		reference *tlsSecretOrConfigMap
		file      *string
	}{
		{"ca", tls.CA, &tls.CAFile},
		{"cert", tls.Cert, &tls.CertFile},
		{"keySecret", keyReference, &tls.KeyFile},
	} {
		if field.reference == nil {
			continue
		}
		if (field.reference.Secret == nil) == (field.reference.ConfigMap == nil) {
			return nil, nil, fmt.Errorf("tlsConfig.%s requires exactly one secret or configMap selector", field.name)
		}
		selector, kind := field.reference.Secret, "Secret"
		if selector == nil {
			selector, kind = field.reference.ConfigMap, "ConfigMap"
		}
		reference := TLSReference{Kind: kind, Namespace: namespace, Name: selector.Name, Key: selector.Key}
		if err := reference.validate(); err != nil {
			return nil, nil, fmt.Errorf("tlsConfig.%s: %w", field.name, err)
		}
		if selector.Optional != nil && *selector.Optional {
			return nil, nil, fmt.Errorf("tlsConfig.%s: optional TLS keys are unsupported; credentials must be present", field.name)
		}
		if field.name == "ca" && spec.ComponentCAFile != "" {
			continue
		}
		if len(spec.TLSMounts) > 0 && !slices.Contains(spec.TLSMounts, reference) {
			return nil, nil, fmt.Errorf("tlsConfig.%s: %s %s/%s key %s is not allowed by tlsMounts", field.name, kind, namespace, selector.Name, selector.Key)
		}
		mount := fileMount(reference, spec.PrometheusNamespace, job)
		*field.file = mount.File
		mounts = append(mounts, mount)
	}
	if (tls.CertFile == "") != (tls.KeyFile == "") {
		return nil, nil, fmt.Errorf("tlsConfig requires both a client certificate and private key")
	}
	if spec.ComponentCAFile != "" {
		tls.CAFile = spec.ComponentCAFile
	}
	if spec.ComponentServerName != "" {
		tls.ServerName = spec.ComponentServerName
	}
	out := map[string]any{"insecure_skip_verify": false}
	for k, v := range map[string]string{"ca_file": tls.CAFile, "cert_file": tls.CertFile, "key_file": tls.KeyFile, "server_name": tls.ServerName} {
		if v != "" {
			out[k] = v
		}
	}
	return out, mounts, nil
}

func fileMount(reference FileReference, namespace, job string) FileMountRequirement {
	// Keep the persisted TLS path format for all selected-key file credentials.
	digest := sha256.Sum256([]byte(reference.Kind + "/" + reference.Namespace + "/" + reference.Name + "/" + reference.Key))
	name := fmt.Sprintf("hcp-perf-tls-%x", digest[:16])
	destination := reference.Name
	copyRequired := reference.Namespace != namespace
	if copyRequired {
		destination = name
	}
	mode, optional := int32(0440), false
	volume := corev1.Volume{Name: name}
	items := []corev1.KeyToPath{{Key: reference.Key, Path: "value"}}
	if reference.Kind == "Secret" {
		volume.Secret = &corev1.SecretVolumeSource{SecretName: destination, Items: items, DefaultMode: &mode, Optional: &optional}
	} else {
		volume.ConfigMap = &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: destination}, Items: items, DefaultMode: &mode, Optional: &optional}
	}
	mount := corev1.VolumeMount{Name: name, MountPath: "/etc/prometheus/tls/" + name, ReadOnly: true}
	return FileMountRequirement{Source: reference, DestinationNamespace: namespace, DestinationName: destination, CopyRequired: copyRequired, File: mount.MountPath + "/value", Volume: volume, VolumeMount: mount, Jobs: []string{job}}
}

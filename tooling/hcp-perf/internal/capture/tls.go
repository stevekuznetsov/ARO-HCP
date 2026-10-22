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
	"errors"
	"fmt"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/scrape"
)

const tlsCredentialPolicy = "immutable-snapshot; no automatic rotation or expiry monitoring"

// Validate the exact generated shape rather than trusting arbitrary Kubernetes
// volumes, paths, optional projections, or permissions supplied in a bundle.
func validateTLSMounts(namespace string, mounts []scrape.TLSMountRequirement) ([]scrape.TLSMountRequirement, error) {
	if len(mounts) > 64 {
		return nil, errors.New("at most 64 TLS mount requirements are allowed")
	}
	var result []scrape.TLSMountRequirement
	seen := map[string]scrape.TLSReference{}
	for _, mount := range mounts {
		source := mount.Source
		if (source.Kind != "Secret" && source.Kind != "ConfigMap") || len(validation.IsDNS1123Label(source.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(source.Name)) != 0 || source.Key == "" || len(validation.IsConfigMapKey(source.Key)) != 0 {
			return nil, errors.New("TLS source must identify a valid Secret or ConfigMap namespace, name, and key")
		}
		// Start never adopts a namespace. Same-namespace sources cannot already
		// exist in the newly created session and must not be silently adopted.
		if source.Namespace == namespace || mount.DestinationNamespace != namespace || !mount.CopyRequired {
			return nil, errors.New("TLS mounts require cross-namespace snapshots into the new capture namespace")
		}
		digest := sha256.Sum256([]byte(source.Kind + "/" + source.Namespace + "/" + source.Name + "/" + source.Key))
		name := fmt.Sprintf("hcp-perf-tls-%x", digest[:16])
		volume := corev1.Volume{Name: name}
		items := []corev1.KeyToPath{{Key: source.Key, Path: "value"}}
		if source.Kind == "Secret" {
			volume.Secret = &corev1.SecretVolumeSource{SecretName: name, Items: items, DefaultMode: ptr(int32(0440)), Optional: ptr(false)}
		} else {
			volume.ConfigMap = &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Items: items, DefaultMode: ptr(int32(0440)), Optional: ptr(false)}
		}
		vm := corev1.VolumeMount{Name: name, MountPath: "/etc/prometheus/tls/" + name, ReadOnly: true}
		if mount.DestinationName != name || mount.File != vm.MountPath+"/value" || !reflect.DeepEqual(mount.Volume, volume) || !reflect.DeepEqual(mount.VolumeMount, vm) {
			return nil, errors.New("TLS destination and projection must match the generated hashed name, read-only path, selected key, and 0440 mode")
		}
		if previous, ok := seen[name]; ok {
			if previous != source {
				return nil, errors.New("TLS destination name collision")
			}
			continue
		}
		seen[name] = source
		// Jobs are generator diagnostics, not needed to materialize credentials.
		mount.Jobs = nil
		mount.Volume, mount.VolumeMount = volume, vm
		result = append(result, mount)
	}
	return result, nil
}

// Read each source object once, retaining only the requested keys. Kubernetes
// GET returns the whole object; no unrelated keys are copied or serialized.
func readTLS(ctx context.Context, client kubernetes.Interface, mounts []scrape.TLSMountRequirement) ([][]byte, error) {
	result := make([][]byte, len(mounts))
	total := 0
	for i, mount := range mounts {
		if result[i] != nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		source := mount.Source
		var data map[string][]byte
		var text map[string]string
		if source.Kind == "Secret" {
			object, err := client.CoreV1().Secrets(source.Namespace).Get(ctx, source.Name, metav1.GetOptions{})
			if err != nil {
				return nil, operationError("get TLS source Secret", err)
			}
			data = object.Data
		} else {
			object, err := client.CoreV1().ConfigMaps(source.Namespace).Get(ctx, source.Name, metav1.GetOptions{})
			if err != nil {
				return nil, operationError("get TLS source ConfigMap", err)
			}
			data, text = object.BinaryData, object.Data
		}
		for j := i; j < len(mounts); j++ {
			ref := mounts[j].Source
			if ref.Kind != source.Kind || ref.Namespace != source.Namespace || ref.Name != source.Name {
				continue
			}
			value, binary := data[ref.Key]
			str, plain := text[ref.Key]
			if binary && plain {
				return nil, errors.New("TLS source key occurs in both data and binaryData")
			}
			if plain {
				value = []byte(str)
			}
			if len(value) == 0 {
				return nil, errors.New("required TLS source key is missing or empty")
			}
			if len(value)+len(ref.Key) > 1024*1024 {
				return nil, errors.New("TLS snapshot exceeds the 1 MiB object data limit")
			}
			total += len(value)
			if total > 16*1024*1024 {
				return nil, errors.New("TLS snapshots exceed the 16 MiB aggregate limit")
			}
			result[j] = append([]byte(nil), value...)
		}
	}
	return result, nil
}

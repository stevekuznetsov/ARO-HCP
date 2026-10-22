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
	"errors"
	"regexp"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var clusterVersions = schema.GroupVersionResource{Group: "config.openshift.io", Version: "v1", Resource: "clusterversions"}
var clusterUUID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// OpenShift config/v1.ClusterVersionSpec defines the required clusterID UUID.
// Status, kubeconfig context names and API endpoint names are not identity proof.
func verifyGuestIdentity(ctx context.Context, guest dynamic.Interface, expected string) error {
	if guest == nil || !clusterUUID.MatchString(expected) || expected == "00000000-0000-0000-0000-000000000000" {
		return errors.New("discovered clusterID must be a nonzero UUID")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	version, err := guest.Resource(clusterVersions).Get(ctx, "version", metav1.GetOptions{})
	if err != nil {
		return errors.New("guest ClusterVersion identity unavailable")
	}
	actual, found, err := unstructured.NestedString(version.Object, "spec", "clusterID")
	if err != nil || !found || version.GetDeletionTimestamp() != nil || !clusterUUID.MatchString(actual) || !strings.EqualFold(actual, expected) {
		return errors.New("guest ClusterVersion clusterID does not match discovered HCP")
	}
	return nil
}

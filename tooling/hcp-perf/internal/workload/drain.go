// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package workload

import (
	"context"
	"errors"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// WaitCanaryDrained is additional read-only recovery evidence, never a rewrite
// of a failed cycle. Scale-down must already be accepted. The caller supplies
// the same scoped identity and original Deployment UID; no admin repair/retry.
func WaitCanaryDrained(ctx context.Context, client kubernetes.Interface, spec Spec, uid types.UID) (Snapshot, error) {
	if uid == "" {
		return Snapshot{}, errors.New("canary recovery requires original UID")
	}
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	setup, stop := context.WithTimeout(ctx, 30*time.Second)
	w, err := newCanaryWatch(setup, client, spec, nil)
	stop()
	if err != nil {
		return Snapshot{}, err
	}
	defer w.stop()
	d := w.deployment
	if d.UID != uid || d.DeletionTimestamp != nil || d.Spec.Replicas == nil || *d.Spec.Replicas != 0 || d.Spec.Paused {
		return Snapshot{}, errors.New("canary recovery requires same live Deployment already scaled to zero")
	}
	selector, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil || selector.Empty() || !selector.Matches(labels.Set(d.Spec.Template.Labels)) {
		return Snapshot{}, errors.New("invalid canary recovery selector")
	}
	w.uid, w.selector = uid, selector
	// A selected Pod whose ReplicaSet disappeared is not proof of zero work.
	// Keep unknown/replaced owner chains in the drain snapshot until removed.
	w.conservativeDrain = true
	w.arm(d.Generation, 0)
	w.start()
	snapshot, _, err := w.wait(ctx)
	return snapshot, err
}

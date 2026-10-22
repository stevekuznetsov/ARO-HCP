// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package startup

import (
	"context"
	"errors"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NoMutationError means Apply returned before issuing its non-dry-run Update.
// It says nothing about another actor's changes to the same resource.
type NoMutationError struct{ Err error }

func (e *NoMutationError) Error() string { return e.Err.Error() }
func (e *NoMutationError) Unwrap() error { return e.Err }

// Apply never retries an update. Dry-run and real update carry the same UID/RV;
// a concurrent writer yields Conflict rather than an overwrite. The empty-HC
// checks are not atomic with creation: the personal environment needs one writer.
func Apply(ctx context.Context, clients Clients, candidate Candidate) (retErr error) {
	return apply(ctx, clients, candidate, nil)
}

// ApplyExisting is explicitly restricted to one pinned personal-environment HCP.
// The inventory checks and update are not atomic; an exclusive writer is required.
func ApplyExisting(ctx context.Context, clients Clients, candidate Candidate, identity Identity) error {
	if identity.HCUID == "" || identity.HCPUID == "" || identity.NamespaceUID == "" || identity.ResourceID == "" || candidate.EmptyHostedClusterSelector != "" {
		return &NoMutationError{Err: errors.New("existing candidate update requires complete identity and unfiltered inventory")}
	}
	return apply(ctx, clients, candidate, &identity)
}

func apply(ctx context.Context, clients Clients, candidate Candidate, identity *Identity) (retErr error) {
	updateSent := false
	defer func() {
		if retErr != nil && !updateSent {
			retErr = &NoMutationError{Err: retErr}
		}
	}()
	if err := candidate.Validate(); err != nil {
		return err
	}
	resource := clients.Dynamic.Resource(SizingConfigurations)
	current, err := resource.Get(ctx, candidate.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	expected, err := DecodeCandidateSpec(candidate.ExpectedSpec)
	if err != nil {
		return err
	}
	desired, err := DecodeCandidateSpec(candidate.DesiredSpec)
	if err != nil {
		return err
	}
	if string(current.GetUID()) != candidate.UID || current.GetResourceVersion() != candidate.ResourceVersion || !candidateSpecEqual(current.Object["spec"], expected) {
		return errors.New("candidate UID/resourceVersion/spec precondition mismatch")
	}
	ensureEmpty := func() error {
		clusters, err := clients.Dynamic.Resource(HostedClusters).List(ctx, metav1.ListOptions{})
		if err != nil {
			return err
		}
		if identity != nil {
			if len(clusters.Items) != 1 || string(clusters.Items[0].GetUID()) != identity.HCUID {
				return errors.New("existing candidate update requires exactly the pinned HostedCluster")
			}
			observed, err := Discover(ctx, clients, identity.ResourceID, *identity)
			if err != nil {
				return err
			}
			if observed != *identity {
				return errors.New("existing candidate target identity changed")
			}
			return nil
		}
		if len(clusters.Items) != 0 {
			return errors.New("refusing candidate update with selected HostedClusters present")
		}
		return nil
	}
	if err := ensureEmpty(); err != nil {
		return err
	}
	current.Object["spec"] = desired
	dryRun, err := resource.Update(ctx, current.DeepCopy(), metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}})
	if err != nil {
		return err
	}
	if string(dryRun.GetUID()) != candidate.UID || !candidateSpecEqual(dryRun.Object["spec"], desired) {
		return errors.New("candidate dry-run changed identity or desired spec")
	}
	if err := ensureEmpty(); err != nil {
		return err
	}
	updateSent = true
	if _, err := resource.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
		return err
	}
	observed, err := resource.Get(ctx, candidate.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if string(observed.GetUID()) != candidate.UID || !candidateSpecEqual(observed.Object["spec"], desired) {
		return errors.New("candidate verification mismatch after update")
	}
	return nil
}

// VerifyUnchanged performs one current GET, never Update. Exact UID and original
// RV are essential: matching spec alone cannot rule out a committed update.
func VerifyUnchanged(ctx context.Context, clients Clients, candidate Candidate) error {
	if err := candidate.Validate(); err != nil {
		return err
	}
	expected, err := DecodeCandidateSpec(candidate.ExpectedSpec)
	if err != nil {
		return err
	}
	current, err := clients.Dynamic.Resource(SizingConfigurations).Get(ctx, candidate.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if string(current.GetUID()) != candidate.UID || current.GetResourceVersion() != candidate.ResourceVersion || !candidateSpecEqual(current.Object["spec"], expected) {
		return errors.New("candidate no longer matches original UID/resourceVersion/spec")
	}
	return nil
}

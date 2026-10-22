// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package iteration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/cost"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/metrics"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/performance"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/provision"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/retention"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/sizing"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/startup"
)

var safeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// Ordered references keep the lifecycle legible and make cycles impossible.
func (s Spec) Validate() error {
	if s.Version != Version || !safeName.MatchString(s.Environment.ID) || !safeName.MatchString(s.Environment.Incarnation) || len(s.Actions) == 0 {
		return errors.New("require version v1, safe environment ID/incarnation and actions")
	}
	seen := map[string]bool{}
	for _, action := range s.Actions {
		if !safeName.MatchString(action.ID) || seen[action.ID] || action.Timeout <= 0 || action.MaxAttempts < 1 || action.MaxAttempts > 10 {
			return errors.New("actions require unique safe IDs, positive timeouts and maxAttempts in [1,10]")
		}
		for _, dependency := range action.DependsOn {
			if !seen[dependency.Action] || (dependency.Milestone != Started && dependency.Milestone != Completed) {
				return fmt.Errorf("action %q dependency must reference an earlier action with milestone started/completed", action.ID)
			}
		}
		seen[action.ID] = true
		if err := action.validate(); err != nil {
			return fmt.Errorf("action %q: %w", action.ID, err)
		}
	}
	for _, observer := range s.Actions {
		if observer.Kind != StartupObserve {
			continue
		}
		candidateReady := false
		for _, candidate := range s.Actions {
			if candidate.Kind != CandidateApply || !hasDependency(observer, candidate.ID, Completed) {
				continue
			}
			applied, appliedErr := startup.DecodeCandidateSpec(candidate.Candidate.DesiredSpec)
			observed, observedErr := startup.DecodeCandidateSpec(observer.Startup.CandidateSpec)
			if appliedErr != nil || observedErr != nil {
				return errors.New("invalid startup candidate specs")
			}
			if candidate.Candidate.Name == observer.Startup.CandidateName && candidate.Candidate.Kubeconfig == observer.Startup.Kubeconfig && reflect.DeepEqual(applied, observed) {
				candidateReady = true
			}
		}
		if !candidateReady {
			return errors.New("startup observer requires a completed matching candidate apply action")
		}
		matched := false
		for _, create := range s.Actions {
			if create.Kind != RPCreateCluster {
				continue
			}
			client, err := provision.New(create.RP.Config, nil)
			if err != nil {
				return err
			}
			request, err := client.ValidateActionInputs(create.RP.Resource, http.MethodPut, create.RP.Payload, "")
			if err != nil {
				return err
			}
			if !strings.EqualFold(request.ResourceID, observer.Startup.ResourceID) {
				continue
			}
			matched = true
			if !hasDependency(create, observer.ID, Started) {
				return errors.New("startup cluster creation must depend on its observer started milestone")
			}
			retentionReady := false
			for _, refresh := range s.Actions {
				if refresh.Kind == RetentionRefresh && hasDependency(create, refresh.ID, Started) && refresh.Retention.Duration >= create.Timeout {
					customerGroup := "/subscriptions/" + create.RP.Config.SubscriptionID + "/resourceGroups/" + create.RP.Resource.ResourceGroup
					for _, group := range refresh.Retention.Required.ResourceGroups {
						if strings.EqualFold(group, customerGroup) {
							retentionReady = true
						}
					}
				}
			}
			if !retentionReady {
				return errors.New("startup creation requires an armed retention action covering the create timeout")
			}
		}
		if !matched {
			return errors.New("startup observer requires an exact matching RP create-cluster action")
		}
	}
	for _, action := range s.Actions {
		if action.Kind != PerformanceRun {
			continue
		}
		for _, prerequisite := range s.Actions {
			if prerequisite.Kind == StartupObserve && strings.EqualFold(prerequisite.Startup.ResourceID, action.Performance.Startup.ResourceID) {
				if !hasDependency(action, prerequisite.ID, Completed) || !reflect.DeepEqual(prerequisite.Startup.Components, action.Performance.Startup.Components) {
					return errors.New("performance requires completed matching startup inventory when startup is in this iteration")
				}
			}
			if prerequisite.Kind == RPAdminCredential && prerequisite.RP.CredentialPath == action.Performance.GuestKubeconfig && !hasDependency(action, prerequisite.ID, Completed) {
				return errors.New("performance requires completed guest credential action")
			}
		}
	}
	return nil
}

func hasDependency(action Action, id string, milestone Milestone) bool {
	for _, dependency := range action.DependsOn {
		if dependency.Action == id && dependency.Milestone == milestone {
			return true
		}
	}
	return false
}

func (a Action) validate() error {
	if a.ObservationIdentity != nil && a.Kind != StartupObserveExisting && a.Kind != CandidateUpdate {
		return errors.New("observation identity only permitted for existing-target observation")
	}
	parameters := 0
	for _, present := range []bool{a.Sizing != nil, a.Cost != nil, a.RP != nil, a.Metrics != nil, a.Candidate != nil, a.Startup != nil, a.Retention != nil, a.Performance != nil, a.PerformanceCleanup != nil, a.CatalogPin != nil} {
		if present {
			parameters++
		}
	}
	if parameters != 1 {
		return errors.New("exactly one typed parameter family is required")
	}
	expected := ReadOnly
	switch a.Kind {
	case CatalogPin:
		expected = GuardedMutation
		if a.CatalogPin == nil || a.MaxAttempts != 1 {
			return errors.New("catalog pin requires one guarded attempt")
		}
		if err := a.CatalogPin.Validate(); err != nil {
			return err
		}
	case PerformanceCleanup:
		expected = GuardedMutation
		if a.PerformanceCleanup == nil || a.MaxAttempts != 1 {
			return errors.New("cleanup requires parameters and one attempt")
		}
		if err := a.PerformanceCleanup.Validate(); err != nil {
			return err
		}
	case PerformanceRun:
		expected = Measurement
		if a.Performance == nil || a.MaxAttempts != performance.MaxAttempts {
			return errors.New("performance parameters and maxAttempts=1 required")
		}
		if err := a.Performance.Validate(); err != nil {
			return err
		}
		if a.Timeout <= a.Performance.MaxDuration()+5*time.Minute {
			return errors.New("performance timeout must exceed run duration plus five-minute finalization")
		}
	case CandidateApply, CandidateUpdate:
		expected = GuardedMutation
		if a.Candidate == nil {
			return errors.New("candidate parameters required")
		}
		if err := a.Candidate.Validate(); err != nil {
			return err
		}
		if a.Kind == CandidateUpdate && (a.MaxAttempts != 1 || a.ObservationIdentity == nil || a.ObservationIdentity.ResourceID == "" || a.ObservationIdentity.HCUID == "" || a.ObservationIdentity.HCPUID == "" || a.ObservationIdentity.NamespaceUID == "" || a.Candidate.EmptyHostedClusterSelector != "") {
			return errors.New("candidate-update requires one attempt, exact existing identity and unfiltered inventory")
		}
	case StartupObserve, StartupObserveExisting:
		expected = Measurement
		if a.Startup == nil {
			return errors.New("startup parameters required")
		}
		if err := a.Startup.Validate(); err != nil {
			return err
		}
		if a.Kind == StartupObserveExisting && (a.ObservationIdentity == nil || a.ObservationIdentity.HCUID == "" || a.ObservationIdentity.HCPUID == "" || a.ObservationIdentity.NamespaceUID == "" || a.ObservationIdentity.ResourceID != a.Startup.ResourceID) {
			return errors.New("existing-target observation requires exact pinned identity")
		}
		if a.Timeout <= a.Startup.MaxDuration {
			return errors.New("startup action timeout must exceed observation duration for finalization")
		}
	case RetentionRefresh:
		expected = GuardedMutation
		if a.Retention == nil {
			return errors.New("retention parameters required")
		}
		if err := a.Retention.Validate(); err != nil {
			return err
		}
		if a.Timeout <= a.Retention.Duration {
			return errors.New("retention timeout must exceed refresh duration")
		}
	case SizingResolve:
		if a.Sizing == nil {
			return errors.New("sizing parameters required")
		}
		resolved, err := sizing.Resolve(a.Sizing.Profile)
		if err != nil {
			return err
		}
		if a.Sizing.HyperShiftDefaults != nil {
			_, err = sizing.ToHyperShift(resolved, *a.Sizing.HyperShiftDefaults)
			if err != nil {
				return err
			}
		}
	case CostEstimate:
		if a.Cost == nil {
			return errors.New("cost parameters required")
		}
		if _, err := cost.Calculate(*a.Cost); err != nil {
			return err
		}
	case RPCreateCluster, RPCreateNodePool, RPUpdateClusterImage, RPUpdateNodePool, RPInspect, RPWait, RPDelete, RPAdminCredential:
		if a.RP == nil {
			return errors.New("RP parameters required")
		}
		client, err := provision.New(a.RP.Config, nil)
		if err != nil {
			return err
		}
		if a.Kind != RPUpdateClusterImage && (a.RP.Image != "" || a.RP.TargetIdentity != nil || a.RP.ManagementKubeconfig != "" || a.RP.AllowExecCredential) {
			return errors.New("image update guards only permitted for image update action")
		}
		if a.Kind != RPUpdateClusterImage && a.Kind != RPUpdateNodePool && len(a.RP.ExpectedResource) != 0 {
			return errors.New("expectedResource only permitted for guarded RP updates")
		}
		if a.Kind != RPUpdateNodePool && a.RP.Replicas != nil {
			return errors.New("replicas only permitted for node pool update")
		}
		if a.Kind == RPUpdateNodePool {
			if a.RP.Replicas == nil || len(a.RP.Payload) != 0 || a.RP.ConfirmDelete {
				return errors.New("node pool update requires typed replicas and no payload or delete confirmation")
			}
			if _, err := client.ValidateNodePoolUpdate(a.RP.Resource, a.RP.ExpectedResource, *a.RP.Replicas); err != nil {
				return err
			}
		}
		if a.Kind == RPUpdateClusterImage {
			if !filepath.IsAbs(a.RP.ManagementKubeconfig) || a.RP.TargetIdentity == nil || a.RP.TargetIdentity.HCUID == "" || a.RP.TargetIdentity.HCPUID == "" || a.RP.TargetIdentity.NamespaceUID == "" {
				return errors.New("image update requires pinned HCP identity and management credential")
			}
			if a.RP.Resource.NodePool != "" {
				return errors.New("image update requires cluster reference")
			}
			payload, err := provision.ImageUpdatePayload(a.RP.ExpectedResource, a.RP.Image)
			if err != nil {
				return err
			}
			var expected, actual any
			if json.Unmarshal(payload, &expected) != nil || json.Unmarshal(a.RP.Payload, &actual) != nil || !reflect.DeepEqual(expected, actual) {
				return errors.New("image update payload must change only canonical image tag")
			}
		} else if a.Kind == RPCreateCluster || a.Kind == RPCreateNodePool {
			if (a.Kind == RPCreateNodePool) != (a.RP.Resource.NodePool != "") {
				return errors.New("RP creation kind does not match node pool reference")
			}
		} else if len(a.RP.Payload) != 0 {
			return errors.New("payload only permitted for RP creation")
		}
		if a.Kind == RPWait && a.RP.OperationURL == "" {
			return errors.New("RP wait requires an operation URL")
		}
		if a.Kind != RPWait && a.RP.OperationURL != "" {
			return errors.New("operation URL only permitted for RP wait")
		}
		if a.Kind == RPDelete && !a.RP.ConfirmDelete {
			return errors.New("RP deletion requires confirmDelete")
		}
		if a.Kind == RPCreateCluster || a.Kind == RPCreateNodePool || a.Kind == RPUpdateClusterImage || a.Kind == RPUpdateNodePool || a.Kind == RPDelete {
			expected = RemoteMutation
		}
		if a.Kind == RPAdminCredential {
			expected = GuardedMutation
			if !filepath.IsAbs(a.RP.CredentialPath) || a.RP.Resource.NodePool != "" {
				return errors.New("admin credential requires a cluster and absolute external output path")
			}
		} else if a.RP.CredentialPath != "" {
			return errors.New("credentialPath only permitted for admin credential action")
		}
		if _, err := client.ValidateActionInputs(a.RP.Resource, a.requestMethod(), a.RP.Payload, a.RP.OperationURL); err != nil {
			return err
		}
	case MetricsExport:
		expected = Measurement
		if a.Metrics == nil {
			return errors.New("metrics parameters required")
		}
		m := a.Metrics
		u, err := url.Parse(m.Endpoint)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("metrics endpoint must not contain credentials, query or fragment")
		}
		if len(m.Queries) == 0 || m.Start.IsZero() || m.End.Before(m.Start) || m.Step < time.Second {
			return errors.New("metrics queries and a bounded range with step >= 1s required")
		}
	default:
		return fmt.Errorf("unsupported action kind %q", a.Kind)
	}
	if a.Recovery != expected {
		return fmt.Errorf("kind %q requires recovery %q", a.Kind, expected)
	}
	return nil
}

type libraryExecutor struct{ client *http.Client }

type readyContextKey struct{}
type observationContextKey struct{}

func markReady(ctx context.Context) error {
	if ready, ok := ctx.Value(readyContextKey{}).(func() error); ok {
		return ready()
	}
	return nil
}

func (a Action) requestMethod() string {
	switch a.Kind {
	case RPCreateCluster, RPCreateNodePool, RPUpdateClusterImage:
		return http.MethodPut
	case RPUpdateNodePool:
		return http.MethodPatch
	case RPDelete:
		return http.MethodDelete
	default:
		return http.MethodGet
	}
}

func (e libraryExecutor) Execute(ctx context.Context, action Action, outputDir, resumeURL string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var result any
	var err error
	switch action.Kind {
	case PerformanceCleanup:
		err = performance.Cleanup(ctx, *action.PerformanceCleanup, filepath.Join(outputDir, "cleanup"))
		result = map[string]string{"state": "deleted-verified"}
	case PerformanceRun:
		if resumeURL != "" {
			return errors.New("performance cannot resume")
		}
		result, err = performance.Run(ctx, *action.Performance, filepath.Join(outputDir, "performance"), e.client)
	case CatalogPin:
		clients, clientErr := startup.NewClients(action.CatalogPin.Kubeconfig, action.CatalogPin.AllowExecCredential)
		if clientErr != nil {
			return &startup.NoMutationError{Err: clientErr}
		}
		err = startup.PinCatalogs(ctx, clients, *action.CatalogPin)
		result = map[string]string{"status": "HC catalog annotations verified; downstream convergence requires observation"}
	case CandidateApply, CandidateUpdate:
		clients, clientErr := startup.NewClients(action.Candidate.Kubeconfig, action.Candidate.AllowExecCredential)
		if clientErr != nil {
			return &startup.NoMutationError{Err: clientErr}
		}
		if action.Kind == CandidateUpdate {
			err = startup.ApplyExisting(ctx, clients, *action.Candidate, *action.ObservationIdentity)
		} else {
			err = startup.Apply(ctx, clients, *action.Candidate)
		}
		result = map[string]string{"candidate": action.Candidate.Name, "status": "verified"}
	case StartupObserve, StartupObserveExisting:
		clients, clientErr := startup.NewClients(action.Startup.Kubeconfig, action.Startup.AllowExecCredential)
		if clientErr != nil {
			return clientErr
		}
		if revision, ok := ctx.Value(observationContextKey{}).(*ObservationRevision); ok {
			revised, revisionErr := revisedObservation(*action.Startup, revision.ImageBindings)
			if revisionErr != nil {
				return revisionErr
			}
			result, err = startup.ObserveContinuation(ctx, clients, revised, outputDir, revision.Identity, func() error { return markReady(ctx) })
		} else if action.Kind == StartupObserveExisting {
			identity, identityErr := startup.Discover(ctx, clients, action.Startup.ResourceID, *action.ObservationIdentity)
			if identityErr != nil || identity != *action.ObservationIdentity {
				return errors.New("existing-target observation identity mismatch")
			}
			result, err = startup.ObserveContinuation(ctx, clients, *action.Startup, outputDir, identity, func() error { return markReady(ctx) })
		} else {
			result, err = startup.Observe(ctx, clients, *action.Startup, outputDir, func() error { return markReady(ctx) })
		}
	case RetentionRefresh:
		return e.refreshRetention(ctx, *action.Retention, outputDir)
	case SizingResolve:
		resolved, resolveErr := sizing.Resolve(action.Sizing.Profile)
		if resolveErr != nil {
			return fmt.Errorf("resolve sizing: %w", resolveErr)
		}
		result = resolved
		if action.Sizing.HyperShiftDefaults != nil {
			result, err = sizing.ToHyperShift(resolved, *action.Sizing.HyperShiftDefaults)
		}
	case CostEstimate:
		result, err = cost.Calculate(*action.Cost)
	case MetricsExport:
		m := action.Metrics
		result, err = metrics.Export(ctx, e.client, m.Endpoint, m.Queries, m.Start, m.End, m.Step, filepath.Join(outputDir, "metrics"))
	default:
		p := action.RP
		client, clientErr := provision.New(p.Config, e.client)
		if clientErr != nil {
			return fmt.Errorf("configure RP client: %w", clientErr)
		}
		var operation provision.Result
		dir := filepath.Join(outputDir, "rp")
		if resumeURL != "" {
			operation, err = client.Resume(ctx, p.Resource, resumeURL, dir)
		} else {
			switch action.Kind {
			case RPCreateCluster:
				operation, err = client.CreateCluster(ctx, p.Resource, p.Payload, dir)
			case RPUpdateClusterImage:
				management, clientErr := startup.NewClients(p.ManagementKubeconfig, p.AllowExecCredential)
				if clientErr != nil {
					return clientErr
				}
				identity, identityErr := startup.Discover(ctx, management, p.TargetIdentity.ResourceID, *p.TargetIdentity)
				if identityErr != nil || identity != *p.TargetIdentity {
					return errors.New("image update HCP identity guard failed")
				}
				request, identityErr := client.ValidateActionInputs(p.Resource, http.MethodGet, nil, "")
				if identityErr != nil || !strings.EqualFold(request.ResourceID, identity.ResourceID) {
					return errors.New("image update ARM identity guard failed")
				}
				operation, err = client.UpdateClusterImage(ctx, p.Resource, p.ExpectedResource, p.Image, dir)
			case RPCreateNodePool:
				operation, err = client.CreateNodePool(ctx, p.Resource, p.Payload, dir)
			case RPUpdateNodePool:
				operation, err = client.UpdateNodePool(ctx, p.Resource, p.ExpectedResource, *p.Replicas, dir)
			case RPInspect:
				operation, err = client.Inspect(ctx, p.Resource, dir)
			case RPWait:
				operation, err = client.Resume(ctx, p.Resource, p.OperationURL, dir)
			case RPDelete:
				operation, err = client.Delete(ctx, p.Resource, dir, p.ConfirmDelete)
			case RPAdminCredential:
				parent, statErr := os.Stat(filepath.Dir(p.CredentialPath))
				if statErr != nil || !parent.IsDir() || parent.Mode().Perm()&0077 != 0 {
					return errors.New("credential output parent must be an existing private directory")
				}
				operation, err = client.RequestAdminCredential(ctx, p.Resource, dir, p.CredentialPath)
			default:
				return errors.New("unsupported library action")
			}
		}
		result = operation
		var operationFailed *provision.OperationFailedError
		if errors.As(err, &operationFailed) {
			// Preserve a separate resource snapshot when possible; its state does
			// not establish or invalidate the operation endpoint's terminal result.
			_, _ = client.Inspect(ctx, p.Resource, filepath.Join(outputDir, "verify"))
		}
		if err == nil && action.Recovery == RemoteMutation {
			// A generic Resume accepts both existence and absence. This action must
			// prove its own desired state, not just a successful operation status.
			observed, inspectErr := client.Inspect(ctx, p.Resource, filepath.Join(outputDir, "verify"))
			if action.Kind == RPDelete {
				if observed.HTTPStatus != http.StatusNotFound {
					err = errors.New("delete completion requires resource HTTP 404")
				}
			} else if inspectErr != nil || observed.HTTPStatus != http.StatusOK || observed.FinalState != "Succeeded" {
				err = errors.New("create completion requires an existing Succeeded resource")
			}
			if err == nil && action.Kind == RPUpdateClusterImage {
				err = client.VerifyClusterImage(ctx, p.Resource, p.Image, filepath.Join(outputDir, "verify-image"))
			}
			if err == nil && action.Kind == RPUpdateNodePool {
				err = client.VerifyNodePoolUpdate(ctx, p.Resource, p.ExpectedResource, *p.Replicas, filepath.Join(outputDir, "verify-replicas"))
			}
		}
	}
	if err != nil {
		return fmt.Errorf("execute %s: %w", action.Kind, err)
	}
	data, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode action result: %w", err)
	}
	return writePrivate(filepath.Join(outputDir, "result.json"), data, 0600)
}

func (e libraryExecutor) refreshRetention(ctx context.Context, spec retention.NativeSpec, outputDir string) error {
	journal, err := os.OpenFile(filepath.Join(outputDir, "retention.ndjson"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer journal.Close()
	end := time.NewTimer(spec.Duration)
	defer end.Stop()
	ticker := time.NewTicker(spec.Interval)
	defer ticker.Stop()
	for {
		result, refreshErr := retention.RefreshNative(ctx, spec, e.client)
		spec = requireObservedGroups(spec, result)
		checkpoint := struct {
			retention.Result
			RequiredResourceGroups []string `json:"requiredResourceGroups"`
		}{Result: result, RequiredResourceGroups: spec.Required.ResourceGroups}
		if err := json.NewEncoder(journal).Encode(checkpoint); err != nil {
			return err
		}
		if err := journal.Sync(); err != nil {
			return err
		}
		if refreshErr != nil {
			return refreshErr
		}
		if err := markReady(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-end.C:
			return nil
		case <-ticker.C:
		}
	}
}

// Once preflight established existence, a future 404 is a failure. The journal
// persists this promotion before readiness or another cycle; lost workers block
// rather than restarting with the original optional allowlist.
func requireObservedGroups(spec retention.NativeSpec, result retention.Result) retention.NativeSpec {
	spec.Required.ResourceGroups = append([]string{}, spec.Required.ResourceGroups...)
	remaining := []string{}
	for _, id := range spec.OptionalResourceGroups {
		observed := false
		for _, group := range result.ResourceGroups {
			if strings.EqualFold(group.ID, id) && group.OldStatus != "unknown" && group.Status != "not-created" {
				observed = true
			}
		}
		if observed {
			spec.Required.ResourceGroups = append(spec.Required.ResourceGroups, id)
		} else {
			remaining = append(remaining, id)
		}
	}
	spec.OptionalResourceGroups = remaining
	return spec
}

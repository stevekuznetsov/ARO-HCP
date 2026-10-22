// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package performance executes one bounded, non-resumable performance case.
// Population is retained. No sizing, node-pool, PVC or HCP mutation is supported.
package performance

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/adversary"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/apiload"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/metrics"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/seed"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/startup"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/workload"
)

const MaxAttempts = 1

type Spec struct {
	StabilityPolicy      string                    `json:"stabilityPolicy,omitempty"`
	RestorePopulation    bool                      `json:"restorePopulation,omitempty"`
	Unloaded             bool                      `json:"unloaded,omitempty"`
	ExistingPopulation   *PopulationReference      `json:"existingPopulation,omitempty"`
	Version              string                    `json:"version"`
	Startup              startup.Spec              `json:"startup"`
	GuestKubeconfig      string                    `json:"guestKubeconfig"`
	ExpectedGuestNodes   []GuestNodeIdentity       `json:"expectedGuestNodes"`
	Seed                 seed.Spec                 `json:"seed"`
	Admission            *workload.AdmissionPolicy `json:"admission,omitempty"`
	MeasurementSeconds   int64                     `json:"measurementSeconds"`
	RecoverySeconds      int64                     `json:"recoverySeconds"`
	SetupSeconds         int64                     `json:"setupSeconds"`
	CanaryCadenceSeconds int64                     `json:"canaryCadenceSeconds"`
	MaxOutstanding       int                       `json:"maxOutstanding"`
	ScheduleSeed         int64                     `json:"scheduleSeed"`
	API                  *apiload.Spec             `json:"api,omitempty"`
	RequestQualityPolicy *RequestQualityPolicy     `json:"requestQualityPolicy,omitempty"`
	Capture              Capture                   `json:"capture"`
	Competitor           *Competitor               `json:"competitor,omitempty"`
}

type GuestNodeIdentity struct {
	Name string `json:"name"`
	UID  string `json:"uid"`
}

func (s Spec) validateGuestNodes() error {
	// Bound the reviewed inventory and per-poll work, not a capacity guarantee.
	if len(s.ExpectedGuestNodes) < 3 || len(s.ExpectedGuestNodes) > 64 || int64(len(s.ExpectedGuestNodes)) != s.Seed.GuestNodes {
		return errors.New("expectedGuestNodes requires 3..64 identities matching seed.guestNodes")
	}
	uids := map[string]bool{}
	for i, node := range s.ExpectedGuestNodes {
		if len(validation.IsDNS1123Subdomain(node.Name)) != 0 || node.UID == "" || len(node.UID) > 128 || strings.ContainsAny(node.UID, " \t\r\n\x00") || uids[node.UID] || (i > 0 && s.ExpectedGuestNodes[i-1].Name >= node.Name) {
			return errors.New("expectedGuestNodes requires valid sorted unique names and nonempty unique UIDs (at most 128 bytes)")
		}
		uids[node.UID] = true
	}
	return nil
}

// RequestQualityPolicy uses an inclusive rational threshold over logical
// dispatched requests. It is opt-in; omission preserves strict timeout behavior.
type RequestQualityPolicy struct {
	MaxDegradedBasisPoints   int   `json:"maxDegradedBasisPoints"`
	ScreeningSeconds         []int `json:"screeningSeconds,omitempty"`
	MinimumCompletedRequests int   `json:"minimumCompletedRequests,omitempty"`
}

// Capture is an externally owned, already-running Go capture source. No source
// credentials, TLS keys, tokens or authority are embedded or exported here.
type Capture struct {
	// Required role -> exact scrape job bindings for observe-degradation.
	SourceRoles         map[string]string    `json:"sourceRoles,omitempty"`
	CriticalJobs        []string             `json:"criticalJobs,omitempty"`
	Endpoint            string               `json:"endpoint"`
	Reference           string               `json:"reference"`
	MaxAgeSeconds       int64                `json:"maxAgeSeconds"`
	Sources             []Source             `json:"sources"`
	Queries             []metrics.Query      `json:"queries"`
	JobTiming           map[string]JobTiming `json:"jobTiming,omitempty"`
	LossGraceSeconds    int64                `json:"lossGraceSeconds,omitempty"`
	RecoveryGateSeconds int64                `json:"recoveryGateSeconds,omitempty"`
}

// JobTiming declares the external owner's scrape policy, not a change to it.
// Durations are whole-second nanosecond values in inline JSON, like startup.
type JobTiming struct {
	Interval time.Duration `json:"intervalNanoseconds"`
	Timeout  time.Duration `json:"timeoutNanoseconds"`
	MaxAge   time.Duration `json:"maxAgeNanoseconds"`
}

func (c Capture) maxAge(job string) time.Duration {
	if timing, ok := c.JobTiming[job]; ok {
		return timing.MaxAge
	}
	if c.MaxAgeSeconds == 0 {
		return 45 * time.Second
	}
	return time.Duration(c.MaxAgeSeconds) * time.Second
}

type Source struct {
	Job      string `json:"job"`
	Replicas int    `json:"replicas"`
}

type Competitor struct {
	Run adversary.RunSpec `json:"run"`
	// Expected placement/request allocations are reviewed inputs, not an
	// auto-approved copy of discovery. TargetNamespace is bound at runtime.
	Nodes  []adversary.NodePlan `json:"nodes"`
	CPUJob string               `json:"cpuJob"` // exact external cAdvisor scrape job
}

func (s Spec) setup() workload.SetupSpec {
	return workload.SetupSpec{Version: "v1", ID: s.Seed.ID, NamespacePrefix: s.Seed.NamespacePrefix, GuestNodes: s.Seed.GuestNodes, Applications: s.Seed.Applications, Image: s.Seed.Image, Admission: s.Admission}
}

// MaxDuration includes setup, steady admission, no-new-load recovery, and hard
// bounds for a straddling canary plus the final recovery probe. Finalization is
// independent and needs another five minutes in the action bound.
func (s Spec) MaxDuration() time.Duration {
	return time.Duration(s.SetupSeconds+s.MeasurementSeconds+s.RecoverySeconds+s.Capture.RecoveryGateSeconds+540) * time.Second
}

// ValidateRun admits new measurements. Validate also accepts the persisted
// three-node contract so controllers can reconcile its original frozen worker
// and cleanup can still validate the original source without inventing UIDs.
func (s Spec) ValidateRun() error {
	if err := s.Validate(); err != nil {
		return err
	}
	return s.validateGuestNodes()
}

func (s Spec) Validate() error {
	if s.StabilityPolicy != "" && s.StabilityPolicy != "strict" && s.StabilityPolicy != "observe-degradation" {
		return errors.New("stabilityPolicy must be strict or observe-degradation")
	}
	if s.StabilityPolicy == "observe-degradation" {
		if s.Capture.LossGraceSeconds == 0 || len(s.Capture.CriticalJobs) == 0 || s.Startup.PollInterval > 5*time.Second {
			return errors.New("observe-degradation requires capture history, explicit criticalJobs and safety poll <=5s")
		}
		if s.RequestQualityPolicy != nil && len(s.RequestQualityPolicy.ScreeningSeconds) != 0 {
			return errors.New("observe-degradation is full-window observation; omit early request screening")
		}
	} else if len(s.Capture.CriticalJobs) != 0 {
		return errors.New("criticalJobs is only permitted with observe-degradation")
	}
	if s.RestorePopulation && (s.Unloaded || s.ExistingPopulation == nil || s.Competitor != nil) {
		return errors.New("restorePopulation requires an existing unloaded quiet population")
	}
	if s.Unloaded && (s.ExistingPopulation == nil || s.Competitor != nil) {
		return errors.New("unloaded comparison requires a verified existing quiet population")
	}
	if s.RequestQualityPolicy != nil && (s.API == nil || s.RequestQualityPolicy.MaxDegradedBasisPoints < 0 || s.RequestQualityPolicy.MaxDegradedBasisPoints > 10000) {
		return errors.New("requestQualityPolicy requires API load and maxDegradedBasisPoints in [0,10000]")
	}
	if policy := s.RequestQualityPolicy; policy != nil {
		if len(policy.ScreeningSeconds) > 10 || (len(policy.ScreeningSeconds) == 0 && policy.MinimumCompletedRequests != 0) || (len(policy.ScreeningSeconds) != 0 && (policy.MinimumCompletedRequests < 100 || policy.MinimumCompletedRequests > apiload.MaxAttempts)) {
			return errors.New("screening requires 1..10 checkpoints and minimumCompletedRequests in [100,1000000], or both omitted")
		}
		for i, seconds := range policy.ScreeningSeconds {
			if seconds < 60 || int64(seconds) >= s.MeasurementSeconds || (i > 0 && seconds <= policy.ScreeningSeconds[i-1]) || seconds*s.API.RatePerSecond < policy.MinimumCompletedRequests {
				return errors.New("screening checkpoints require ordered unique seconds >=60 inside measurement with enough intended requests")
			}
		}
	}
	if s.ExistingPopulation != nil && (!filepath.IsAbs(s.ExistingPopulation.Directory) || len(s.ExistingPopulation.OutcomeSHA256) != 64 || len(s.ExistingPopulation.InventorySHA256) != 64 || s.Competitor != nil) {
		return errors.New("population reuse requires absolute completed quiet case and outcome/inventory hashes")
	}
	if s.ExistingPopulation != nil {
		if err := s.ExistingPopulation.validateScreenedPopulation(); err != nil {
			return err
		}
		if s.ExistingPopulation.ScreenedPopulation != nil && (s.Unloaded || s.RestorePopulation) {
			return errors.New("screened population requires a loaded quiet population")
		}
		if err := validatePopulationImages(s.Startup, s.ExistingPopulation.ImageBindings); err != nil {
			return err
		}
	}
	if s.Version != "v1" || !filepath.IsAbs(s.GuestKubeconfig) || !filepath.IsAbs(s.Seed.Binary.Path) {
		return errors.New("performance requires v1 and absolute external credential/binary paths")
	}
	if err := s.Startup.Validate(); err != nil {
		return err
	}
	if err := s.Seed.Validate(); err != nil {
		return err
	}
	if err := s.setup().Validate(); err != nil {
		return err
	}
	if s.ExpectedGuestNodes == nil {
		if s.Seed.GuestNodes != 3 {
			return errors.New("legacy performance contract requires exactly three guest nodes")
		}
	} else if err := s.validateGuestNodes(); err != nil {
		return err
	}
	if s.MeasurementSeconds != 180 && s.MeasurementSeconds != 300 && s.MeasurementSeconds != 3600 {
		return errors.New("measurement must be 3m, 5m or 1h")
	}
	if s.RecoverySeconds < 30 || s.RecoverySeconds > 600 || s.SetupSeconds < 60 || s.SetupSeconds > 3600 || s.Seed.TimeoutSeconds > s.SetupSeconds || s.CanaryCadenceSeconds < 5 || s.CanaryCadenceSeconds > 300 || s.MaxOutstanding < 1 || s.MaxOutstanding > 100 {
		return errors.New("require recovery 30..600s, setup 60..3600s covering seed, cadence 5..300s and outstanding 1..100")
	}
	if s.API != nil {
		if _, err := s.API.Validate(); err != nil {
			return err
		}
		if s.API.DurationSeconds != int(s.MeasurementSeconds) {
			return errors.New("API duration must equal measurement")
		}
		allowed := map[string]bool{}
		for n := int64(0); n < (s.Seed.Applications+19)/20; n++ {
			allowed[fmt.Sprintf("%s-%d", s.Seed.NamespacePrefix, n)] = true
		}
		for _, ns := range s.API.Namespaces {
			if !allowed[ns] {
				return errors.New("API namespaces must belong to this seed")
			}
		}
		for _, target := range s.API.Targets {
			if target.Verb != "get" {
				continue
			}
			found := false
			for app := int64(0); app < s.Seed.Applications; app++ {
				ns, name := workload.TurnoverApplication(s.Seed.NamespacePrefix, app)
				if ns == target.Namespace && name == target.Name {
					found = true
					break
				}
			}
			if !found {
				return errors.New("API GET target must belong to this seed")
			}
		}
	}
	u, err := url.Parse(s.Capture.Endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1"))) || u.User != nil || u.RawQuery != "" || u.Fragment != "" || s.Capture.Reference == "" || (s.Capture.MaxAgeSeconds != 0 && (s.Capture.MaxAgeSeconds < 15 || s.Capture.MaxAgeSeconds > 60)) {
		return errors.New("capture requires HTTPS (or loopback HTTP), an evidence reference and default freshness 15..60s (zero selects 45s)")
	}
	if err := metrics.ValidateQueries(s.Capture.Queries); err != nil {
		return err
	}
	if len(s.Capture.Sources) == 0 || len(s.Capture.Sources) > 100 || len(s.Capture.Queries) > 100 {
		return errors.New("bounded nonempty capture sources/queries required")
	}
	seen := map[string]bool{}
	for _, source := range s.Capture.Sources {
		if source.Job == "" || seen[source.Job] || source.Replicas < 1 || source.Replicas > 100 {
			return errors.New("capture sources require unique jobs and explicit replica counts")
		}
		seen[source.Job] = true
	}
	for job, timing := range s.Capture.JobTiming {
		if !seen[job] {
			return fmt.Errorf("capture timing job %q is not an exact declared source", job)
		}
		if timing.Interval < 5*time.Second || timing.Interval > 5*time.Minute || timing.Timeout < time.Second || timing.Timeout > timing.Interval || timing.MaxAge < timing.Interval+timing.Timeout || timing.MaxAge > 10*time.Minute || timing.MaxAge > 2*timing.Interval+timing.Timeout || timing.Interval%time.Second != 0 || timing.Timeout%time.Second != 0 || timing.MaxAge%time.Second != 0 {
			return fmt.Errorf("capture timing for %q requires whole seconds: interval 5..300s, timeout 1s..interval, interval+timeout <= maxAge <= min(2*interval+timeout,600s)", job)
		}
	}
	critical := map[string]bool{}
	for _, job := range s.Capture.CriticalJobs {
		if !seen[job] || critical[job] {
			return errors.New("criticalJobs must be unique exact capture source jobs")
		}
		critical[job] = true
	}
	if s.StabilityPolicy == "observe-degradation" {
		roles := []string{"kas", "etcd", "container", "node", "kubelet"}
		if len(s.Capture.SourceRoles) != len(roles) {
			return errors.New("observe-degradation requires exactly kas/etcd/container/node/kubelet sourceRoles")
		}
		bound := map[string]bool{}
		for _, role := range roles {
			job := s.Capture.SourceRoles[role]
			if job == "" || !seen[job] || !critical[job] || bound[job] {
				return errors.New("each critical source role requires a distinct declared job included in criticalJobs")
			}
			bound[job] = true
		}
	} else if len(s.Capture.SourceRoles) != 0 {
		return errors.New("sourceRoles requires observe-degradation")
	}
	if s.Capture.LossGraceSeconds != 0 || s.Capture.RecoveryGateSeconds != 0 {
		if s.Capture.LossGraceSeconds < 60 || s.Capture.LossGraceSeconds > 180 || s.Capture.RecoveryGateSeconds < 120 || s.Capture.RecoveryGateSeconds > 180 || s.Capture.LossGraceSeconds > s.Capture.RecoveryGateSeconds {
			return errors.New("capture history requires lossGraceSeconds 60..180 and recoveryGateSeconds 120..180 covering grace")
		}
		for _, source := range s.Capture.Sources {
			if s.Capture.maxAge(source.Job) > time.Duration(s.Capture.RecoveryGateSeconds)*time.Second {
				return errors.New("capture recovery gate must cover every source maxAge")
			}
		}
	}
	if s.Competitor != nil {
		c := s.Competitor
		checked := c.Run
		checked.TargetNamespace = "runtime-bound-hcp"
		if err := checked.Validate(); err != nil {
			return err
		}
		if c.Run.ID != s.Seed.ID || c.Run.TargetNamespace != "" || len(c.Nodes) == 0 || c.CPUJob == "" || c.Run.DurationSeconds < s.MeasurementSeconds+s.RecoverySeconds+540 || c.Run.DurationSeconds > 7200 {
			return errors.New("competitor requires matching fresh ID, runtime-bound namespace, frozen nodes/CPU queries and sufficient bounded duration")
		}
		if len(validation.IsDNS1123Label(c.Run.Namespace)) != 0 {
			return errors.New("invalid competitor namespace")
		}
		for i, node := range c.Nodes {
			if i > 0 && c.Nodes[i-1].Name >= node.Name {
				return errors.New("competitor nodes must be unique and sorted")
			}
			if node.CPURequestMilli <= 0 || node.Workers <= 0 || node.MemoryRequestMi <= 0 || node.MemoryLimitMi < node.MemoryRequestMi || node.DeadlineSeconds != c.Run.DurationSeconds {
				return errors.New("competitor node allocation/CPU query mismatch")
			}
		}
	}
	return nil
}

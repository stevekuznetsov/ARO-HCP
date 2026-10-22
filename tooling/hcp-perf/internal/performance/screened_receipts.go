// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0.
package performance

import (
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/apiload"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/experiment"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/workload"
)

// Pins establish which receipts were reviewed; this checks that they describe
// one internally consistent native execution, not just independent valid JSON.
func screenedReceipts(s Spec, out Outcome, r Results, inventory []populationObject) error {
	if err := s.ValidateRun(); err != nil {
		return fmt.Errorf("screened source spec: %w", err)
	}
	if s.API == nil || s.RequestQualityPolicy == nil || len(s.RequestQualityPolicy.ScreeningSeconds) == 0 || out.GuardFailure != nil || out.CaptureReference != s.Capture.Reference || out.Identity.ResourceID != s.Startup.ResourceID {
		return errors.New("screened source policy, capture or guard contradiction")
	}
	a := r.API
	apiSpec, err := s.API.Validate()
	if err != nil {
		return err
	}
	if a == nil || a.Version != "v1" || a.RequestAccountingVersion != apiload.RequestAccountingVersion || a.Status != "canceled" || !reflect.DeepEqual(a.Spec, apiSpec) || a.IntendedQPS != apiSpec.RatePerSecond || a.Started.Before(out.Started) || a.SchedulingStopped.Before(a.Started) || a.Finished.Before(a.SchedulingStopped) || a.Finished.After(out.Finished) {
		return errors.New("screened API receipt does not match source execution")
	}
	if !screenedCounts(a.Counts) || a.Completed != a.Dispatched || a.Offered != a.Dispatched+a.GeneratorLimited || a.Offered > apiSpec.DurationSeconds*apiSpec.RatePerSecond {
		return errors.New("screened API counters inconsistent")
	}
	if len(r.Screening) == 0 || len(r.Screening) > len(s.RequestQualityPolicy.ScreeningSeconds) || len(r.Screening) != len(a.Checkpoints) {
		return errors.New("screened checkpoints incomplete")
	}
	var previous apiload.Counts
	lastTime := a.Started
	for i, decision := range r.Screening {
		c := decision.Checkpoint
		if c.AfterSeconds != s.RequestQualityPolicy.ScreeningSeconds[i] || !reflect.DeepEqual(c, a.Checkpoints[i]) || c.SettledAt.Before(a.Started.Add(time.Duration(c.AfterSeconds)*time.Second)) || c.SettledAt.Before(lastTime) || c.SettledAt.After(a.Finished) || c.Offered != c.AfterSeconds*apiSpec.RatePerSecond || !screenedCounts(c.Counts) || !countsWithin(previous, c.Counts) || !countsWithin(c.Counts, a.Counts) || !reflect.DeepEqual(decision, screenRequests(*s.RequestQualityPolicy, c)) || (decision.Status == "rejected") != (i == len(r.Screening)-1) {
			return errors.New("screened checkpoint contradicts source policy or API receipt")
		}
		previous, lastTime = c.Counts, c.SettledAt
	}
	if a.SchedulingStopped.Before(lastTime) || out.Finished.Before(lastTime.Add(time.Duration(s.RecoverySeconds)*time.Second)) {
		return errors.New("screened execution did not finish recovery")
	}
	c := r.Screening[len(r.Screening)-1].Checkpoint.Counts
	fraction := float64(c.DegradedRequests) / float64(c.Dispatched)
	quality := &RequestQualityAssessment{Status: "exceeded", CapacityStatus: "screened-out", MaxDegradedBasisPoints: s.RequestQualityPolicy.MaxDegradedBasisPoints, Dispatched: c.Dispatched, DegradedRequests: c.DegradedRequests, Rejected429: c.Rejected429, Timeouts: c.Timeouts, DegradedFraction: &fraction}
	if !reflect.DeepEqual(out.Workload.RequestQuality, quality) || out.Workload.Shed429 != a.Rejected429 || !reflect.DeepEqual(out.Workload.Violations, []string{"predeclared request quality screening rejected the point; full measurement not completed"}) {
		return errors.New("screened outcome contradicts rejection receipt")
	}
	t := r.Turnover
	want := workload.TurnoverSpec{Version: "v1", GuestNodes: s.Seed.GuestNodes, Applications: s.Seed.Applications, Seed: s.ScheduleSeed, NamespacePrefix: s.Seed.NamespacePrefix, DurationSeconds: s.MeasurementSeconds, TurnoverPeriodSeconds: 3600, RecoverySeconds: s.RecoverySeconds, MaxOutstanding: s.MaxOutstanding}
	if t == nil || t.Version != "v1" || t.Spec != want || t.StartedAt.Before(out.Started) || t.FinishedAt.Before(t.StartedAt) || t.FinishedAt.After(out.Finished) || !t.AdmissionEnd.Equal(t.StartedAt.Add(time.Duration(s.MeasurementSeconds)*time.Second)) || t.Outstanding != 0 || t.Backlog != 0 || t.PeakOutstanding < 0 || t.PeakOutstanding > s.MaxOutstanding {
		return errors.New("screened turnover source or finalization mismatch")
	}
	nonce, err := hex.DecodeString(t.RunID)
	if err != nil || len(nonce) != 16 {
		return errors.New("screened turnover run identity invalid")
	}
	schedule, err := experiment.RolloutsForPeriod(s.Seed.Applications, s.ScheduleSeed, time.Duration(s.MeasurementSeconds)*time.Second, time.Hour)
	if err != nil {
		return err
	}
	if len(t.Intents) != len(schedule) || t.Scheduled != len(schedule) {
		return errors.New("screened turnover schedule incomplete")
	}
	topology, err := screenedTopology(s)
	if err != nil {
		return err
	}
	uids := map[string]string{}
	for _, o := range inventory {
		key := populationKey(o.Resource, o.Namespace, o.Name)
		if topology[key] == nil || uids[key] != "" || o.UID == "" || len(o.SHA256) != 64 {
			return errors.New("screened inventory topology or identity invalid")
		}
		uids[key] = o.UID
	}
	if len(uids) != len(topology) {
		return errors.New("screened inventory topology incomplete")
	}
	admitted, accepted, completed, failed, pending := 0, 0, 0, 0, 0
	for i, intent := range t.Intents {
		ns, name := workload.TurnoverApplication(s.Seed.NamespacePrefix, schedule[i].Application)
		if intent.Index != i || intent.Intent != schedule[i] || intent.Namespace != ns || intent.Deployment != name || intent.RolloutID != fmt.Sprintf("%s-%06d", t.RunID, i) || !intent.ScheduledAt.Equal(t.StartedAt.Add(schedule[i].Offset)) || intent.Attempts < 0 || intent.ReadAttempts < 0 {
			return errors.New("screened turnover intent identity mismatch")
		}
		if intent.AdmittedAt.IsZero() {
			pending++
			if intent.State != "pending" || !intent.ScheduledAt.After(t.FinishedAt) || !intent.AttemptedAt.IsZero() || !intent.AcceptedAt.IsZero() || !intent.CompletedAt.IsZero() || !intent.FinishedAt.IsZero() || intent.Attempts != 0 || intent.ReadAttempts != 0 || intent.DeploymentUID != "" || intent.Generation != 0 || intent.Error != "" || !reflect.DeepEqual(intent.Last, workload.Snapshot{}) {
				return errors.New("screened pending intent has execution evidence")
			}
			continue
		}
		admitted++
		if intent.AdmittedAt.Before(intent.ScheduledAt) || intent.AdmittedAt.After(t.AdmissionEnd) || intent.FinishedAt.Before(intent.AdmittedAt) || intent.FinishedAt.After(t.FinishedAt) || (intent.State != "completed" && intent.State != "failed") {
			return errors.New("screened admitted rollout not terminal")
		}
		if intent.DeploymentUID != "" && string(intent.DeploymentUID) != uids["deployments/"+ns+"/"+name] {
			return errors.New("screened intent UID differs from inventory")
		}
		if (intent.Attempts == 0) != intent.AttemptedAt.IsZero() || (!intent.AttemptedAt.IsZero() && (intent.AttemptedAt.Before(intent.AdmittedAt) || intent.AttemptedAt.After(intent.FinishedAt))) {
			return errors.New("screened attempt timestamp mismatch")
		}
		if !intent.AcceptedAt.IsZero() {
			accepted++
			if intent.Attempts == 0 || intent.AcceptedAt.Before(intent.AttemptedAt) || intent.AcceptedAt.After(intent.FinishedAt) || intent.DeploymentUID == "" || intent.Generation <= 0 {
				return errors.New("screened acceptance evidence invalid")
			}
		} else if intent.Generation != 0 {
			return errors.New("screened unaccepted generation")
		}
		if intent.State == "completed" {
			completed++
			last := intent.Last.Deployment
			if intent.AcceptedAt.IsZero() || intent.CompletedAt.Before(intent.AcceptedAt) || intent.CompletedAt.After(intent.FinishedAt) || intent.Error != "" || last.UID != intent.DeploymentUID || last.Generation != intent.Generation || last.ObservedGeneration != intent.Generation || last.Desired != 5 || last.Replicas != 5 || last.Updated != 5 || last.Ready != 5 || last.Available != 5 || intent.Last.ObservedAt.Before(intent.AcceptedAt) || intent.Last.ObservedAt.After(intent.CompletedAt) {
				return errors.New("screened completion evidence invalid")
			}
		} else {
			failed++
			if !intent.CompletedAt.IsZero() || intent.Error == "" {
				return errors.New("screened failure evidence invalid")
			}
		}
	}
	if t.Admitted != admitted || t.Accepted != accepted || t.Completed != completed || t.Failed != failed || t.NotStarted != pending || (admitted > 0 && t.PeakOutstanding == 0) {
		return errors.New("screened turnover counters contradict intents")
	}
	return nil
}

func countsWithin(a, b apiload.Counts) bool {
	x, y := reflect.ValueOf(a), reflect.ValueOf(b)
	for i := 0; i < x.NumField(); i++ {
		if x.Field(i).Int() < 0 || x.Field(i).Int() > y.Field(i).Int() {
			return false
		}
	}
	return true
}

func screenedCounts(c apiload.Counts) bool {
	return countsWithin(apiload.Counts{}, c) && c.Offered <= apiload.MaxAttempts && c.Completed <= c.Dispatched && c.Dispatched <= c.Offered && c.HTTPAttempts >= c.Dispatched && c.DegradedRequests >= max(c.Timeouts, c.Rejected429) && c.DegradedRequests <= c.Timeouts+c.Rejected429 && c.DegradedRequests <= c.Completed && c.Successful+c.DegradedRequests+c.OtherHTTP+c.TransportErrors+c.Canceled+c.BodyLimitErrors == c.Completed
}

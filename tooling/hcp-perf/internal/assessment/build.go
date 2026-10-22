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

package assessment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/progress"
)

const maxMetadataBytes = 1 << 20

type builder struct {
	ctx    context.Context
	base   string
	result Result
}

// Build reads only allowlisted local artifacts. Invalid discovered evidence becomes
// an inconclusive finding; invalid explicit request evidence, cancellation and an
// unavailable run root are fatal.
// Evidence hashes cover the bytes actually decoded, not a prior progress report.
func Build(ctx context.Context, opts Options) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if opts.Input == "" {
		return Result{}, fmt.Errorf("input run directory is required")
	}
	base, err := filepath.Abs(opts.Input)
	if err != nil {
		return Result{}, err
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return Result{}, fmt.Errorf("input run directory unavailable")
	}
	defer root.Close()
	now := opts.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	b := builder{ctx: ctx, base: base, result: Result{
		SchemaVersion: SchemaVersion, AnalysisVersion: AnalysisVersion, GeneratedAt: now,
		Experiment: Experiment{ID: "unknown", Title: "unknown", Question: "unknown", Mode: "unknown", CPUTreatment: "unknown", MeasurementMode: "unspecified", FieldEvidence: map[string][]string{}},
		Status:     Status{Workload: "incomplete", Performance: "unknown", Capture: "unknown", Qualification: "not_assessed"},
		Findings:   []Finding{}, Failures: []Failure{}, Evidence: []Evidence{}, DataPoints: []DataPoint{}, CarryForward: []CarryForward{}, MetricsCoverage: []Coverage{},
	}}
	// Never walk the run root: private credentials, object dumps, raw logs and
	// archives are not JSON input. Failed canary stderr is classification-only.
	docs := map[string]map[string]json.RawMessage{}
	refs := map[string]string{}
	for _, path := range []string{"inputs/seed.json", "inputs/turnover.json", "inputs/session.json", "inputs/case.json", "inputs/profile.json", "evidence/resolved-profile.json", "commands/guest-capacity.json", "evidence/run.json", "evidence/outcome.json", "commands/turnover.json", "evidence/turnover/result.json", "evidence/record/manifest.json"} {
		var doc map[string]json.RawMessage
		if ref := b.document(root, path, &doc); ref != "" && doc != nil {
			docs[path], refs[path] = doc, ref
		}
	}
	// Conflicting explicit values are unknown; input ordering must not decide facts.
	conflicts := map[string]bool{}
	values := map[string]string{}
	fieldPointers := map[string][]string{}
	for _, path := range []string{"inputs/seed.json", "inputs/turnover.json", "inputs/session.json", "inputs/case.json", "commands/guest-capacity.json", "evidence/run.json"} {
		doc := docs[path]
		for _, field := range []string{"id", "title", "question", "mode", "guestNodes", "applications", "pods", "steadySeconds", "cpuTreatment", "measurementMode", "canaryPolicy"} {
			key := field
			if field == "pods" {
				if _, ok := doc["applicationPods"]; ok {
					key = "applicationPods"
				}
			}
			if field == "steadySeconds" && path == "inputs/turnover.json" {
				key = "durationSeconds"
			}
			raw, ok := doc[key]
			if !ok && field == "cpuTreatment" {
				var contended bool
				if v, exists := doc["contendedRequested"]; exists && json.Unmarshal(v, &contended) == nil && string(v) != "null" {
					raw = json.RawMessage(`"quiet"`)
					if contended {
						raw = json.RawMessage(`"contended"`)
					}
					ok = true
				}
			}
			if !ok || string(raw) == "null" {
				continue
			}
			var text string
			if field == "guestNodes" || field == "applications" || field == "pods" || field == "steadySeconds" {
				var n int64
				if json.Unmarshal(raw, &n) != nil || n < 0 || n > 1<<53 {
					continue
				}
				text = strconv.FormatInt(n, 10)
			} else if json.Unmarshal(raw, &text) != nil || text == "" {
				continue
			}
			if old, exists := values[field]; exists && old != text {
				conflicts[field] = true
			}
			values[field] = text
			b.result.Experiment.FieldEvidence[field] = append(b.result.Experiment.FieldEvidence[field], refs[path])
			fieldPointers[field] = append(fieldPointers[field], "/"+key)
		}
	}
	e := &b.result.Experiment
	for _, field := range []string{"id", "title", "question", "mode", "guestNodes", "applications", "pods", "steadySeconds", "cpuTreatment", "measurementMode", "canaryPolicy"} {
		v, exists := values[field]
		if !exists {
			continue
		}
		if conflicts[field] {
			b.finding("measurement", "inconclusive", "Conflicting explicit experiment field: "+field, e.FieldEvidence[field], "experiment")
			continue
		}
		switch field {
		case "id":
			e.ID = v
		case "title":
			e.Title = v
		case "question":
			e.Question = v
		case "mode":
			if v == "soak" || v == "validation" {
				e.Mode = v
			}
		case "cpuTreatment":
			if v == "quiet" || v == "contended" {
				e.CPUTreatment = v
			}
		case "measurementMode":
			e.MeasurementMode = v
		case "canaryPolicy":
			if v == "observe-degradation" {
				e.CanaryPolicy = v
			}
		default:
			n, _ := strconv.ParseInt(v, 10, 64)
			switch field {
			case "guestNodes":
				e.GuestNodes = &n
			case "applications":
				e.Applications = &n
			case "pods":
				e.Pods = &n
			case "steadySeconds":
				e.SteadySeconds = &n
			}
		}
	}
	for _, field := range []struct {
		name  string
		value *int64
	}{{"guestNodes", e.GuestNodes}, {"applications", e.Applications}, {"pods", e.Pods}} {
		if field.value != nil {
			b.point("benchmark."+field.name, float64(*field.value), "count", "configured", "benchmark input, not observed inventory", e.FieldEvidence[field.name], unique(fieldPointers[field.name]), "explicit input", Window{}, []string{"Configured count does not prove successful creation."})
		}
	}
	for _, path := range []string{"inputs/profile.json", "evidence/resolved-profile.json"} {
		var anchor map[string]int64
		if json.Unmarshal(docs[path]["anchor"], &anchor) != nil {
			continue
		}
		for _, field := range []struct{ name, unit string }{{"cpuMilli", "millicores"}, {"memoryMiB", "MiB"}} {
			if n, ok := anchor[field.name]; ok && n > 0 && n <= 1<<53 {
				b.point("sizingAnchor."+field.name, float64(n), field.unit, "configured", path, []string{refs[path]}, []string{"/anchor/" + field.name}, "explicit policy artifact", Window{}, []string{"Policy anchor is not proof of applied requests, whole-HCP consumption, or safe next sizing."})
			}
		}
	}
	b.workload(root, docs, refs)
	if opts.RequestEvidence != "" {
		if err := b.requestEvidence(opts.RequestEvidence); err != nil {
			return Result{}, err
		}
	}
	b.capture(root, docs, refs, opts.RecorderSummary)
	for _, dir := range opts.Metrics {
		b.metrics(dir)
	}
	if len(opts.Metrics) == 0 {
		b.finding("capture", "inconclusive", "No metrics roots supplied; metric coverage is unknown.", nil, "metrics")
	}
	b.finding("measurement", "inconclusive", "Only explicitly recorded measurement methods are used; missing methods are unspecified, not inferred as legacy polling. Timing comparisons require matching method and observation cadence.", e.FieldEvidence["measurementMode"], "canary timings")
	b.finding("resources", "inconclusive", "No whole-HCP sizing or memory-safety conclusion: per-series extrema cannot establish simultaneous whole-HCP demand, headroom, or a safe next size.", nil, "whole HCP")
	b.result.CarryForward = append(b.result.CarryForward, CarryForward{ID: "measurement-boundary", Action: "rebaseline", Recommendation: "Do not reuse baseline deadlines across a polling-to-watch measurement change. Record measurement mode and poll/watch cadence, then collect a new baseline.", EvidenceRefs: append([]string{}, e.FieldEvidence["measurementMode"]...), DataPointRefs: []string{}, Scope: "canary measurement", BlockedBy: []string{"measurement method and cadence equivalence not established"}, Conditions: []string{"review before changing measurement implementation"}, Kind: "fix_harness"})
	if len(b.result.DataPoints) > 0 {
		ids := []string{}
		evidence := []string{}
		for _, p := range b.result.DataPoints {
			if strings.HasPrefix(p.Name, "canary.") || p.Name == "turnover.completed" {
				ids = append(ids, p.ID)
				evidence = append(evidence, p.Provenance.EvidenceRefs...)
			}
		}
		if len(ids) > 0 {
			b.result.CarryForward = append(b.result.CarryForward, CarryForward{ID: "retain-observations", Action: "retain", Recommendation: "Retain these scoped workload observations for comparison; they are not capacity qualification or automatically fitted deadlines.", EvidenceRefs: unique(evidence), DataPointRefs: ids, Scope: "same workload, treatment and measurement method", BlockedBy: []string{}, Conditions: []string{"verify capture coverage and measurement equivalence before comparison"}, Kind: "retain"})
		}
	}
	b.outcome(docs["evidence/outcome.json"], refs["evidence/outcome.json"])
	b.commandProblems(root)
	return b.result, ctx.Err()
}

func (b *builder) finding(category, status, statement string, refs []string, scope string) {
	b.result.Findings = append(b.result.Findings, Finding{ID: fmt.Sprintf("finding-%d", len(b.result.Findings)+1), Category: category, Status: status, Statement: statement, Evidence: append([]string{}, refs...), Scope: scope})
}

func (b *builder) evidence(path, hash string) string {
	rel, _ := filepath.Rel(b.base, path)
	id := fmt.Sprintf("evidence-%d", len(b.result.Evidence)+1)
	b.result.Evidence = append(b.result.Evidence, Evidence{ID: id, Path: filepath.ToSlash(rel), SHA256: hash})
	return id
}

func (b *builder) document(root *os.Root, path string, target any) string {
	data, err := readMetadata(b.ctx, root, path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		b.finding("measurement", "inconclusive", "Metadata unavailable, nonregular, or exceeds 1 MiB: "+path, nil, "local evidence")
		return ""
	}
	hash := sha256.Sum256(data)
	ref := b.evidence(filepath.Join(root.Name(), path), hex.EncodeToString(hash[:]))
	var header struct {
		Version json.RawMessage `json:"version"`
	}
	if json.Unmarshal(data, &header) != nil || strings.TrimSpace(string(data)) == "null" || (len(header.Version) > 0 && string(header.Version) != `"v1"` && string(header.Version) != "1") || json.Unmarshal(data, target) != nil {
		b.finding("measurement", "inconclusive", "Invalid or unsupported metadata: "+path, []string{ref}, "local evidence")
		return ""
	}
	return ref
}

func readMetadata(ctx context.Context, root *os.Root, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Reject directory aliases as well as leaf symlinks, including aliases into
	// private/ inside the root. OpenRoot additionally prevents escaping the root.
	for parent := filepath.Dir(path); parent != "."; parent = filepath.Dir(parent) {
		info, err := root.Lstat(parent)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("metadata parent is not a real directory")
		}
	}
	info, err := root.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxMetadataBytes {
		return nil, fmt.Errorf("not bounded regular metadata")
	}
	f, err := root.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxMetadataBytes+1))
	if len(data) > maxMetadataBytes {
		return nil, fmt.Errorf("metadata grew past limit")
	}
	return data, err
}

func (b *builder) point(name string, value float64, unit, statistic, population string, refs, pointers []string, method string, window Window, limitations []string) {
	b.result.DataPoints = append(b.result.DataPoints, DataPoint{ID: fmt.Sprintf("point-%d", len(b.result.DataPoints)+1), Name: name, Value: value, Unit: unit, Statistic: statistic, Window: window, Population: population, Provenance: Provenance{EvidenceRefs: unique(refs), JSONPointers: pointers}, MeasurementMethod: method, Limitations: limitations, Applicability: "this run and stated population only"})
}

func unique(values []string) []string {
	result := []string{}
	seen := map[string]bool{}
	for _, v := range values {
		if v != "" && !seen[v] {
			result = append(result, v)
			seen[v] = true
		}
	}
	return result
}

var canaryName = regexp.MustCompile(`^canary-(baseline|steady)-[1-9][0-9]*\.json$`)
var numberedWorkloadCommand = regexp.MustCompile(`^(canary-(baseline|steady)|seed-warmup|adversaries-running)-(0|[1-9][0-9]*)$`)

// Only known workload command markers are read, never arbitrary command bodies,
// credential outputs, expected dispatch failures, or capture/finalization exits.
func workloadCommand(name string) bool {
	if numberedWorkloadCommand.MatchString(name) {
		return true
	}
	switch name {
	case "turnover", "turnover-gate", "failure-recovery", "recovery-window", "settle",
		"canary-warmup", "baseline-budgets", "baseline-expectations", "adversaries-start", "token-lifetime",
		"seed", "seed-deployments", "seed-deployment-gate", "seed-pods", "seed-pod-gate",
		"seed-identities", "seed-credentials", "workload-setup", "canary-credentials",
		"guest-nodes", "guest-node-gate", "guest-baseline", "guest-capacity", "hcp-inventory", "hcp-gate":
		return true
	}
	return false
}

type stage struct {
	Started             time.Time `json:"started"`
	Completed           time.Time `json:"completed"`
	Duration            int64     `json:"durationNanoseconds"`
	Attempts            int       `json:"attempts"`
	Expected            *int64    `json:"expectedDurationNanoseconds,omitempty"`
	Deadline            *int64    `json:"deadlineNanoseconds,omitempty"`
	ExpectationExceeded bool      `json:"expectationExceeded,omitempty"`
}

func (b *builder) workload(root *os.Root, docs map[string]map[string]json.RawMessage, refs map[string]string) {
	passed := map[string]int{}
	evaluated, degraded := map[string]int{}, map[string]int{}
	failed := false
	incomplete := false
	type timings struct {
		values  []float64
		refs    []string
		methods []string
		window  Window
	}
	samples := map[string]*timings{}
	var workloadRefs []string
	var measurementMethods, measurementRefs []string
	commandExits := map[string]int{}
	commandRefs := map[string]string{}
	var budgets map[string]json.RawMessage
	budgetRef := b.document(root, "commands/baseline-budgets.json", &budgets)
	deadlines := map[string]float64{}
	if budgetRef != "" {
		for _, part := range []string{"ack", "ready", "cleanup"} {
			var n *int64
			if json.Unmarshal(budgets[part], &n) != nil || n == nil || *n <= 0 {
				b.finding("measurement", "inconclusive", "Invalid positive integer nanosecond baseline budget: "+part, []string{budgetRef}, "steady canary deadlines")
				continue
			}
			deadlines[part] = float64(*n) / 1e9
		}
	}
	var expectations map[string]json.RawMessage
	if ref := b.document(root, "commands/baseline-expectations.json", &expectations); ref != "" {
		for _, part := range []string{"ack", "ready", "cleanup"} {
			var n *int64
			if json.Unmarshal(expectations[part], &n) != nil || n == nil || *n <= 0 {
				b.finding("measurement", "inconclusive", "Invalid positive integer nanosecond baseline expectation: "+part, []string{ref}, "steady canary expectations")
				continue
			}
			b.point("canaryExpectation."+part, float64(*n)/1e9, "seconds", "configured", "baseline-derived soft expectation summary", []string{ref}, []string{"/" + part}, "explicit expectation artifact", Window{}, []string{"Comparisons use each completed stage's stored expectation, never this file to reclassify historical results."})
		}
	}
	dir, err := root.Open("commands")
	if err == nil {
		entries, readErr := dir.ReadDir(-1)
		dir.Close()
		if readErr != nil {
			incomplete = true
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			name := entry.Name()
			ext := filepath.Ext(name)
			command := strings.TrimSuffix(name, ext)
			if (ext != ".exit" && ext != ".started") || !workloadCommand(command) {
				continue
			}
			data, err := readMetadata(b.ctx, root, "commands/"+name)
			if err != nil {
				incomplete = true
				b.finding("workload", "inconclusive", "Workload command marker unavailable: "+name, nil, "workload commands")
				continue
			}
			hash := sha256.Sum256(data)
			ref := b.evidence(filepath.Join(b.base, "commands", name), hex.EncodeToString(hash[:]))
			workloadRefs = append(workloadRefs, ref)
			if ext == ".started" {
				if _, ok := commandExits[command]; !ok {
					incomplete = true
					b.finding("workload", "inconclusive", "Started workload command has no valid exit evidence: "+command, []string{ref}, "workload commands")
				}
				continue
			}
			code, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil || code < 0 || code > 255 {
				incomplete = true
				b.finding("workload", "inconclusive", "Invalid workload command exit: "+command, []string{ref}, "workload commands")
				continue
			}
			commandExits[command], commandRefs[command] = code, ref
			if code != 0 {
				failed = true
				b.failure(root, command, ref, deadlines, budgetRef)
			}
		}
		for _, entry := range entries {
			name := entry.Name()
			if !canaryName.MatchString(name) {
				continue
			}
			command := strings.TrimSuffix(name, ".json")
			if exit, exists := commandExits[command]; exists && exit != 0 {
				continue // Already assessed as a failure, never a successful latency.
			}
			phase := "baseline"
			if strings.HasPrefix(name, "canary-steady-") {
				phase = "steady"
			}
			var sample struct {
				Ack             stage  `json:"ack"`
				Ready           stage  `json:"ready"`
				Cleanup         stage  `json:"cleanup"`
				MeasurementMode string `json:"measurementMode"`
			}
			ref := b.document(root, "commands/"+name, &sample)
			_, exists := commandExits[command]
			if !exists || ref == "" {
				incomplete = true
				continue
			}
			exitRef := commandRefs[command]
			workloadRefs = append(workloadRefs, ref, exitRef)
			valid := true
			for _, s := range []stage{sample.Ack, sample.Ready, sample.Cleanup} {
				if s.Started.IsZero() || s.Completed.Before(s.Started) || s.Completed.After(b.result.GeneratedAt) || s.Duration <= 0 {
					valid = false
				}
			}
			if !valid {
				incomplete = true
				continue
			}
			passed[phase]++
			cycleEvaluated, cycleDegraded := false, false
			for _, part := range []struct {
				name  string
				stage stage
			}{{"ack", sample.Ack}, {"ready", sample.Ready}, {"cleanup", sample.Cleanup}} {
				s := part.stage
				if s.Expected == nil || *s.Expected <= 0 {
					continue
				}
				cycleEvaluated = true
				if s.Duration > *s.Expected {
					cycleDegraded = true
					hard := "unknown"
					if s.Deadline != nil && *s.Deadline > 0 {
						hard = failureDuration(float64(*s.Deadline) / 1e9)
					}
					b.finding("performance", "observed", fmt.Sprintf("%s: %s completed in %s, exceeding the recorded soft expectation %s by %s; explicit hard stop %s. Ready and cleanup completed; the cycle continued rather than canceling at the expectation. This is latency degradation, not a workload failure.", command, strings.ToUpper(part.name), failureDuration(float64(s.Duration)/1e9), failureDuration(float64(*s.Expected)/1e9), failureDuration(float64(s.Duration-*s.Expected)/1e9), hard), []string{ref, exitRef}, "completed canary cycle")
				}
			}
			if cycleEvaluated || cycleDegraded {
				evaluated[phase]++
			}
			if cycleDegraded {
				degraded[phase]++
			}
			method := sample.MeasurementMode
			if method == "" {
				method = "unspecified"
			}
			measurementMethods = append(measurementMethods, method)
			measurementRefs = append(measurementRefs, ref)
			for _, part := range []struct {
				name  string
				stage stage
			}{{"ack", sample.Ack}, {"ready", sample.Ready}, {"cleanup", sample.Cleanup}} {
				key := phase + "." + part.name
				s := samples[key]
				if s == nil {
					s = &timings{}
					samples[key] = s
				}
				s.values = append(s.values, float64(part.stage.Duration)/1e9)
				s.refs = append(s.refs, ref, exitRef)
				s.methods = append(s.methods, method)
				start, end := part.stage.Started.UTC(), part.stage.Completed.UTC()
				if s.window.Start == nil || start.Before(*s.window.Start) {
					s.window.Start = &start
				}
				if s.window.End == nil || end.After(*s.window.End) {
					s.window.End = &end
				}
			}
		}
	} else {
		incomplete = true
	}
	if methods := unique(measurementMethods); len(methods) > 0 {
		e := &b.result.Experiment
		if e.MeasurementMode != "unspecified" {
			methods = unique(append(methods, e.MeasurementMode))
		}
		if len(methods) == 1 {
			e.MeasurementMode = methods[0]
		} else {
			e.MeasurementMode = "mixed"
		}
		e.FieldEvidence["measurementMode"] = unique(append(e.FieldEvidence["measurementMode"], measurementRefs...))
	}
	if degraded["baseline"]+degraded["steady"] > 0 {
		b.result.Status.Performance = "degraded"
	} else if evaluated["steady"] > 0 && evaluated["steady"] == passed["steady"] {
		b.result.Status.Performance = "within_expectations"
	}
	for _, phase := range []string{"baseline", "steady"} {
		if evaluated[phase] > 0 {
			b.point("canary."+phase+".evaluated", float64(evaluated[phase]), "count", "count", "completed cycles with recorded soft expectations", workloadRefs, []string{"/ack/expectedDurationNanoseconds", "/ready/expectedDurationNanoseconds", "/cleanup/expectedDurationNanoseconds"}, "recorded stage expectations", Window{}, []string{"This is an observation count, not the number of planned cadence slots."})
			b.point("canary."+phase+".degraded", float64(degraded[phase]), "count", "count", "completed cycles with recorded soft expectations", workloadRefs, []string{"/ack/expectedDurationNanoseconds", "/ready/expectedDurationNanoseconds", "/cleanup/expectedDurationNanoseconds"}, "recorded stage duration compared with its own expectation", Window{}, []string{"Failed and censored cycles are not completed latency observations; legacy hard budgets are not soft expectations."})
		}
		for _, part := range []string{"ack", "ready", "cleanup"} {
			s := samples[phase+"."+part]
			if s == nil {
				continue
			}
			sort.Float64s(s.values)
			n := len(s.values)
			median := s.values[n/2]
			if n%2 == 0 {
				median = (s.values[n/2-1] + median) / 2
			}
			method := "mixed"
			if methods := unique(s.methods); len(methods) == 1 {
				method = methods[0]
			}
			for _, stat := range []struct {
				name  string
				value float64
				unit  string
			}{{"median", median, "seconds"}, {"max", s.values[n-1], "seconds"}, {"count", float64(n), "count"}} {
				b.point("canary."+phase+"."+part, stat.value, stat.unit, stat.name, "successful "+phase+" cycles with complete stage and zero command exit", s.refs, []string{"/" + part + "/durationNanoseconds"}, method, s.window, []string{"Successful cycles only; failed and incomplete cycles excluded.", "Client-observed elapsed time, not pure server or scheduler latency; absent method/cadence cannot be inferred from current source code."})
			}
		}
	}
	var turnover struct {
		Started     time.Time `json:"startedAt"`
		End         time.Time `json:"admissionEnd"`
		Finished    time.Time `json:"finishedAt"`
		Scheduled   *int      `json:"scheduled"`
		Completed   *int      `json:"completed"`
		Failed      *int      `json:"failed"`
		NotStarted  *int      `json:"notStarted"`
		Backlog     *int      `json:"backlog"`
		Outstanding *int      `json:"outstanding"`
	}
	turnoverOK := false
	turnoverRef := ""
	for _, path := range []string{"evidence/turnover/result.json", "commands/turnover.json"} {
		doc := docs[path]
		if doc == nil {
			continue
		}
		data, _ := json.Marshal(doc)
		if json.Unmarshal(data, &turnover) != nil {
			incomplete = true
			b.finding("workload", "inconclusive", "Invalid typed turnover result: "+path, []string{refs[path]}, "turnover")
			break
		}
		turnoverRef = refs[path]
		workloadRefs = append(workloadRefs, refs[path])
		counts := []*int{turnover.Scheduled, turnover.Completed, turnover.Failed, turnover.NotStarted, turnover.Backlog, turnover.Outstanding}
		valid := true
		for _, n := range counts {
			if n == nil || *n < 0 || *n > 80000 {
				valid = false
			}
		}
		if !valid || turnover.Started.IsZero() || turnover.End.Before(turnover.Started) || turnover.Finished.Before(turnover.End) || turnover.Finished.After(b.result.GeneratedAt) {
			incomplete = true
			break
		}
		if *turnover.Completed+*turnover.Failed+*turnover.NotStarted != *turnover.Scheduled {
			incomplete = true
			break
		}
		failed = failed || *turnover.Failed > 0
		turnoverOK = *turnover.Completed == *turnover.Scheduled && *turnover.Failed == 0 && *turnover.NotStarted == 0 && *turnover.Backlog == 0 && *turnover.Outstanding == 0
		for _, p := range []struct {
			name string
			n    *int
		}{{"scheduled", turnover.Scheduled}, {"completed", turnover.Completed}, {"failed", turnover.Failed}, {"notStarted", turnover.NotStarted}} {
			b.point("turnover."+p.name, float64(*p.n), "count", "count", "recorded turnover intents", []string{refs[path]}, []string{"/" + p.name}, "turnover result counters", Window{Start: &turnover.Started, End: &turnover.Finished}, []string{"Application-level rollout counts, not pod turnover, HTTP requests or APF receipts."})
		}
		break
	}
	var outcome struct {
		Exit           *int  `json:"exitCode"`
		WorkloadExit   *int  `json:"workloadExitCode"`
		ShutdownFailed *bool `json:"shutdownOrExportFailed"`
	}
	data, _ := json.Marshal(docs["evidence/outcome.json"])
	outcomeErr := json.Unmarshal(data, &outcome)
	_, explicitWorkloadExit := docs["evidence/outcome.json"]["workloadExitCode"]
	outcomeValid := outcomeErr == nil && outcome.Exit != nil && *outcome.Exit >= 0 && *outcome.Exit <= 255 && outcome.ShutdownFailed != nil &&
		(!explicitWorkloadExit || (outcome.WorkloadExit != nil && *outcome.WorkloadExit >= 0 && *outcome.WorkloadExit <= 255))
	if !outcomeValid && refs["evidence/outcome.json"] != "" {
		b.finding("workload", "inconclusive", "Invalid typed outcome: require integer exitCode in 0..255, boolean shutdownOrExportFailed, and integer workloadExitCode in 0..255 when present; no partially decoded values are used.", []string{refs["evidence/outcome.json"]}, "terminal workload outcome")
	}
	var session struct {
		Baseline  *int   `json:"baselineCycles"`
		Steady    *int   `json:"expectedSteadyCanaries"`
		Scheduled *int64 `json:"scheduledRollouts"`
	}
	var hardDeadlines map[string]json.RawMessage
	if raw, exists := docs["inputs/session.json"]["hardDeadlineSeconds"]; exists {
		if json.Unmarshal(raw, &hardDeadlines) != nil || hardDeadlines == nil {
			b.finding("measurement", "inconclusive", "Invalid hardDeadlineSeconds metadata.", []string{refs["inputs/session.json"]}, "canary hard stops")
		} else {
			for _, part := range []string{"ack", "ready", "cleanup"} {
				var n *int64
				if json.Unmarshal(hardDeadlines[part], &n) != nil || n == nil || *n <= 0 || *n > math.MaxInt64/int64(time.Second) {
					b.finding("measurement", "inconclusive", "Invalid positive integer hard-deadline seconds within nanosecond range: "+part, []string{refs["inputs/session.json"]}, "canary hard stops")
					continue
				}
				b.point("canaryHardDeadline."+part, float64(*n), "seconds", "configured", "explicit session hard stop", []string{refs["inputs/session.json"]}, []string{"/hardDeadlineSeconds/" + part}, "explicit session policy", Window{}, []string{"Stage-recorded hard deadlines take precedence; session policy does not reinterpret historical client waits."})
			}
		}
	}
	data, _ = json.Marshal(docs["inputs/session.json"])
	sessionErr := json.Unmarshal(data, &session)
	if sessionErr != nil {
		incomplete = true
		b.finding("workload", "inconclusive", "Invalid typed session metadata; completion expectations are unusable.", []string{refs["inputs/session.json"]}, "workload intent")
	}
	if data, err := readMetadata(b.ctx, root, "evidence/canary-cadence.jsonl"); !os.IsNotExist(err) {
		var cadenceRefs []string
		if err == nil {
			hash := sha256.Sum256(data)
			cadenceRefs = []string{b.evidence(filepath.Join(b.base, "evidence/canary-cadence.jsonl"), hex.EncodeToString(hash[:]))}
		}
		slots, decodeErr := progress.CadenceSkips(data)
		valid := err == nil && decodeErr == nil
		for _, slot := range slots {
			_, attempted := commandExits["canary-steady-"+strconv.Itoa(slot)]
			valid = valid && !attempted && session.Steady != nil && slot <= *session.Steady
		}
		if !valid {
			incomplete = true
			b.finding("measurement", "inconclusive", "Invalid cadence observations; skipped-slot count unknown.", cadenceRefs, "steady canary observation coverage")
		} else {
			b.point("canary.steady.skipped", float64(len(slots)), "count", "count", "recorded skipped cadence slots", cadenceRefs, []string{"/slotIndex", "/type"}, "explicit cadence observations", Window{}, []string{"Skipped slots are not completed samples or workload failures."})
			if len(slots) > 0 {
				incomplete = true
				b.finding("measurement", "observed", fmt.Sprintf("%d canary slots were skipped; %d steady cycles completed. Observation gaps leave workload coverage incomplete, not failed solely because observations were missed.", len(slots), passed["steady"]), unique(append(cadenceRefs, workloadRefs...)), "steady canary observation coverage")
			}
		}
	}
	workloadRefs = append(workloadRefs, refs["inputs/session.json"], refs["evidence/outcome.json"])
	// A legacy combined exit cannot distinguish export-only failure from failure
	// after the turnover gate (for example, recovery-window). Never infer it clean.
	workloadFinished := false
	if outcomeValid {
		if explicitWorkloadExit {
			failed = failed || *outcome.WorkloadExit != 0
			workloadFinished = *outcome.WorkloadExit == 0
			if workloadFinished && ((*outcome.Exit != 0 && !*outcome.ShutdownFailed) || (*outcome.Exit == 0 && *outcome.ShutdownFailed)) {
				workloadFinished = false
				b.finding("workload", "inconclusive", "Conflicting combined and workload exit evidence.", []string{refs["evidence/outcome.json"]}, "terminal workload outcome")
			}
		} else if *outcome.Exit == 0 && !*outcome.ShutdownFailed {
			workloadFinished = true
		} else if *outcome.Exit != 0 && !*outcome.ShutdownFailed {
			failed = true
		} else {
			b.finding("workload", "inconclusive", "Legacy combined shutdown/export failure has no workloadExitCode; even a successful turnover gate does not establish terminal workload success.", []string{refs["evidence/outcome.json"]}, "terminal workload outcome")
		}
	}
	expected := session.Scheduled
	intentRefs := []string{refs["inputs/session.json"]}
	_, explicitSchedule := docs["inputs/session.json"]["scheduledRollouts"]
	// Validate every explicit source, including the executor's resolved spec.
	// Missing period in historical artifacts means hourly; null or contradictory
	// evidence must not silently fall back to hourly or an explicit count.
	intentValid := sessionErr == nil
	intent := map[string]int64{}
	var periodRefs []string
	for _, path := range []string{"inputs/seed.json", "inputs/turnover.json", "inputs/session.json", "inputs/case.json", "evidence/run.json", "commands/turnover.json", "evidence/turnover/result.json"} {
		doc := docs[path]
		nested := path == "commands/turnover.json" || path == "evidence/turnover/result.json"
		if nested {
			if raw, exists := doc["spec"]; exists {
				if json.Unmarshal(raw, &doc) != nil || doc == nil {
					intentValid = false
					intentRefs = append(intentRefs, refs[path])
					continue
				}
			} else {
				continue
			}
		}
		for _, field := range []struct {
			name     string
			min, max int64
		}{{"applications", 0, 40000}, {"steadySeconds", 1, 7200}, {"turnoverPeriodSeconds", 0, 86400}, {"scheduledRollouts", 0, 80000}} {
			key := field.name
			if key == "steadySeconds" && (nested || path == "inputs/turnover.json") {
				key = "durationSeconds"
			}
			raw, exists := doc[key]
			if !exists {
				continue
			}
			intentRefs = append(intentRefs, refs[path])
			if field.name == "turnoverPeriodSeconds" {
				periodRefs = append(periodRefs, refs[path])
			}
			var n *int64
			if json.Unmarshal(raw, &n) != nil || n == nil || *n < field.min || *n > field.max {
				intentValid = false
				continue
			}
			if field.name == "turnoverPeriodSeconds" && *n == 0 {
				*n = 3600
			}
			if old, exists := intent[field.name]; exists && old != *n {
				intentValid = false
			}
			intent[field.name] = *n
		}
	}
	period, recordedPeriod := intent["turnoverPeriodSeconds"]
	if !recordedPeriod {
		period = 3600
	}
	if n, exists := intent["scheduledRollouts"]; exists {
		expected, explicitSchedule = &n, true
	}
	apps, haveApps := intent["applications"]
	duration, haveDuration := intent["steadySeconds"]
	if intentValid && haveApps && haveDuration {
		// Bounds above make the product and rounding addition safe in int64.
		n := (apps*duration + period - 1) / period
		if n > 80000 || (explicitSchedule && (expected == nil || *expected != n)) {
			intentValid = false
		} else if !explicitSchedule {
			expected = &n
		}
	}
	if !intentValid {
		incomplete = true
		b.finding("workload", "inconclusive", "Invalid or conflicting explicit rollout intent: applications, duration, period and scheduled count must agree and fit the 80000-rollout cap.", unique(intentRefs), "turnover schedule")
	} else {
		b.result.Experiment.TurnoverPeriodSeconds = &period
		b.result.Experiment.FieldEvidence["turnoverPeriodSeconds"] = unique(periodRefs)
	}
	scheduleKnown := intentValid && expected != nil && *expected >= 0 && *expected <= 80000
	if scheduleKnown {
		pointers := []string{"/scheduledRollouts"}
		method := "explicit recorded schedule"
		if !explicitSchedule {
			pointers = []string{"/applications", "/steadySeconds", "/durationSeconds", "/turnoverPeriodSeconds", "/spec/applications", "/spec/durationSeconds", "/spec/turnoverPeriodSeconds"}
			method = "canonical period schedule from recorded inputs (absent or zero period defaults to 3600 seconds)"
		}
		b.point("turnover.intended", float64(*expected), "count", "configured", "intended admission-window rollouts", intentRefs, pointers, method, Window{}, []string{"Intent is not completed throughput."})
		if !explicitSchedule {
			b.result.DataPoints[len(b.result.DataPoints)-1].Provenance.Formula = fmt.Sprintf("ceil(applications * steadySeconds / %d)", period)
		}
		workloadRefs = append(workloadRefs, intentRefs...)
		if turnover.Scheduled != nil && int64(*turnover.Scheduled) != *expected {
			incomplete = true
			b.finding("workload", "observed", fmt.Sprintf("Turnover scheduled count %d differs from intended count %d; a successful gate cannot override this mismatch.", *turnover.Scheduled, *expected), unique(append(append([]string{}, intentRefs...), turnoverRef)), "turnover schedule")
		}
	} else {
		b.finding("workload", "inconclusive", "Intended rollout count is unavailable or invalid; result counters alone cannot establish workload completion.", unique(intentRefs), "turnover schedule")
	}
	steady := b.result.Experiment.SteadySeconds
	fullWindow := steady != nil && *steady > 0 && float64(*steady) <= turnover.End.Sub(turnover.Started).Seconds()
	if turnoverOK && passed["baseline"]+passed["steady"] > 0 {
		b.finding("workload", "observed", fmt.Sprintf("Recorded %d successful baseline and %d successful steady canaries; turnover completed all %d recorded scheduled rollouts. These observations do not establish terminal workload success or agreement with intent.", passed["baseline"], passed["steady"], *turnover.Completed), unique(workloadRefs), "completed workload observations")
	}
	if failed {
		b.result.Status.Workload = "fail"
		if b.result.Status.Performance == "within_expectations" {
			b.result.Status.Performance = "unknown"
		}
	} else if !incomplete && scheduleKnown && turnoverOK && workloadFinished && fullWindow && session.Baseline != nil && *session.Baseline > 0 && passed["baseline"] == *session.Baseline && session.Steady != nil && *session.Steady > 0 && passed["steady"] == *session.Steady {
		b.result.Status.Workload = "pass"
	}
	status := "observed"
	if b.result.Status.Workload == "incomplete" {
		status = "inconclusive"
	}
	b.finding("workload", status, "Workload status is "+b.result.Status.Workload+"; this axis does not establish capture completeness or qualify capacity.", unique(workloadRefs), "recorded workload execution")
}

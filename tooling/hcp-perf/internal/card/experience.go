// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package card

import (
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"time"
)

const ExperienceSchemaVersion = "hcp-perf.experience/v1"
const MaxExperienceBytes = 2 << 20

// Experience retains the reviewed wire data, including nulls and provenance.
// Parsing checks internal consistency; rendering additionally binds it to Card.
// Neither operation opens source files or verifies their declared hashes.
type Experience struct {
	data map[string]any
}

func experienceClose(a, b float64) bool {
	// Rates were calculated before binary64 Unix timestamp conversion, including
	// the short final bin. Never use this relative tolerance for epoch boundaries.
	return math.Abs(a-b) <= 1e-7*math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
}

func experienceTime(a, b float64) bool { return math.Abs(a-b) <= 1e-6 }

func experienceStamp(m map[string]any, numeric, exact string) error {
	v, vok := m[numeric].(float64)
	s, sok := m[exact].(string)
	if !vok && m[numeric] == nil && m[exact] == nil {
		return nil
	}
	if !vok || !sok {
		return fmt.Errorf("%s and %s must both be present or null", numeric, exact)
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || !experienceTime(v, float64(t.Unix())+float64(t.Nanosecond())/1e9) {
		return fmt.Errorf("%s disagrees with %s", numeric, exact)
	}
	return nil
}

func ParseExperience(data []byte) (*Experience, error) {
	if len(data) > MaxExperienceBytes {
		return nil, fmt.Errorf("experience JSON exceeds %d bytes", MaxExperienceBytes)
	}
	d, err := decode(data, ExperienceSchemaVersion)
	if err != nil {
		return nil, err
	}
	if id, ok := d["cardID"].(string); !ok || strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("experience cardID is required")
	}
	for _, k := range []string{"window.start", "window.end", "method.binSeconds"} {
		if err := numberAt(d, k, true); err != nil {
			return nil, err
		}
	}
	start, end := at(d, "window.start").(float64), at(d, "window.end").(float64)
	if end <= start || end > 253402300799 || at(d, "method.binSeconds") != float64(10) || at(d, "method.timestampUnit") != "unixSeconds" {
		return nil, fmt.Errorf("experience requires an increasing UTC window and 10-second bins")
	}
	sources, ok := d["sources"].([]any)
	if !ok || len(sources) == 0 {
		return nil, fmt.Errorf("experience sources are required")
	}
	declared := map[string]bool{}
	for _, raw := range sources {
		s, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("experience source must be an object")
		}
		id, _ := s["id"].(string)
		file, _ := s["file"].(string)
		hash, _ := s["sha256"].(string)
		decoded, err := hex.DecodeString(hash)
		if strings.TrimSpace(id) == "" || strings.TrimSpace(file) == "" || declared[id] || err != nil || len(decoded) != 32 {
			return nil, fmt.Errorf("experience source requires unique id, file and SHA-256 metadata")
		}
		declared[id] = true
	}
	refs := func(m map[string]any, keys []string) error {
		for _, k := range keys {
			id, _ := at(m, "sourceIDs."+k).(string)
			if !declared[id] {
				return fmt.Errorf("experience sourceIDs.%s must reference a declared source", k)
			}
		}
		return nil
	}
	if err := refs(d["method"].(map[string]any), []string{"intent", "expectations", "window"}); err != nil {
		return nil, err
	}
	arms, ok := d["arms"].([]any)
	if !ok || len(arms) == 0 || len(arms) > 32 {
		return nil, fmt.Errorf("experience requires 1 to 32 arms")
	}
	ids, cycles := map[string]bool{}, map[string]bool{}
	canaries := func(raw any, armID, phase string) error {
		items, ok := raw.([]any)
		if !ok || len(items) > 1000 {
			return fmt.Errorf("experience canaries must be an array of at most 1000 cycles")
		}
		for _, raw := range items {
			c, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("experience canary must be an object")
			}
			id, _ := c["id"].(string)
			if !declared[id] || !declared[id+"-exit"] || cycles[id] || c["armID"] != armID || c["phase"] != phase || c["measurementMode"] != "watch" {
				return fmt.Errorf("experience canary %q has invalid identity, sources, phase or measurementMode", id)
			}
			cycles[id] = true
			if c["status"] != "completed" && c["status"] != "failed" && c["status"] != "unknown" {
				return fmt.Errorf("experience canary %s status must be completed, failed or unknown", id)
			}
			if err := numberAt(c, "started", true); err != nil {
				return err
			}
			if c["started"].(float64) < start || c["started"].(float64) > end {
				return fmt.Errorf("experience canary %s is outside window", id)
			}
			for _, name := range []string{"ack", "ready", "cleanup"} {
				s, ok := at(c, "stages."+name).(map[string]any)
				if !ok {
					return fmt.Errorf("experience canary %s requires stage %s (nullable observations)", id, name)
				}
				for _, k := range []string{"start", "completed", "durationSeconds", "durationNanoseconds", "expectedSeconds", "hardDeadlineSeconds"} {
					if err := numberAt(s, k, false); err != nil {
						return err
					}
				}
				for _, k := range []string{"start", "completed"} {
					if v, ok := s[k].(float64); ok && (v < c["started"].(float64) || v > end) {
						return fmt.Errorf("experience canary %s %s outside cycle/window", id, k)
					}
				}
				if err := experienceStamp(s, "start", "startedRFC3339"); err != nil {
					return err
				}
				if err := experienceStamp(s, "completed", "completedRFC3339"); err != nil {
					return err
				}
				duration, dok := s["durationSeconds"].(float64)
				nanos, nok := s["durationNanoseconds"].(float64)
				if dok != nok || (nok && (nanos != math.Trunc(nanos) || !experienceClose(duration, nanos/1e9))) {
					return fmt.Errorf("experience canary %s inconsistent duration", id)
				}
				from, fok := s["start"].(float64)
				to, tok := s["completed"].(float64)
				if tok && (!fok || to < from || !dok) || c["status"] == "completed" && !tok {
					return fmt.Errorf("experience canary %s invalid completion evidence", id)
				}
				if tok && !experienceTime(to-from, duration) {
					return fmt.Errorf("experience canary %s duration disagrees with endpoints", id)
				}
				if phase == "baseline" && s["expectedSeconds"] != nil {
					return fmt.Errorf("baseline expectations must remain null, not retroactively evaluated")
				}
			}
			ack, ready := at(c, "stages.ack").(map[string]any), at(c, "stages.ready").(map[string]any)
			if a, aok := ack["start"].(float64); aok {
				if r, rok := ready["start"].(float64); rok && !experienceTime(a, r) {
					return fmt.Errorf("Ready must include ACK from the same start")
				}
			}
			if a, aok := ack["completed"].(float64); aok {
				if r, rok := ready["completed"].(float64); rok && r < a {
					return fmt.Errorf("Ready completion cannot precede ACK completion")
				}
			}
			if r, rok := ready["completed"].(float64); rok {
				if cleanup, cok := at(c, "stages.cleanup.start").(float64); cok && cleanup < r {
					return fmt.Errorf("Cleanup cannot precede Ready completion")
				}
			}
		}
		return nil
	}
	if err := canaries(d["canaries"], "baseline", "baseline"); err != nil {
		return nil, err
	}
	previousEnd := start
	countKeys := []string{"dispatched", "completed", "successful", "http429", "timeouts", "otherErrors"}
	for _, raw := range arms {
		a, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("experience arm must be an object")
		}
		id, _ := a["id"].(string)
		if strings.TrimSpace(id) == "" || id == "baseline" || ids[id] {
			return nil, fmt.Errorf("experience arm id must be unique and nonempty")
		}
		ids[id] = true
		for _, k := range []string{"shares", "start", "stop", "drainEnd", "offeredQPS"} {
			if err := numberAt(a, k, true); err != nil {
				return nil, err
			}
		}
		from, stop, to := a["start"].(float64), a["stop"].(float64), a["drainEnd"].(float64)
		if from < previousEnd || stop <= from || to < stop || to > end {
			return nil, fmt.Errorf("experience arm %s boundaries must be ordered, nonoverlapping and within window", id)
		}
		previousEnd = to
		for numeric, exact := range map[string]string{"start": "startedRFC3339", "stop": "schedulingStoppedRFC3339", "drainEnd": "finishedRFC3339"} {
			if err := experienceStamp(a, numeric, exact); err != nil {
				return nil, err
			}
		}
		if err := refs(a, []string{"requests", "result", "spec"}); err != nil {
			return nil, err
		}
		target, ok := a["targetOfferedQPS"].([]any)
		if !ok || len(target) != 2 {
			return nil, fmt.Errorf("experience targetOfferedQPS needs active and drain segments")
		}
		for i, want := range []map[string]any{{"start": from, "end": stop, "qps": a["offeredQPS"]}, {"start": stop, "end": to, "qps": float64(0)}} {
			s, ok := target[i].(map[string]any)
			if !ok || s["start"] != want["start"] || s["end"] != want["end"] || s["qps"] != want["qps"] {
				return nil, fmt.Errorf("experience targetOfferedQPS disagrees with arm boundaries/demand")
			}
		}
		totals, ok := a["totals"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("experience totals required")
		}
		for _, k := range append(append([]string{}, countKeys...), "offered", "generatorLimited", "httpAttempts", "http429TimeoutOverlap", "http200Timeouts") {
			if err := numberAt(totals, k, true); err != nil {
				return nil, err
			}
			if v := totals[k].(float64); v != math.Trunc(v) {
				return nil, fmt.Errorf("experience %s must be an integer count", k)
			}
		}
		if totals["completed"] != totals["dispatched"] || totals["offered"].(float64) != totals["dispatched"].(float64)+totals["generatorLimited"].(float64) || totals["httpAttempts"].(float64) < totals["dispatched"].(float64) || totals["http200Timeouts"].(float64)+totals["http429TimeoutOverlap"].(float64) > totals["timeouts"].(float64) {
			return nil, fmt.Errorf("experience %s inconsistent scheduling/terminal totals", id)
		}
		bins, ok := a["bins"].([]any)
		if !ok || len(bins) == 0 || len(bins) > 2000 {
			return nil, fmt.Errorf("experience bins must contain 1 to 2000 intervals")
		}
		sums := map[string]float64{}
		boundary := from
		for i, raw := range bins {
			b, ok := raw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("experience bin must be an object")
			}
			for _, k := range append([]string{"start", "end"}, countKeys...) {
				if err := numberAt(b, k, true); err != nil {
					return nil, err
				}
			}
			left, right := b["start"].(float64), b["end"].(float64)
			if left != boundary || right <= left || !experienceTime(right, math.Min(from+float64(i+1)*10, to)) || right > to || b["offered"] != nil {
				return nil, fmt.Errorf("experience %s bins must be contiguous 10-second intervals with actual final width and null offered", id)
			}
			boundary = right
			for _, k := range countKeys {
				v := b[k].(float64)
				if v != math.Trunc(v) {
					return nil, fmt.Errorf("experience bin %s must be an integer count", k)
				}
				sums[k] += v
			}
			// Completions may carry over from earlier bins, but cannot precede dispatch.
			if sums["completed"] > sums["dispatched"] {
				return nil, fmt.Errorf("experience %s bin %d: cumulative completed exceeds cumulative dispatched", id, i)
			}
			if err := experienceRates(b, right-left, false); err != nil {
				return nil, fmt.Errorf("experience %s bin %d: %w", id, i, err)
			}
			for _, k := range []string{"latencyP50Seconds", "latencyP95Seconds", "successLatencyP95Seconds", "getSuccessP95Seconds", "listSuccessP95Seconds"} {
				if err := numberAt(b, k, false); err != nil {
					return nil, err
				}
				n := b["successful"]
				if k == "latencyP50Seconds" || k == "latencyP95Seconds" {
					n = b["completed"]
				}
				if n == float64(0) && b[k] != nil {
					return nil, fmt.Errorf("experience empty latency subset must be null")
				}
			}
			p50, p50ok := b["latencyP50Seconds"].(float64)
			p95, p95ok := b["latencyP95Seconds"].(float64)
			if p50ok && p95ok && p50 > p95 {
				return nil, fmt.Errorf("experience latency p50 exceeds p95")
			}
		}
		if boundary != to {
			return nil, fmt.Errorf("experience bins must include the entire drain")
		}
		for _, k := range countKeys {
			if sums[k] != totals[k] {
				return nil, fmt.Errorf("experience %s bin sum disagrees with totals.%s", id, k)
			}
		}
		if err := experienceRates(totals, to-from, true); err != nil {
			return nil, err
		}
		if err := canaries(a["canaries"], id, "steady"); err != nil {
			return nil, err
		}
		for _, raw := range a["canaries"].([]any) {
			cycleStart := raw.(map[string]any)["started"].(float64)
			if cycleStart < from || cycleStart > stop {
				return nil, fmt.Errorf("experience steady canary start must be within its active arm")
			}
		}
	}
	return &Experience{data: d}, nil
}

func experienceRates(m map[string]any, width float64, total bool) error {
	completed, success, rejected := m["completed"].(float64), m["successful"].(float64), m["http429"].(float64)
	timeouts, other := m["timeouts"].(float64), m["otherErrors"].(float64)
	overlap := success + rejected + timeouts + other - completed
	if overlap < 0 || overlap > math.Min(rejected, timeouts) || total && overlap != m["http429TimeoutOverlap"] {
		return fmt.Errorf("outcome counts do not partition completions with 429/timeout overlap")
	}
	for k, want := range map[string]float64{"successfulQPS": success / width, "completionQPS": completed / width} {
		if err := numberAt(m, k, true); err != nil {
			return err
		}
		if !experienceClose(m[k].(float64), want) {
			return fmt.Errorf("%s disagrees with count / actual interval width", k)
		}
	}
	if completed == 0 {
		if m["rejectedFraction"] != nil {
			return fmt.Errorf("empty completion bin requires null rejectedFraction")
		}
	} else {
		if err := numberAt(m, "rejectedFraction", true); err != nil {
			return err
		}
		v := m["rejectedFraction"].(float64)
		if v > 1 || math.Abs(v-rejected/completed) > 1e-12 {
			return fmt.Errorf("rejectedFraction must equal HTTP 429 / completed requests")
		}
	}
	return nil
}

func (e *Experience) match(c *Card, contention *Contention, resources *Resources) error {
	if e.data == nil || e.data["cardID"] != c.data["id"] {
		return fmt.Errorf("experience cardID must match card id")
	}
	var others []map[string]any
	if contention != nil {
		others = append(others, contention.data)
	}
	if resources != nil {
		others = append(others, resources.data)
	}
	for _, other := range others {
		for _, k := range []string{"window.start", "window.end"} {
			if at(e.data, k) != at(other, k) {
				return fmt.Errorf("experience and other evidence windows must match")
			}
		}
	}
	arms := at(c.data, "apfComparison.arms").([]any)
	if len(arms) != len(e.data["arms"].([]any)) {
		return fmt.Errorf("experience arm count must match card")
	}
	for _, raw := range e.data["arms"].([]any) {
		a := raw.(map[string]any)
		var cardArm map[string]any
		for _, raw := range arms {
			if candidate := raw.(map[string]any); candidate["id"] == a["id"] {
				cardArm = candidate
			}
		}
		if cardArm == nil || cardArm["shares"] != a["shares"] {
			return fmt.Errorf("experience arm %v must match card id and shares", a["id"])
		}
		for k, ck := range map[string]string{"start": "startEpoch", "stop": "stopEpoch"} {
			v, ok := cardArm[ck].(float64)
			if !ok || !experienceTime(a[k].(float64), v) {
				return fmt.Errorf("experience arm %v %s must match card", a["id"], k)
			}
		}
		if offered := at(c.data, "apfComparison.client.offeredQPS"); offered != nil && offered != a["offeredQPS"] {
			return fmt.Errorf("experience configured offeredQPS must match card")
		}
		for k, ck := range map[string]string{"offered": "offered", "dispatched": "dispatched", "httpAttempts": "httpAttempts", "successful": "successfulBodies", "http429": "http429", "timeouts": "timeouts", "generatorLimited": "generatorLimited", "http200Timeouts": "http200WithBodyTimeout"} {
			if want := cardArm[ck]; want != nil && want != at(a, "totals."+k) {
				return fmt.Errorf("experience %v totals.%s must match card", a["id"], k)
			}
		}
		if want, ok := cardArm["successfulQPSWallIncludingDrain"].(float64); ok && !experienceClose(at(a, "totals.successfulQPS").(float64), want) {
			return fmt.Errorf("experience whole-case successful QPS must match card")
		}
		completed, degraded := 0.0, 0.0
		softKnown := true
		for _, raw := range a["canaries"].([]any) {
			cycle := raw.(map[string]any)
			if cycle["status"] == "completed" {
				completed++
			}
			cycleDegraded, cycleKnown := false, true
			for _, name := range []string{"ack", "ready", "cleanup"} {
				s := at(cycle, "stages."+name).(map[string]any)
				d, dok := s["durationSeconds"].(float64)
				x, xok := s["expectedSeconds"].(float64)
				if dok && xok && d > x {
					cycleDegraded = true
				}
				cycleKnown = cycleKnown && dok && xok
				deadlineKey := map[string]string{"ack": "hardACKSeconds", "ready": "hardReadySeconds", "cleanup": "hardCleanupSeconds"}[name]
				if want := at(c.data, "apfComparison.protectedCanaries."+deadlineKey); want != nil && s["hardDeadlineSeconds"] != nil && want != s["hardDeadlineSeconds"] {
					return fmt.Errorf("experience %v %s hard deadline must match card", a["id"], name)
				}
			}
			if cycleDegraded {
				degraded++
			}
			softKnown = softKnown && (cycleDegraded || cycleKnown)
		}
		for k, got := range map[string]float64{"hardCompletedCanaries": completed, "softDegradedCanaries": degraded} {
			if k == "softDegradedCanaries" && !softKnown {
				continue
			}
			if want := cardArm[k]; want != nil && want != got {
				return fmt.Errorf("experience %v %s must match card", a["id"], k)
			}
		}
		if want := at(c.data, "apfComparison.protectedCanaries.perArm"); want != nil && want != float64(len(a["canaries"].([]any))) {
			return fmt.Errorf("experience canary count must match card")
		}
	}
	return nil
}

// Describe only a conjunction supported by every card arm, never by the card ID
// or by fast all-outcome latency. Missing data must not inherit this run's result.
func clientBehavior(c *Card) string {
	majority, nearDeadline, completed := true, true, true
	deadline, deadlineOK := at(c.data, "apfComparison.client.requestTimeoutSeconds").(float64)
	expected, expectedOK := at(c.data, "apfComparison.protectedCanaries.perArm").(float64)
	for _, raw := range at(c.data, "apfComparison.arms").([]any) {
		a := raw.(map[string]any)
		rejected, rok := a["http429"].(float64)
		dispatched, dok := a["dispatched"].(float64)
		list, lok := a["successfulLISTP95Seconds"].(float64)
		count, cok := a["hardCompletedCanaries"].(float64)
		majority = majority && rok && dok && dispatched > 0 && rejected/dispatched > .5
		nearDeadline = nearDeadline && lok && deadlineOK && deadline > 0 && list >= .8*deadline && list <= deadline
		completed = completed && cok && expectedOK && expected > 0 && count == expected
	}
	if majority && nearDeadline && completed {
		return "Client behavior: most requests shed, successful LISTs near " + number(deadline, 0) + "s tail, protected progress still completed."
	}
	return "Client behavior: see the recorded throughput, shedding, successful latency and protected-progress outcomes below; the combined degradation conclusion is unknown or not supported."
}

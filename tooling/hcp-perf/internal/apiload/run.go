// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package apiload

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/utils/clock"
)

type Options struct {
	OutputDir string
	Factory   func(namespace string) (*rest.Config, error)
	// Checkpoints freeze cumulative logical cohorts by scheduled offset. Observe
	// runs serially in the collector only after that cohort is fully settled.
	Checkpoints       []int
	ObserveCheckpoint func(Checkpoint) error
	// Test seam for a blocked evidence sync; production always uses file.Sync.
	syncEvidence func() error
}

type Checkpoint struct {
	AfterSeconds int       `json:"afterSeconds"`
	SettledAt    time.Time `json:"settledAt"`
	Counts
}

type Counts struct {
	Offered          int `json:"offered"`
	Dispatched       int `json:"dispatched"`
	GeneratorLimited int `json:"generatorLimited"`
	HTTPAttempts     int `json:"httpAttempts"`
	Completed        int `json:"completed"`
	Successful       int `json:"successful"`
	Rejected429      int `json:"rejected429"`
	OtherHTTP        int `json:"otherHTTPErrors"`
	TransportErrors  int `json:"transportErrors"`
	Timeouts         int `json:"timeouts"`
	Canceled         int `json:"canceled"`
	BodyLimitErrors  int `json:"bodyLimitErrors"`
	// A logical request contributes once, even if a 429 body also times out.
	DegradedRequests int `json:"degradedRequests"`
}

const RequestAccountingVersion = "v1"

// OperationResult separates useful latency by Kubernetes operation, rather than
// mixing fast rejections with successful GET, Pod LIST and ReplicaSet LIST work.
type OperationResult struct {
	APIVersion string `json:"apiVersion"`
	Resource   string `json:"resource"`
	Verb       string `json:"verb"`
	Counts
	SuccessfulBodyQPS    float64       `json:"successfulBodyQPS"`
	SuccessfulLatencyP50 time.Duration `json:"successfulLatencyP50Nanoseconds"`
	SuccessfulLatencyP95 time.Duration `json:"successfulLatencyP95Nanoseconds"`
	SuccessfulLatencyP99 time.Duration `json:"successfulLatencyP99Nanoseconds"`
}

type TargetResult struct {
	Target Target `json:"target"`
	Counts
}

type Result struct {
	Pacing                   Pacing       `json:"pacing"`
	Checkpoints              []Checkpoint `json:"checkpoints,omitempty"`
	RequestAccountingVersion string       `json:"requestAccountingVersion,omitempty"`
	Version                  string       `json:"version"`
	Status                   string       `json:"status"`
	Spec                     Spec         `json:"spec"`
	Started                  time.Time    `json:"started"`
	SchedulingStopped        time.Time    `json:"schedulingStopped"`
	Finished                 time.Time    `json:"finished"`
	IntendedQPS              int          `json:"intendedQPS"`
	ExecutedQPS              float64      `json:"executedQPS"`
	SuccessfulBodyQPS        float64      `json:"successfulBodyQPS"`
	Counts
	LatencyP50           time.Duration     `json:"latencyP50Nanoseconds"`
	LatencyP95           time.Duration     `json:"latencyP95Nanoseconds"`
	SuccessfulLatencyP50 time.Duration     `json:"successfulLatencyP50Nanoseconds"`
	SuccessfulLatencyP95 time.Duration     `json:"successfulLatencyP95Nanoseconds"`
	SuccessfulLatencyP99 time.Duration     `json:"successfulLatencyP99Nanoseconds"`
	Operations           []OperationResult `json:"operations"`
	Targets              []TargetResult    `json:"targets"`
}

// Event contains no URL, credential, response body, or raw error text. Target is
// the zero-based index in the normalized spec. Latency includes body consumption.
type Event struct {
	Sequence            int           `json:"sequence"`
	Target              int           `json:"target"`
	Scheduled           time.Time     `json:"scheduled"`
	Enqueued            time.Time     `json:"enqueued,omitempty"`
	QueueDelay          time.Duration `json:"queueDelayNanoseconds,omitempty"`
	Started             time.Time     `json:"started"`
	Finished            time.Time     `json:"finished"`
	Latency             time.Duration `json:"latencyNanoseconds"`
	HeaderLatency       time.Duration `json:"headerLatencyNanoseconds"`
	HTTPAttempts        int           `json:"httpAttempts"`
	StatusCode          int           `json:"statusCode"`
	ErrClass            string        `json:"errClass,omitempty"`
	ResponseBytes       int64         `json:"responseBytes"`
	APFFlowSchemaUID    string        `json:"apfFlowSchemaUID,omitempty"`
	APFPriorityLevelUID string        `json:"apfPriorityLevelUID,omitempty"`
	AuditID             string        `json:"auditID,omitempty"`
	RetryAfter          string        `json:"retryAfter,omitempty"`
}

type clientTarget struct {
	client *http.Client
	url    string
}

// Run performs no discovery, provisioning, retries, pagination, or writes to the
// cluster. A fresh evidence directory is mandatory; existing output is untouched.
func Run(ctx context.Context, spec Spec, opts Options) (result Result, retErr error) {
	spec, err := spec.Validate()
	if err != nil {
		return result, err
	}
	if opts.Factory == nil || opts.OutputDir == "" {
		return result, fmt.Errorf("namespace config factory and new output directory are required")
	}
	if (len(opts.Checkpoints) != 0) != (opts.ObserveCheckpoint != nil) || len(opts.Checkpoints) > 10 {
		return result, errors.New("checkpoints require a callback and at most ten checkpoints")
	}
	for i, seconds := range opts.Checkpoints {
		if seconds < 1 || seconds >= spec.DurationSeconds || (i > 0 && seconds <= opts.Checkpoints[i-1]) {
			return result, errors.New("checkpoints must be ordered unique seconds inside the admission window")
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := os.Mkdir(opts.OutputDir, 0700); err != nil {
		return result, fmt.Errorf("cannot create new evidence directory: %w", err)
	}
	clients := make(map[string]*http.Client, len(spec.Namespaces))
	bases := make(map[string]*url.URL, len(spec.Namespaces))
	defer func() {
		for _, c := range clients {
			c.CloseIdleConnections()
		}
	}()
	for _, ns := range spec.Namespaces {
		config, err := opts.Factory(ns)
		if err != nil || config == nil {
			return result, fmt.Errorf("cannot load namespace %q credentials", ns)
		}
		base, err := url.Parse(config.Host)
		if err != nil || base.Host == "" || base.Scheme != "https" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.RawPath != "" || config.Insecure {
			return result, fmt.Errorf("namespace %q requires a verified HTTPS API endpoint without userinfo, query, fragment, or escaped path", ns)
		}
		config = rest.CopyConfig(config)
		config.QPS, config.Burst, config.RateLimiter = -1, 0, nil
		config.Timeout = time.Duration(spec.RequestTimeoutSeconds) * time.Second
		config.UserAgent = "hcp-perf-api-load/v1"
		client, err := rest.HTTPClientFor(config)
		if err != nil {
			return result, fmt.Errorf("cannot create namespace %q HTTPS client", ns)
		}
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		clients[ns], bases[ns] = client, base
	}
	result = Result{Version: "v1", RequestAccountingVersion: RequestAccountingVersion, Status: "running", Spec: spec, IntendedQPS: spec.RatePerSecond}
	targets := make([]clientTarget, len(spec.Targets))
	operationIndices := make([]int, len(spec.Targets))
	operations := map[string]int{}
	totalWeight := 0
	for i, t := range spec.Targets {
		targets[i] = clientTarget{clients[t.Namespace], t.requestURL(bases[t.Namespace])}
		result.Targets = append(result.Targets, TargetResult{Target: t})
		key := t.APIVersion + "/" + t.Resource + "/" + t.Verb
		index, exists := operations[key]
		if !exists {
			index = len(result.Operations)
			operations[key] = index
			result.Operations = append(result.Operations, OperationResult{APIVersion: t.APIVersion, Resource: t.Resource, Verb: t.Verb})
		}
		operationIndices[i] = index
		totalWeight += t.Weight
	}
	f, err := os.OpenFile(filepath.Join(opts.OutputDir, "requests.ndjson"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return result, err
	}
	defer func() { retErr = errors.Join(retErr, f.Close()) }()
	syncFile := f.Sync
	if opts.syncEvidence != nil {
		syncFile = opts.syncEvidence
	}
	if err := writeJSON(filepath.Join(opts.OutputDir, "spec.json"), spec); err != nil {
		return result, err
	}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan Event, spec.MaxInFlight)
	events := make(chan Event, spec.MaxInFlight)
	slots := make(chan struct{}, spec.MaxInFlight)
	var workers sync.WaitGroup
	var evidenceWaitMax atomic.Int64
	for range spec.MaxInFlight {
		workers.Add(1)
		go func() {
			defer utilruntime.HandleCrash()
			defer workers.Done()
			for e := range jobs {
				e = execute(workCtx, targets[e.Target], time.Duration(spec.RequestTimeoutSeconds)*time.Second, e)
				waiting := time.Now()
				events <- e
				wait := int64(time.Since(waiting))
				for old := evidenceWaitMax.Load(); wait > old; old = evidenceWaitMax.Load() {
					if evidenceWaitMax.CompareAndSwap(old, wait) {
						break
					}
				}
				<-slots
			}
		}()
	}
	// The collector owns completion counters and disk IO. A slow disk occupies
	// worker slots, never blocks the offered schedule, and becomes generatorLimited.
	collected := make(chan error, 1)
	cohorts := make(chan Checkpoint, len(opts.Checkpoints))
	var screeningErr error    // Collector-owned until collected is received.
	var syncMax time.Duration // Collector-owned until collected is received.
	go func() {
		defer utilruntime.HandleCrash()
		writer := bufio.NewWriterSize(f, 64<<10)
		enc := json.NewEncoder(writer)
		syncEvidence := func() error {
			started := time.Now()
			err := errors.Join(writer.Flush(), syncFile())
			syncMax = max(syncMax, time.Since(started))
			return err
		}
		flush := time.NewTicker(time.Second)
		defer flush.Stop()
		var logErr error
		var histogram [30002]int // Millisecond upper bounds; last bucket is overflow.
		var successfulHistogram [30002]int
		operationHistograms := make([][30002]int, len(result.Operations))
		screens := make([]Checkpoint, len(opts.Checkpoints))
		frozen := make([]bool, len(screens))
		nextScreen := 0
		for i, seconds := range opts.Checkpoints {
			screens[i].AfterSeconds = seconds
		}
		for {
			select {
			case cohort := <-cohorts:
				for i := range screens {
					if screens[i].AfterSeconds == cohort.AfterSeconds {
						screens[i].Offered = cohort.Offered
						screens[i].Dispatched = cohort.Dispatched
						screens[i].GeneratorLimited = cohort.GeneratorLimited
						frozen[i] = true
					}
				}
			case e, ok := <-events:
				if !ok {
					result.LatencyP50 = quantile(histogram[:], result.Completed, 50)
					result.LatencyP95 = quantile(histogram[:], result.Completed, 95)
					result.SuccessfulLatencyP50 = quantile(successfulHistogram[:], result.Successful, 50)
					result.SuccessfulLatencyP95 = quantile(successfulHistogram[:], result.Successful, 95)
					result.SuccessfulLatencyP99 = quantile(successfulHistogram[:], result.Successful, 99)
					for i := range result.Operations {
						operation := &result.Operations[i]
						operation.SuccessfulLatencyP50 = quantile(operationHistograms[i][:], operation.Successful, 50)
						operation.SuccessfulLatencyP95 = quantile(operationHistograms[i][:], operation.Successful, 95)
						operation.SuccessfulLatencyP99 = quantile(operationHistograms[i][:], operation.Successful, 99)
					}
					collected <- errors.Join(logErr, syncEvidence())
					return
				}
				result.Counts.observe(e)
				for i, seconds := range opts.Checkpoints {
					if e.Sequence <= seconds*spec.RatePerSecond {
						screens[i].Counts.observe(e)
					}
				}
				result.Targets[e.Target].Counts.observe(e)
				result.Operations[operationIndices[e.Target]].Counts.observe(e)
				bucket := min(int((e.Latency+time.Millisecond-1)/time.Millisecond), len(histogram)-1)
				histogram[bucket]++
				if e.ErrClass == "" && e.StatusCode >= 200 && e.StatusCode < 300 {
					successfulHistogram[bucket]++
					operationHistograms[operationIndices[e.Target]][bucket]++
				}
				if logErr == nil {
					logErr = enc.Encode(e)
				}
			case <-flush.C:
				if logErr == nil {
					logErr = syncEvidence()
				}
			}
			for screeningErr == nil && logErr == nil && workCtx.Err() == nil && nextScreen < len(screens) && frozen[nextScreen] && screens[nextScreen].Completed == screens[nextScreen].Dispatched {
				checkpoint := screens[nextScreen]
				checkpoint.SettledAt = time.Now().UTC()
				// Persist the underlying request events before a decision can stop
				// siblings or publish its checkpoint receipt.
				logErr = syncEvidence()
				if logErr != nil {
					break
				}
				result.Checkpoints = append(result.Checkpoints, checkpoint)
				screeningErr = opts.ObserveCheckpoint(checkpoint)
				nextScreen++
				if screeningErr != nil {
					cancel()
				}
			}
			if logErr != nil {
				cancel()
			}
		}
	}()
	result.Started = time.Now()
	scheduled := make([]Counts, len(spec.Targets))
	rng := rand.New(rand.NewSource(*spec.Seed))
	offered := 0
	dispatched, checkpointIndex := 0, 0
	freezeCheckpoint := func() {
		if checkpointIndex < len(opts.Checkpoints) && offered == opts.Checkpoints[checkpointIndex]*spec.RatePerSecond {
			cohorts <- Checkpoint{AfterSeconds: opts.Checkpoints[checkpointIndex], Counts: Counts{Offered: offered, Dispatched: dispatched, GeneratorLimited: offered - dispatched}}
			checkpointIndex++
		}
	}
	maxSlots, maxJobs, maxEvents := 0, 0, 0
	result.Pacing, result.SchedulingStopped = pace(workCtx, clock.RealClock{}, result.Started, spec, func(sequence int, scheduledAt, _ time.Time, eligible bool) bool {
		maxSlots = max(maxSlots, len(slots))
		maxJobs = max(maxJobs, len(jobs))
		maxEvents = max(maxEvents, len(events))
		i := chooseTarget(rng, spec.Targets, totalWeight)
		scheduled[i].Offered++
		e := Event{Sequence: sequence, Target: i, Scheduled: scheduledAt}
		offered++
		if eligible {
			select {
			case slots <- struct{}{}:
				maxSlots = max(maxSlots, len(slots))
				e.Enqueued = time.Now()
				jobs <- e
				scheduled[i].Dispatched++
				dispatched++
				freezeCheckpoint()
				return true
			default:
			}
		}
		scheduled[i].GeneratorLimited++
		freezeCheckpoint()
		return false
	})
	close(jobs)
	workers.Wait()
	close(events)
	logErr := <-collected
	result.Pacing.MaxSlotsUsed = maxSlots
	result.Pacing.MaxJobsQueued = maxJobs
	result.Pacing.MaxEventsQueued = maxEvents
	result.Pacing.MaxEvidenceSync = syncMax
	result.Pacing.MaxWorkerEvidenceWait = time.Duration(evidenceWaitMax.Load())
	for i, c := range scheduled {
		result.Offered += c.Offered
		result.Dispatched += c.Dispatched
		result.GeneratorLimited += c.GeneratorLimited
		result.Targets[i].Offered = c.Offered
		result.Targets[i].Dispatched = c.Dispatched
		result.Targets[i].GeneratorLimited = c.GeneratorLimited
		operation := &result.Operations[operationIndices[i]]
		operation.Offered += c.Offered
		operation.Dispatched += c.Dispatched
		operation.GeneratorLimited += c.GeneratorLimited
	}
	result.Finished = time.Now()
	result.ExecutedQPS = float64(result.HTTPAttempts) / result.Finished.Sub(result.Started).Seconds()
	result.SuccessfulBodyQPS = float64(result.Successful) / result.Finished.Sub(result.Started).Seconds()
	for i := range result.Operations {
		result.Operations[i].SuccessfulBodyQPS = float64(result.Operations[i].Successful) / result.Finished.Sub(result.Started).Seconds()
	}
	result.Status = "completed"
	if screeningErr != nil {
		result.Status = "screening-stopped"
	}
	if ctx.Err() != nil {
		result.Status = "canceled"
	}
	if logErr != nil {
		result.Status = "evidenceError"
		logErr = fmt.Errorf("request evidence incomplete; harness failure, not HCP performance: %w", logErr)
	}
	return result, errors.Join(ctx.Err(), logErr, screeningErr, writeJSON(filepath.Join(opts.OutputDir, "result.json"), result))
}

var uidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func execute(ctx context.Context, target clientTarget, timeout time.Duration, e Event) Event {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	started := time.Now()
	if !e.Enqueued.IsZero() {
		e.QueueDelay = started.Sub(e.Enqueued)
	}
	e.Started = started.UTC()
	var attempts atomic.Int64
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteRequest: func(info httptrace.WroteRequestInfo) {
		if info.Err == nil {
			attempts.Add(1)
		}
	}})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.url, nil)
	if err == nil {
		req.Header.Set("Accept", "application/json")
		if err = ctx.Err(); err == nil {
			var resp *http.Response
			resp, err = target.client.Do(req)
			e.HeaderLatency = time.Since(started)
			if resp != nil {
				e.StatusCode = resp.StatusCode
				for header, dest := range map[string]*string{
					"X-Kubernetes-PF-FlowSchema-UID":    &e.APFFlowSchemaUID,
					"X-Kubernetes-PF-PriorityLevel-UID": &e.APFPriorityLevelUID,
					"Audit-ID":                          &e.AuditID,
				} {
					if value := resp.Header.Get(header); len(value) == 36 && uidPattern.MatchString(value) {
						*dest = value
					}
				}
				if value := resp.Header.Get("Retry-After"); len(value) > 0 && len(value) <= 64 {
					_, numberErr := strconv.ParseUint(value, 10, 32)
					_, dateErr := http.ParseTime(value)
					if numberErr == nil || dateErr == nil {
						e.RetryAfter = value
					}
				}
				// Stream and discard, including errors; never decode or retain bodies.
				var readErr error
				e.ResponseBytes, readErr = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxResponseBytes+1))
				err = errors.Join(err, readErr, resp.Body.Close())
				if e.ResponseBytes > MaxResponseBytes {
					e.ErrClass = "bodyLimit"
				}
			}
		}
	}
	if err != nil {
		var netErr net.Error
		switch {
		case errors.Is(err, context.Canceled):
			e.ErrClass = "canceled"
		case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
			e.ErrClass = "timeout"
		default:
			e.ErrClass = "transport"
		}
	}
	e.Finished = time.Now().UTC()
	e.Latency = time.Since(started)
	e.HTTPAttempts = int(attempts.Load())
	return e
}

func (c *Counts) observe(e Event) {
	c.Completed++
	c.HTTPAttempts += e.HTTPAttempts
	if e.StatusCode == http.StatusTooManyRequests || e.ErrClass == "timeout" {
		c.DegradedRequests++
	}
	switch {
	case e.StatusCode == http.StatusTooManyRequests:
		c.Rejected429++
	case e.StatusCode != 0 && (e.StatusCode < 200 || e.StatusCode >= 300):
		c.OtherHTTP++
	case e.StatusCode >= 200 && e.StatusCode < 300 && e.ErrClass == "":
		c.Successful++
	}
	switch e.ErrClass {
	case "transport":
		c.TransportErrors++
	case "timeout":
		c.Timeouts++
	case "canceled":
		c.Canceled++
	case "bodyLimit":
		c.BodyLimitErrors++
	}
}

func quantile(histogram []int, count, percentile int) time.Duration {
	if count == 0 {
		return 0
	}
	rank := (count*percentile + 99) / 100
	for i, n := range histogram {
		rank -= n
		if rank <= 0 {
			return time.Duration(i) * time.Millisecond
		}
	}
	return 0
}

func writeJSON(path string, value any) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return errors.Join(enc.Encode(value), f.Sync(), f.Close())
}

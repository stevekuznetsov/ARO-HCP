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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/adversary"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/apiload"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/credentials"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/experiment"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/metrics"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/record"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/seed"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/startup"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/workload"
)

type Outcome struct {
	Error              string               `json:"error,omitempty"`
	GuardFailure       *GuardFailure        `json:"guardFailure,omitempty"`
	Critical           CriticalStatus       `json:"critical"`
	Stability          StabilitySummary     `json:"stability"`
	CanaryFailures     int                  `json:"canaryFailures"`
	CanaryRecoveries   int                  `json:"canaryRecoveries"`
	Phase              string               `json:"phase"`
	State              string               `json:"state"`
	Started            time.Time            `json:"started"`
	Finished           time.Time            `json:"finished"`
	Identity           startup.Identity     `json:"identity"`
	CaptureReference   string               `json:"captureReference"`
	MetricsStatus      string               `json:"metricsStatus"`
	RecordStatus       string               `json:"recordStatus"`
	CleanupStatus      string               `json:"cleanupStatus"`
	PopulationRetained bool                 `json:"populationRetained"`
	Degraded           bool                 `json:"degraded"`
	ExecutionStatus    string               `json:"executionStatus"`
	Workload           Assessment           `json:"workload"`
	CaptureHistory     CaptureHistoryStatus `json:"captureHistory"`
}

type GuardFailure struct {
	At    time.Time `json:"at"`
	Error string    `json:"error"`
}

type guardFailure struct {
	once    sync.Once
	failure *GuardFailure
	err     error
}

// Capture the first cause before canceling siblings. Evidence I/O must never
// delay cancellation, and cancellation fallout must not replace its cause.
// Read the fields only after the guard workers have joined.
func (g *guardFailure) fail(ctx context.Context, cancel context.CancelCauseFunc, cause error, emit func(*GuardFailure) error) {
	g.once.Do(func() {
		if ctx.Err() != nil {
			return
		}
		g.failure = &GuardFailure{At: time.Now().UTC(), Error: cause.Error()}
		cancel(cause)
		g.err = errors.Join(cause, emit(g.failure))
	})
}

type journal struct {
	sync.Mutex
	dir  string
	file *os.File
}

func (j *journal) event(kind string, value any) error {
	j.Lock()
	defer j.Unlock()
	err := json.NewEncoder(j.file).Encode(struct {
		At    time.Time `json:"at"`
		Kind  string    `json:"kind"`
		Value any       `json:"value"`
	}{time.Now().UTC(), kind, value})
	return errors.Join(err, j.file.Sync())
}
func publish(dir, name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, name+".tmp"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err = errors.Join(err, f.Sync(), f.Close()); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), filepath.Join(dir, name)); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}
func wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(max(0, duration))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Run reserves new private output before any mutating API. Measurement is never
// replayed, even when setup failed. On process loss, absent outcome means
// interrupted/unknown; the iteration measurement policy blocks automatic retry.
func Run(ctx context.Context, spec Spec, dir string, client *http.Client) (out Outcome, err error) {
	if err := spec.ValidateRun(); err != nil {
		return out, err
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return out, err
	}
	parent, err := os.Open(filepath.Dir(dir))
	if err != nil {
		return out, err
	}
	if err := errors.Join(parent.Sync(), parent.Close()); err != nil {
		return out, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "events.ndjson"), os.O_CREATE|os.O_EXCL|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return out, err
	}
	j := &journal{dir: dir, file: f}
	defer f.Close()
	out = Outcome{Phase: "preflight", State: "running", Started: time.Now().UTC(), CaptureReference: spec.Capture.Reference, MetricsStatus: "not-exported", CleanupStatus: "not-needed", PopulationRetained: true}
	out.Workload = Assessment{Status: "not-evaluated", Violations: []string{}}
	if err := publish(dir, "spec.json", spec); err != nil {
		return out, err
	}
	if err := publish(dir, "status.json", out); err != nil {
		return out, err
	}
	deadlineCtx, stopDeadline := context.WithTimeout(ctx, spec.MaxDuration())
	defer stopDeadline()
	runCtx, cancelCause := context.WithCancelCause(deadlineCtx)
	cancel := func() { cancelCause(context.Canceled) }
	defer cancel()
	var children sync.WaitGroup
	captureHistory := &captureTracker{}
	g := &safety{}
	critical := &criticalMonitor{}
	var management startup.Clients
	var competitorIdentity adversary.RunStatus
	var competitorWorkers []competitorWorker
	competitorAttempted := false
	recorderDir := filepath.Join(dir, "record")
	defer func() {
		if runCtx.Err() != nil && (context.Cause(runCtx) != context.Canceled || ctx.Err() != nil) {
			err = errors.Join(err, context.Cause(runCtx))
		}
		cancel()
		children.Wait()
		out.CaptureHistory = captureHistory.status
		out.Stability = g.summary
		out.Critical = critical.status
		cleanup, stop := context.WithTimeout(context.Background(), 2*time.Minute)
		err = errors.Join(err, finalizeCapture(cleanup, spec, dir, client, management.Kubernetes, competitorAttempted, &competitorIdentity, &out, j))
		stop()
		out.RecordStatus = "unavailable"
		if health, healthErr := record.Health(recorderDir); healthErr == nil {
			out.RecordStatus = "recorded-not-lossless"
			if health.Final == nil || health.Final.APIErrors != 0 || health.Final.IncompleteStreams != 0 {
				out.RecordStatus = "partial"
			}
			err = errors.Join(err, j.event("record-final", health))
		}
		out.State = "completed"
		if out.ExecutionStatus == "" {
			out.ExecutionStatus = "failed"
		}
		if err != nil {
			out.State = "failed"
		}
		if ctx.Err() != nil {
			out.State = "interrupted"
			out.ExecutionStatus = "interrupted"
		}
		out.Finished = time.Now().UTC()
		if err != nil {
			out.Error = err.Error()
		}
		reportObservation(&out)
		persistErr := errors.Join(publish(dir, "outcome.json", out), publish(dir, "status.json", out))
		err = errors.Join(err, persistErr)
		var violation *WorkloadViolation
		if persistErr == nil && out.ExecutionStatus == "completed" && (out.CleanupStatus == "not-needed" || out.CleanupStatus == "stopped-verified") && errors.As(err, &violation) {
			err = &AssessedFailure{Err: err}
		}
	}()
	phase := func(name string) error {
		out.Phase = name
		return errors.Join(j.event("phase", name), publish(dir, "status.json", out))
	}
	management, err = startup.NewClients(spec.Startup.Kubeconfig, spec.Startup.AllowExecCredential)
	if err != nil {
		return out, errors.New("management client setup failed")
	}
	// Guest admin plugins are not permitted: this external credential is expected
	// to be created by the parent's explicit RP credential action.
	if _, err := startup.NewClients(spec.GuestKubeconfig); err != nil {
		return out, errors.New("guest credential validation failed")
	}
	admin, err := clientcmd.BuildConfigFromFlags("", spec.GuestKubeconfig)
	if err != nil || admin.Insecure {
		return out, errors.New("guest requires verified TLS")
	}
	admin.Timeout = 30 * time.Second
	guest, err := kubernetes.NewForConfig(admin)
	if err != nil {
		return out, errors.New("guest client setup failed")
	}
	g.livez = func(ctx context.Context) error {
		return guest.Discovery().RESTClient().Get().AbsPath("/livez").Do(ctx).Error()
	}
	guestDynamic, err := dynamic.NewForConfig(admin)
	if err != nil {
		return out, errors.New("guest identity client setup failed")
	}
	out.Identity, err = startup.Discover(runCtx, management, spec.Startup.ResourceID, startup.Identity{})
	if err != nil || out.Identity.HCPUID == "" {
		return out, errors.New("exact control-plane identity unavailable")
	}
	if err := verifyGuestIdentity(runCtx, guestDynamic, out.Identity.ClusterID); err != nil {
		return out, err
	}
	sample, err := g.check(runCtx, management, spec, out.Identity, true)
	if saveErr := j.event("initial-control-plane", sample); saveErr != nil {
		return out, saveErr
	}
	if err != nil {
		return out, errors.Join(err, j.event("preflight-failure", map[string]string{"stage": "control-plane", "error": err.Error()}))
	}
	allocation, err := guestCapacity(runCtx, guest, spec)
	if err != nil {
		return out, errors.Join(err, j.event("preflight-failure", map[string]string{"stage": "guest-capacity", "error": err.Error()}))
	}
	if err := j.event("guest-capacity", allocation); err != nil {
		return out, err
	}
	if err := captureHistory.gate(runCtx, client, spec.Capture, j); err != nil {
		return out, err
	}
	if err := noCompetitors(runCtx, management.Kubernetes); err != nil {
		return out, err
	}
	setupCtx, setupCancel := context.WithTimeout(runCtx, time.Duration(spec.SetupSeconds)*time.Second)
	defer setupCancel()
	// Existing populations cannot be adopted, including a reused canary ID.
	names := []string{spec.setup().CanaryNamespace()}
	for n := int64(0); n < (spec.Seed.Applications+19)/20; n++ {
		names = append(names, fmt.Sprintf("%s-%d", spec.Seed.NamespacePrefix, n))
	}
	if spec.ExistingPopulation != nil {
		if err := verifyPopulation(runCtx, guestDynamic, spec, out.Identity); err != nil {
			return out, err
		}
	} else {
		for _, namespace := range names {
			_, getErr := guest.CoreV1().Namespaces().Get(setupCtx, namespace, metav1.GetOptions{})
			if !apierrors.IsNotFound(getErr) {
				return out, errors.New("fresh namespace gate failed")
			}
		}
	}
	recordSpec := record.Spec{Version: "v1", Namespaces: []string{out.Identity.HCPNamespace}, IntervalSeconds: 5, DurationSeconds: int64(spec.MaxDuration() / time.Second), OutputDir: recorderDir}
	recordDone := make(chan error, 1)
	children.Add(1)
	go func() {
		defer utilruntime.HandleCrash()
		defer children.Done()
		_, recordErr := record.RecordWithLogClient(runCtx, management.Kubernetes, management.Kubernetes, recordSpec)
		recordDone <- recordErr
	}()
	for recorderReady(recorderDir, 45*time.Second) != nil {
		select {
		case <-recordDone:
			return out, errors.New("recorder exited before seed")
		default:
		}
		if err := wait(setupCtx, time.Second); err != nil {
			return out, err
		}
	}
	failure := &guardFailure{}
	failGuard := func(cause error) {
		failure.fail(runCtx, cancelCause, cause, func(value *GuardFailure) error {
			return publish(dir, "guard-failure.json", value)
		})
	}
	if spec.Competitor != nil {
		copy := *spec.Competitor
		copy.Run.TargetNamespace = out.Identity.HCPNamespace
		spec.Competitor = &copy
	}
	var competing atomic.Bool
	guardSpec, identity := spec, out.Identity
	children.Add(1)
	go func() {
		defer utilruntime.HandleCrash()
		defer children.Done()
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			started := time.Now()
			cycle, stop := context.WithTimeout(runCtx, 10*time.Second)
			current, checkErr := g.check(cycle, management, guardSpec, identity, false)
			if checkErr == nil {
				nodes, nodeErr := guest.CoreV1().Nodes().List(cycle, metav1.ListOptions{})
				checkErr = nodeErr
				if nodeErr == nil {
					checkErr = guestNodesSafe(nodes.Items, guardSpec)
				}
			}
			checkErr = errors.Join(checkErr, cycle.Err())
			stop()
			if checkErr != nil {
				failGuard(fmt.Errorf("safety inspection after %s (10s budget): %w", time.Since(started), checkErr))
			}
			checkErr = errors.Join(checkErr, j.event("safety", current), recorderReady(recorderDir, 45*time.Second))
			select {
			case <-recordDone:
				checkErr = errors.Join(checkErr, errors.New("recorder exited during run"))
			default:
			}
			if checkErr != nil {
				if runCtx.Err() != nil {
					return
				}
				failGuard(checkErr)
				return
			}
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	children.Add(1)
	go func() {
		defer utilruntime.HandleCrash()
		defer children.Done()
		for wait(runCtx, guardSpec.Startup.PollInterval) == nil {
			_, captureErr := captureHistory.check(runCtx, client, guardSpec.Capture, j)
			if captureErr != nil {
				failGuard(captureErr)
				return
			}
			if competing.Load() {
				if _, err := competitorReady(runCtx, management.Kubernetes, client, guardSpec, competitorIdentity, competitorWorkers, j); err != nil {
					failGuard(err)
					return
				}
			}
		}
	}()
	defer func() {
		cancel()
		children.Wait()
		if failure.failure != nil {
			out.GuardFailure = failure.failure
			err = errors.Join(err, failure.err)
			out.ExecutionStatus = "failed"
		}
	}()
	if spec.ExistingPopulation == nil {
		if err := phase("seed"); err != nil {
			return out, err
		}
		_, err = seed.Run(setupCtx, spec.Seed, seed.Options{KubeconfigPath: spec.GuestKubeconfig, OutputDir: filepath.Join(dir, "seed")})
		if err != nil {
			return out, fmt.Errorf("seed failed; population retained: %w", err)
		}
		if err := seed.ConfigureIdentities(setupCtx, guest, spec.Seed); err != nil {
			return out, errors.New("seed identity setup failed")
		}
		if _, err := workload.Configure(setupCtx, guest, spec.setup()); err != nil {
			return out, errors.New("workload setup failed")
		}
	}
	configs := map[string]*rest.Config{}
	if spec.Unloaded || spec.RestorePopulation {
		phaseName := "unload-population"
		if spec.RestorePopulation {
			phaseName = "restore-population"
		}
		if err := phase(phaseName); err != nil {
			return out, err
		}
		beforeSpec := spec
		raw, err := os.ReadFile(filepath.Join(spec.ExistingPopulation.Directory, "spec.json"))
		if err != nil {
			return out, err
		}
		var prior Spec
		if err := json.Unmarshal(raw, &prior); err != nil {
			return out, err
		}
		beforeSpec.Unloaded = prior.Unloaded
		before, err := populationInventory(setupCtx, guestDynamic, beforeSpec)
		if err != nil {
			return out, err
		}
		if err := publish(dir, "unloaded-before.json", before); err != nil {
			return out, err
		}
		if err := unloadPopulation(setupCtx, guest, spec, j, before); err != nil {
			return out, err
		}
		inventory, err := populationInventory(setupCtx, guestDynamic, spec)
		if err != nil {
			return out, err
		}
		if err := publish(dir, "unloaded-population.json", inventory); err != nil {
			return out, err
		}
	}
	clients := map[string]kubernetes.Interface{}
	for i, namespace := range names {
		account := seed.IdentityName
		if i == 0 {
			account = workload.CanaryName
			err = workload.VerifyCanaryIdentity(setupCtx, guest, spec.setup())
		} else {
			err = credentials.VerifySeedIdentity(setupCtx, guest, spec.Seed, int64(i-1))
		}
		if err != nil {
			return out, errors.New("scoped identity verification failed")
		}
		config, expires, mintErr := credentials.MintCredentials(setupCtx, guest, admin, namespace, account, spec.MaxDuration()+5*time.Minute)
		if mintErr != nil {
			return out, mintErr
		}
		if !expires.After(time.Now().Add(spec.MaxDuration())) {
			return out, errors.New("scoped token lifetime insufficient")
		}
		if err := j.event("credential-expiry", struct {
			Namespace  string    `json:"namespace"`
			ValidUntil time.Time `json:"validUntil"`
		}{namespace, expires}); err != nil {
			return out, err
		}
		configs[namespace] = config
		clients[namespace], err = kubernetes.NewForConfig(config)
		if err != nil {
			return out, errors.New("scoped client construction failed")
		}
	}
	canarySpec := workload.Spec{Namespace: names[0], Name: workload.CanaryName, Deadlines: experiment.CanaryDeadlines{Ack: 30 * time.Second, Ready: 120 * time.Second, Cleanup: 120 * time.Second}}
	canary := func(ctx context.Context, kind string) (workload.Result, error) {
		result, canaryErr := workload.RunCanary(ctx, clients[names[0]], canarySpec)
		if saveErr := j.event(kind, result); saveErr != nil {
			return result, saveErr
		}
		if kind == "measured-canary" || kind == "recovery-probe" {
			out.Degraded = out.Degraded || result.Degraded
		}
		if canaryErr != nil {
			if spec.StabilityPolicy == "observe-degradation" && (kind == "measured-canary" || kind == "recovery-probe") {
				out.CanaryFailures++
				out.Degraded = true
				started := time.Now()
				drained, drainErr := recoverCanary(ctx, clients[names[0]], canarySpec, result, canaryErr)
				if err := j.event("canary-recovery", struct {
					Original  workload.Result   `json:"original"`
					Drained   workload.Snapshot `json:"drained"`
					Duration  time.Duration     `json:"durationNanoseconds"`
					Recovered bool              `json:"recovered"`
				}{result, drained, time.Since(started), drainErr == nil}); err != nil {
					return result, err
				}
				if drainErr == nil {
					out.CanaryRecoveries++
					return result, nil
				}
				return result, errors.New("canary recovery unsafe or not drained within bound")
			}
			canaryErr = errors.New("canary hard failure")
		}
		return result, canaryErr
	}
	if err := phase("warmup-baseline"); err != nil {
		return out, err
	}
	if err := captureHistory.gate(setupCtx, client, spec.Capture, j); err != nil {
		return out, err
	}
	if _, err := canary(setupCtx, "warmup"); err != nil {
		return out, err
	}
	baseline := []experiment.CanarySample{}
	for range 5 {
		result, err := canary(setupCtx, "baseline")
		if err != nil {
			return out, err
		}
		baseline = append(baseline, experiment.CanarySample{Ack: result.Ack.Duration, Ready: result.Ready.Duration, Cleanup: result.Cleanup.Duration})
	}
	canarySpec.Expectations, err = experiment.CalculateCanaryDeadlines(baseline)
	if err != nil {
		return out, err
	}
	if spec.Competitor != nil {
		if err := captureHistory.gate(setupCtx, client, spec.Capture, j); err != nil {
			return out, err
		}
		planned, planErr := adversary.Discover(setupCtx, management.Kubernetes, spec.Competitor.Run)
		if err := j.event("competitor-plan", planned); err != nil {
			return out, err
		}
		if planErr != nil || !reflect.DeepEqual(planned.Plan.Nodes, spec.Competitor.Nodes) {
			return out, errors.New("competitor frozen placement/request plan mismatch")
		}
		competitorAttempted = true
		started, startErr := adversary.Start(setupCtx, management.Kubernetes, spec.Competitor.Run)
		competitorIdentity = started
		if startErr != nil || !reflect.DeepEqual(started.Plan.Nodes, spec.Competitor.Nodes) || !reflect.DeepEqual(started.Snapshot.TargetPods, planned.Snapshot.TargetPods) {
			return out, errors.New("competitor start failed or allocation changed")
		}
		for {
			workers, readyErr := competitorReady(setupCtx, management.Kubernetes, client, spec, competitorIdentity, competitorWorkers, j)
			if len(workers) != 0 && len(competitorWorkers) == 0 {
				competitorWorkers = workers
				if err := j.event("competitor-identities", workers); err != nil {
					return out, err
				}
			}
			if readyErr == nil {
				break
			}
			if err := wait(setupCtx, 5*time.Second); err != nil {
				return out, errors.Join(readyErr, err)
			}
		}
		competing.Store(true)
	}
	if err := captureHistory.gate(setupCtx, client, spec.Capture, j); err != nil {
		return out, err
	}
	// Baseline fits soft expectations. Hard deadlines remain fixed, and a
	// cycle admitted before the cutoff may finish during no-new-load recovery.
	// The phase receipts retain its actual start/end rather than clipping it.
	if err := phase("measurement"); err != nil {
		return out, err
	}
	if spec.StabilityPolicy == "observe-degradation" {
		g.Lock()
		g.observing = true
		g.summary = StabilitySummary{}
		g.Unlock()
		captureHistory.Lock()
		captureHistory.observing = true
		captureHistory.Unlock()
		children.Add(1)
		go func() {
			defer utilruntime.HandleCrash()
			defer children.Done()
			critical.run(runCtx, 5*time.Second, 4*time.Second, 15*time.Second, func(ctx context.Context) CriticalSample {
				return criticalSample(ctx, management.Kubernetes, spec, identity.HCPNamespace, g.livez)
			}, func(sample CriticalSample) error { return j.event("critical-availability", sample) }, failGuard)
		}()
	}
	loadCtx, stopLoad := context.WithCancel(runCtx)
	defer stopLoad()
	var load sync.WaitGroup
	var results Results
	var screened atomic.Bool
	loadErrors := make(chan error, 2)
	defer func() {
		stopLoad()
		load.Wait()
		close(loadErrors)
		for loadErr := range loadErrors {
			err = errors.Join(err, loadErr)
		}
		if err == nil {
			out.ExecutionStatus = "completed"
		} else {
			out.ExecutionStatus = "failed"
		}
		assessment, resultErr := ValidateResult(spec, results, out.Degraded)
		if spec.StabilityPolicy == "observe-degradation" {
			g.Lock()
			incidents := g.summary.UnhealthySamples > 0 || g.summary.Restarts > 0 || g.summary.OOMs > 0
			g.Unlock()
			if incidents {
				assessment.Status = "violated"
				assessment.Violations = append(assessment.Violations, "control-plane degradation observed during fixed window")
				resultErr = &WorkloadViolation{Violations: assessment.Violations}
			}
		}
		if out.CanaryFailures > 0 {
			assessment.Status = "violated"
			assessment.Violations = append(assessment.Violations, "measured canary cycles failed; observed through safe recovery")
			resultErr = &WorkloadViolation{Violations: assessment.Violations}
		}
		if screened.Load() {
			// A deliberate short rejection is never a full-window capacity result.
			assessment.Status = "screened-out"
			assessment.FullWindow = false
			for _, decision := range results.Screening {
				if decision.Status == "rejected" {
					c := decision.Checkpoint.Counts
					fraction := float64(c.DegradedRequests) / float64(c.Dispatched)
					assessment.RequestQuality = &RequestQualityAssessment{Status: "exceeded", CapacityStatus: "screened-out", MaxDegradedBasisPoints: decision.MaxDegradedBasisPoints, Dispatched: c.Dispatched, DegradedRequests: c.DegradedRequests, Rejected429: c.Rejected429, Timeouts: c.Timeouts, DegradedFraction: &fraction}
				}
			}
			resultErr = &WorkloadViolation{Violations: []string{"predeclared request quality screening rejected the point; full measurement not completed"}}
			assessment.Violations = resultErr.(*WorkloadViolation).Violations
		}
		out.Workload = assessment
		out.Degraded = out.Degraded || resultErr != nil
		if assessment.RequestQuality != nil && assessment.RequestQuality.Status == "degraded" {
			out.Degraded = true
		}
		saveErr := errors.Join(publish(dir, "results.json", results), j.event("workload-assessment", assessment))
		if saveErr != nil {
			out.ExecutionStatus = "failed"
		}
		err = errors.Join(err, resultErr, saveErr)
	}()
	if !spec.Unloaded {
		load.Add(1)
		go func() {
			defer utilruntime.HandleCrash()
			defer load.Done()
			result, loadErr := workload.RunTurnover(loadCtx, workload.TurnoverSpec{Version: "v1", GuestNodes: spec.Seed.GuestNodes, Applications: spec.Seed.Applications, NamespacePrefix: spec.Seed.NamespacePrefix, Seed: spec.ScheduleSeed, DurationSeconds: spec.MeasurementSeconds, TurnoverPeriodSeconds: 3600, RecoverySeconds: spec.RecoverySeconds, MaxOutstanding: spec.MaxOutstanding}, workload.TurnoverOptions{OutputDir: filepath.Join(dir, "turnover"), Factory: func(ns string) (kubernetes.Interface, error) {
				c := clients[ns]
				if c == nil {
					return nil, errors.New("no scoped client")
				}
				return c, nil
			}})
			if screened.Load() && errors.Is(loadErr, context.Canceled) {
				loadErr = nil
			}
			if spec.StabilityPolicy == "observe-degradation" && turnoverDrainFailure(result, loadErr) {
				// Intents are not retried or fabricated. Keep independent API load
				// running; final counters/debt still violate workload acceptance.
				loadErr = nil
			}
			if loadErr != nil {
				loadErr = errors.New("turnover failed")
				cancel()
			}
			results.Turnover = &result
			loadErrors <- errors.Join(loadErr, j.event("turnover-final", result))
		}()
	}
	if spec.API != nil {
		load.Add(1)
		go func() {
			defer utilruntime.HandleCrash()
			defer load.Done()
			options := apiload.Options{OutputDir: filepath.Join(dir, "api"), Factory: func(ns string) (*rest.Config, error) {
				config := configs[ns]
				if config == nil {
					return nil, errors.New("no scoped config")
				}
				return config, nil
			}}
			if policy := spec.RequestQualityPolicy; policy != nil && len(policy.ScreeningSeconds) != 0 {
				options.Checkpoints = append([]int(nil), policy.ScreeningSeconds...)
				options.ObserveCheckpoint = func(checkpoint apiload.Checkpoint) error {
					decision := screenRequests(*policy, checkpoint)
					results.Screening = append(results.Screening, decision)
					if err := j.event("request-quality-screen", decision); err != nil {
						return err
					}
					if decision.Status == "rejected" {
						screened.Store(true)
						stopLoad()
						return &screeningRejected{}
					}
					return nil
				}
			}
			result, loadErr := apiload.Run(loadCtx, *spec.API, options)
			var rejected *screeningRejected
			if errors.As(loadErr, &rejected) && result.Status != "evidenceError" {
				loadErr = nil
			}
			if loadErr != nil {
				loadErr = errors.New("API load failed")
				cancel()
			}
			results.API = &result
			loadErrors <- errors.Join(loadErr, j.event("api-final", result))
		}()
	}
	measurementEnd := time.Now().Add(time.Duration(spec.MeasurementSeconds) * time.Second)
	for time.Now().Before(measurementEnd) {
		if screened.Load() {
			break
		}
		_, probeErr := canary(runCtx, "measured-canary")
		if probeErr != nil {
			return out, probeErr
		}
		if spec.Competitor != nil {
			if _, err := competitorReady(runCtx, management.Kubernetes, client, spec, competitorIdentity, competitorWorkers, j); err != nil {
				return out, err
			}
		}
		if err := wait(loadCtx, min(time.Duration(spec.CanaryCadenceSeconds)*time.Second, time.Until(measurementEnd))); err != nil {
			if screened.Load() && runCtx.Err() == nil {
				break
			}
			return out, err
		}
	}
	if screened.Load() {
		measurementEnd = time.Now()
	}
	if err := phase("recovery"); err != nil {
		return out, err
	}
	if err := wait(runCtx, time.Until(measurementEnd.Add(time.Duration(spec.RecoverySeconds)*time.Second))); err != nil {
		return out, err
	}
	load.Wait()
	if _, err := canary(runCtx, "recovery-probe"); err != nil {
		return out, err
	}
	final, finalErr := g.check(runCtx, management, spec, out.Identity, false)
	if finalErr != nil || (!final.Healthy && spec.StabilityPolicy != "observe-degradation") {
		return out, errors.New("recovery ended without current control-plane health")
	}
	if err := j.event("recovery-control-plane", final); err != nil {
		return out, err
	}
	population, err := populationInventory(runCtx, guestDynamic, spec)
	if err != nil {
		return out, err
	}
	if err := publish(dir, "population.json", population); err != nil {
		return out, err
	}
	return out, nil
}

// Cleanup exhaustion must not consume the export's independent budget.
func finalizeCapture(cleanup context.Context, spec Spec, dir string, client *http.Client, kube kubernetes.Interface, attempted bool, identity *adversary.RunStatus, out *Outcome, j *journal) error {
	var cleanupErr error
	if attempted {
		cleanupErr = stopCompetitor(cleanup, kube, spec.Competitor.Run, identity)
		out.CleanupStatus = "stopped-verified"
		if cleanupErr != nil {
			out.CleanupStatus = "unverified-load-may-remain"
		}
	}
	exportCtx, stop := context.WithTimeout(context.Background(), 2*time.Minute)
	defer stop()
	manifest, exportErr := metrics.Export(exportCtx, client, spec.Capture.Endpoint, spec.Capture.Queries, out.Started, time.Now().UTC(), 15*time.Second, filepath.Join(dir, "metrics"))
	out.MetricsStatus = "exported-not-qualified"
	if exportErr != nil {
		out.MetricsStatus = "partial-or-failed"
	}
	for _, query := range manifest.Queries {
		if query.Status != metrics.StatusSuccess {
			out.MetricsStatus = "partial-or-failed"
		}
	}
	// Capture gaps remain distinct from workload/harness failure.
	return errors.Join(cleanupErr, j.event("metrics-final", manifest))
}

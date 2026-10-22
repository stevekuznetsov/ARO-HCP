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

// Package record provides read-only, namespace-scoped Kubernetes polling and
// opt-in watch/raw-log recording to a caller-provided filesystem (such as a
// mounted PVC). Neither polling nor live streams are a lossless historical audit.
package record

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sutilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
)

// MaxObservationBytes bounds the combined snapshot streams. Checkpoints and the
// terminal summary are separate so reaching this limit can still be reported.
const MaxObservationBytes int64 = 8 << 30

var resources = []string{"pods", "events", "deployments", "statefulsets"}

type Spec struct {
	Version         string   `json:"version"`
	Namespaces      []string `json:"namespaces"`
	IntervalSeconds int64    `json:"intervalSeconds"`
	DurationSeconds int64    `json:"durationSeconds"`
	OutputDir       string   `json:"outputDir"`
	EnableWatch     bool     `json:"enableWatch,omitempty"`
	EnableLogs      bool     `json:"enableLogs,omitempty"`
	// RawLogs acknowledges that log bodies are unsanitized and can contain secrets.
	RawLogs                  bool  `json:"rawLogs,omitempty"`
	MaxStreamBytes           int64 `json:"maxStreamBytes,omitempty"`           // per live file; zero defaults to 1 GiB
	MaxObservationBytes      int64 `json:"maxObservationBytes,omitempty"`      // combined poll files; zero defaults to 1 GiB
	MaxConcurrentLogs        int   `json:"maxConcurrentLogs,omitempty"`        // per worker; zero defaults to 256
	LogRequestQPS            int64 `json:"logRequestQPS,omitempty"`            // aggregate per worker; zero defaults to 20
	LogRequestBurst          int64 `json:"logRequestBurst,omitempty"`          // zero defaults to 40
	StreamSyncIntervalMillis int64 `json:"streamSyncIntervalMillis,omitempty"` // zero defaults to 1000
}

func (s Spec) withDefaults() Spec {
	if s.StreamSyncIntervalMillis == 0 {
		s.StreamSyncIntervalMillis = 1000
	}
	if s.MaxStreamBytes == 0 {
		s.MaxStreamBytes = 1 << 30
	}
	if s.MaxObservationBytes == 0 {
		s.MaxObservationBytes = 1 << 30
	}
	if s.MaxConcurrentLogs == 0 {
		s.MaxConcurrentLogs = defaultLogWorkers
	}
	if s.LogRequestQPS == 0 {
		s.LogRequestQPS = 20
	}
	if s.LogRequestBurst == 0 {
		s.LogRequestBurst = 40
	}
	return s
}

// Validate does not contact Kubernetes or require namespaces to exist. Mapping
// future HCPs to explicit namespace names is the caller's responsibility.
func (s Spec) Validate() error {
	if s.Version != "v1" {
		return errors.New("record version must be v1")
	}
	if len(s.Namespaces) == 0 {
		return errors.New("namespaces must explicitly name at least one namespace")
	}
	seen := map[string]bool{}
	for _, namespace := range s.Namespaces {
		if len(validation.IsDNS1123Label(namespace)) != 0 || seen[namespace] {
			return errors.New("namespaces must be unique nonempty DNS labels")
		}
		seen[namespace] = true
	}
	if s.IntervalSeconds < 5 || s.IntervalSeconds > 300 {
		return errors.New("intervalSeconds must be between 5 and 300")
	}
	if s.DurationSeconds < 1 || s.DurationSeconds > 86400 {
		return errors.New("durationSeconds must be between 1 and 86400")
	}
	if s.OutputDir == "" {
		return errors.New("outputDir must explicitly name a new directory with an existing parent")
	}
	if s.MaxObservationBytes < 0 || s.MaxObservationBytes > MaxObservationBytes {
		return errors.New("maxObservationBytes must be 0 (default 1 GiB) or 1..8589934592 bytes across poll files")
	}
	if err := s.validateStreams(); err != nil {
		return err
	}
	return nil
}

// Result is suitable for JSON stdout by the caller; Record itself never prints.
// Samples counts complete namespace/resource snapshots, including empty lists.
// Pages/entries can include incomplete snapshots; only a complete observation
// advances LastSuccess. All streams are required, even if a namespace is absent.
type Result struct {
	Version           string         `json:"version"`
	OutputDir         string         `json:"outputDir"`
	Mode              string         `json:"mode"`
	IntervalSeconds   int64          `json:"intervalSeconds"`
	StartedAt         time.Time      `json:"startedAt"`
	EndedAt           time.Time      `json:"endedAt"`
	Polls             int64          `json:"polls"`
	Samples           int64          `json:"samples"`
	APIErrors         int64          `json:"apiErrors"`
	IncompleteStreams int64          `json:"incompleteStreams"`
	ObservationBytes  int64          `json:"observationBytes"`
	Shutdown          string         `json:"shutdown"` // running, duration, canceled, error
	Streams           []StreamResult `json:"streams"`
	Live              *LiveSummary   `json:"live,omitempty"`
}

type StreamResult struct {
	Namespace   string     `json:"namespace"`
	Resource    string     `json:"resource"`
	Samples     int64      `json:"samples"`
	Pages       int64      `json:"pages"`
	Entries     int64      `json:"entries"`
	APIErrors   int64      `json:"apiErrors"`
	LastSuccess *time.Time `json:"lastSuccess"`
}

// Observation identifies pages of one poll's namespace/resource snapshot. Only
// a subsequent complete record makes the snapshot complete. Continue tokens and
// raw API errors are never persisted. atTime is the end of the page's API call.
type Observation struct {
	Version   string    `json:"version"`
	Mode      string    `json:"mode"`
	AtTime    time.Time `json:"atTime"`
	Poll      int64     `json:"poll"`
	Namespace string    `json:"namespace"`
	Resource  string    `json:"resource"`
	Type      string    `json:"type"` // page, complete, apiError, interrupted
	Page      int64     `json:"page"`
	Entries   []any     `json:"entries"`
	Error     *APIError `json:"error,omitempty"`
}

// APIError deliberately excludes messages, URLs, response bodies and arbitrary
// server-provided reason strings, which can contain credentials.
type APIError struct {
	Reason string `json:"reason"`
	Code   int32  `json:"code,omitempty"`
	Stage  string `json:"requestStage,omitempty"`
}

// Checkpoint is appended and synced after each poll, then once at shutdown.
// Readers recovering from a crash should ignore any unterminated final line.
// ScheduledAt stays on the original cadence; missed ticks are skipped, not
// replayed. Poll timing exposes slow API calls and skipped sampling intervals.
type Checkpoint struct {
	Type           string    `json:"type"`
	AtTime         time.Time `json:"atTime"`
	ScheduledAt    time.Time `json:"scheduledAt"`
	PollStartedAt  time.Time `json:"pollStartedAt"`
	PollFinishedAt time.Time `json:"pollFinishedAt"`
	DurationMillis int64     `json:"durationMillis"`
	SkippedTicks   int64     `json:"skippedTicks"`
	Result         Result    `json:"result"`
}

// Record polls immediately, then at a fixed cadence. Opt-in live workers run
// alongside polling, share its deadline, and are joined before finalization.
// Every List has a timeout of at most 30s, bounded by ctx and the run deadline.
// API failures are recorded and retried at the next poll, but make the eventual
// return non-nil. Cancellation returns ctx.Err() after best-effort finalization.
// Storage failures stop recording immediately; Result still describes progress.
//
// The new directory is 0700 and files are 0600. manifest.json is immutable;
// resource NDJSON files and checkpoints.ndjson are append-only. summary.json is
// written on shutdown. Files and directory entries are synced before returning
// and at each checkpoint. Durability requires a filesystem that honors fsync;
// neither Kubernetes calls nor local filesystem I/O can force a broken server
// or filesystem to honor cancellation. This function creates no API resources.
func Record(ctx context.Context, client kubernetes.Interface, spec Spec) (result Result, retErr error) {
	return RecordWithLogClient(ctx, client, client, spec)
}

// RecordWithLogClient isolates log GETs (including identity checks) from the
// observation client's polling, LIST/WATCH and log discovery request budget.
// Callers supplying clients own their rate limits; NewClients applies Spec's budget.
func RecordWithLogClient(ctx context.Context, client, logClient kubernetes.Interface, spec Spec) (result Result, retErr error) {
	result = Result{Version: "v1", OutputDir: spec.OutputDir, Mode: "poll", IntervalSeconds: spec.IntervalSeconds, Shutdown: "error"}
	if err := spec.Validate(); err != nil {
		return result, err
	}
	spec = spec.withDefaults()
	if client == nil || (spec.EnableLogs && logClient == nil) {
		return result, errors.New("Kubernetes client is required")
	}
	path, err := filepath.Abs(spec.OutputDir)
	if err != nil {
		return result, err
	}
	result.OutputDir = path
	if err := os.Mkdir(path, 0700); err != nil {
		return result, fmt.Errorf("create new output directory: %w", err)
	}
	result.StartedAt = time.Now().UTC()
	result.Shutdown = "running"
	for _, namespace := range spec.Namespaces {
		for _, resource := range resources {
			result.Streams = append(result.Streams, StreamResult{Namespace: namespace, Resource: resource})
		}
	}
	runCtx, cancel := context.WithDeadlineCause(ctx, result.StartedAt.Add(time.Duration(spec.DurationSeconds)*time.Second), errRecordingDuration)
	defer cancel()

	files := map[string]*os.File{}
	var checkpoint Checkpoint
	// Install finalization before opening files so initialization failures also
	// leave a partial summary whenever the filesystem remains writable.
	defer func() {
		result.EndedAt = time.Now().UTC()
		if ctx.Err() != nil {
			result.Shutdown = "canceled"
			retErr = errors.Join(retErr, ctx.Err())
		} else if retErr != nil {
			result.Shutdown = "error"
		} else {
			result.Shutdown = "duration"
		}
		if result.APIErrors > 0 {
			retErr = errors.Join(retErr, fmt.Errorf("recording encountered %d API errors", result.APIErrors))
		}
		for _, stream := range result.Streams {
			if stream.Samples == 0 {
				result.IncompleteStreams++
			}
		}
		if result.IncompleteStreams > 0 {
			retErr = errors.Join(retErr, fmt.Errorf("%d required streams have no complete snapshot", result.IncompleteStreams))
		}
		// Data must reach storage before a checkpoint claims it was recorded.
		for _, resource := range resources {
			if f := files[resource]; f != nil {
				retErr = errors.Join(retErr, f.Sync())
			}
		}
		if retErr != nil && result.Shutdown == "duration" {
			result.Shutdown = "error"
		}
		checkpoint.Type, checkpoint.AtTime, checkpoint.Result = "shutdown", result.EndedAt, result
		if f := files["checkpoints"]; f != nil {
			_, err := appendJSON(f, checkpoint)
			retErr = errors.Join(retErr, err, f.Sync())
		}
		for _, f := range files {
			retErr = errors.Join(retErr, f.Close())
		}
		if retErr != nil && result.Shutdown != "canceled" {
			result.Shutdown = "error"
		}
		retErr = errors.Join(retErr, writeNewJSON(filepath.Join(path, "summary.json"), result))
		retErr = errors.Join(retErr, syncDirectory(path), syncDirectory(filepath.Dir(path)))
		if retErr != nil && result.Shutdown != "canceled" {
			result.Shutdown = "error"
		}
	}()

	manifest := struct {
		Spec                Spec      `json:"spec"`
		Mode                string    `json:"mode"`
		StartedAt           time.Time `json:"startedAt"`
		Resources           []string  `json:"resources"`
		PageLimit           int64     `json:"pageLimit"`
		MaxObservationBytes int64     `json:"maxObservationBytes"`
		Limitations         string    `json:"limitations"`
	}{spec, "poll", result.StartedAt, resources, 500, spec.MaxObservationBytes,
		"Polling can miss short-lived objects and transitions. Allowlisted operational fields only; no labels, annotations, env, commands, args, messages, logs, or Secret bodies. Names, images and reason codes are retained, not a guarantee against secrets embedded in those fields. Filesystem I/O and server compliance with pagination/timeouts are external constraints."}
	if spec.EnableWatch || spec.EnableLogs {
		manifest.Limitations = "Poll snapshots: " + manifest.Limitations + " Live streams: " + liveLimitations
	}
	if err := writeNewJSON(filepath.Join(path, "manifest.json"), manifest); err != nil {
		return result, err
	}
	for _, name := range append([]string{"checkpoints"}, resources...) {
		f, err := os.OpenFile(filepath.Join(path, name+".ndjson"), os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			return result, err
		}
		files[name] = f
	}
	if err := errors.Join(syncDirectory(path), syncDirectory(filepath.Dir(path))); err != nil {
		return result, err
	}
	if spec.EnableWatch || spec.EnableLogs {
		type outcome struct {
			summary LiveSummary
			err     error
		}
		done := make(chan outcome, 1)
		go func() {
			defer k8sutilruntime.HandleCrash()
			summary, err := runStreamsWithLogClient(runCtx, client, logClient, spec)
			done <- outcome{summary, err}
		}()
		defer func() {
			cancel()
			live := <-done
			result.Live = &live.summary
			retErr = errors.Join(retErr, live.err)
		}()
	}

	interval := time.Duration(spec.IntervalSeconds) * time.Second
	scheduled := result.StartedAt
	for runCtx.Err() == nil {
		result.Polls++
		checkpoint = Checkpoint{Type: "poll", ScheduledAt: scheduled, PollStartedAt: time.Now().UTC()}
		for i := range result.Streams {
			stream := &result.Streams[i]
			if runCtx.Err() != nil {
				break
			}
			continuation := ""
			for page := int64(1); runCtx.Err() == nil; page++ {
				callCtx, callCancel := context.WithTimeout(runCtx, 30*time.Second)
				entries, next, callErr := listPage(callCtx, client, stream.Resource, stream.Namespace, metav1.ListOptions{Limit: 500, Continue: continuation})
				callCancel()
				observation := Observation{Version: "v1", Mode: "poll", AtTime: time.Now().UTC(), Poll: result.Polls,
					Namespace: stream.Namespace, Resource: stream.Resource, Type: "page", Page: page, Entries: entries}
				if callErr != nil {
					observation.Type, observation.Error = "apiError", classifyError(callErr)
					if runCtx.Err() != nil && (errors.Is(callErr, context.Canceled) || errors.Is(callErr, context.DeadlineExceeded) || errors.Is(callErr, context.Cause(runCtx))) {
						observation.Type, observation.Error = "interrupted", nil
					} else {
						result.APIErrors++
						stream.APIErrors++
					}
				}
				n, err := appendObservation(files[stream.Resource], observation, result.ObservationBytes, spec.MaxObservationBytes)
				result.ObservationBytes += n
				if err != nil {
					return result, err
				}
				if callErr != nil {
					break
				}
				stream.Pages++
				stream.Entries += int64(len(entries))
				if next == "" {
					observation.Type, observation.Entries = "complete", nil
					n, err := appendObservation(files[stream.Resource], observation, result.ObservationBytes, spec.MaxObservationBytes)
					result.ObservationBytes += n
					if err != nil {
						return result, err
					}
					stream.Samples++
					stream.LastSuccess = &observation.AtTime
					result.Samples++
					break
				}
				continuation = next
			}
		}
		checkpoint.PollFinishedAt = time.Now().UTC()
		checkpoint.DurationMillis = checkpoint.PollFinishedAt.Sub(checkpoint.PollStartedAt).Milliseconds()
		for _, resource := range resources {
			if err := files[resource].Sync(); err != nil {
				return result, err
			}
		}
		scheduled = scheduled.Add(interval)
		if now := time.Now(); !scheduled.After(now) {
			checkpoint.SkippedTicks = int64(now.Sub(scheduled)/interval) + 1
			scheduled = scheduled.Add(time.Duration(checkpoint.SkippedTicks) * interval)
		}
		checkpoint.AtTime, checkpoint.Result = time.Now().UTC(), result
		if _, err := appendJSON(files["checkpoints"], checkpoint); err != nil {
			return result, err
		}
		if err := files["checkpoints"].Sync(); err != nil {
			return result, err
		}
		timer := time.NewTimer(time.Until(scheduled))
		select {
		case <-runCtx.Done():
		case <-timer.C:
		}
		timer.Stop()
	}
	return result, nil
}

func appendObservation(f *os.File, observation Observation, used, limit int64) (int64, error) {
	data, err := json.Marshal(observation)
	if err != nil {
		return 0, err
	}
	if int64(len(data))+1 > limit-used {
		return 0, errors.New("observation byte limit reached")
	}
	return appendLine(f, data)
}

func appendJSON(f *os.File, value any) (int64, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return 0, err
	}
	return appendLine(f, data)
}

func appendLine(f *os.File, data []byte) (int64, error) {
	data = append(data, '\n')
	n, err := f.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return int64(n), err
}

func writeNewJSON(path string, value any) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = appendJSON(f, value)
	return errors.Join(err, f.Sync(), f.Close())
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func classifyError(err error) *APIError {
	result := &APIError{Reason: "apiFailure"}
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		result.Code = status.Status().Code
	}
	switch {
	// client-go has no typed limiter error. Match only its fixed local prefix,
	// never a server status/message, and never persist the original error.
	case status == nil && strings.Contains(err.Error(), "client rate limiter Wait returned an error:"):
		result.Reason = "clientRateLimited"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, errStreamRenewal), apierrors.IsTimeout(err), apierrors.IsServerTimeout(err):
		result.Reason = "timeout"
	case apierrors.IsNotFound(err):
		result.Reason = "notFound"
	case apierrors.IsForbidden(err):
		result.Reason = "forbidden"
	case apierrors.IsUnauthorized(err):
		result.Reason = "unauthorized"
	case apierrors.IsTooManyRequests(err):
		result.Reason = "tooManyRequests"
	case apierrors.IsResourceExpired(err):
		result.Reason = "expired"
	}
	return result
}

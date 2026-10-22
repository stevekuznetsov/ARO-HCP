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

package record

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sutilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
)

type podLogOpener func(context.Context, kubernetes.Interface, string, string, *corev1.PodLogOptions) (io.ReadCloser, error)

func openPodLogs(ctx context.Context, client kubernetes.Interface, ns, name string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
	return client.CoreV1().Pods(ns).GetLogs(name, opts).Stream(ctx)
}

type logKey struct {
	uid       types.UID
	container string
	restart   int32
	previous  bool
}

type logTask struct {
	key             logKey
	namespace, pod  string
	observedRestart int32
	terminal        bool
	since           *time.Time
}

type logOutcome struct {
	key                            logKey
	since                          *time.Time
	done                           bool
	err                            error
	opened, complete, canceled     bool
	requests, streamsOpened, lines int64
}

// Discovery owns the bounded cache; workers receive copies and return outcomes.
// No worker can mutate another worker's progress or the discovery cache.
func recordLogs(ctx context.Context, client kubernetes.Interface, spec Spec, f *liveFile, open podLogOpener) error {
	return recordLogsWithLogClient(ctx, client, client, spec, f, open)
}

func recordLogsWithLogClient(ctx context.Context, client, logClient kubernetes.Interface, spec Spec, f *liveFile, open podLogOpener) (retErr error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type progress struct {
		active, done bool
		since        *time.Time
		out          logOutcome
		attempted    bool
	}
	cache := map[logKey]progress{}
	limit := spec.withDefaults().MaxConcurrentLogs
	completed := make(chan logOutcome, limit)
	active := 0
	collect := func(out logOutcome) {
		cache[out.key] = progress{done: out.done, since: out.since, out: out, attempted: true}
		active--
		retErr = errors.Join(retErr, out.err)
	}
	defer func() {
		cancel()
		for active > 0 {
			collect(<-completed)
		}
		counts := &LogLifecycle{Discovered: int64(len(cache))}
		for _, state := range cache {
			if !state.attempted {
				continue
			}
			counts.Attempted++
			out := state.out
			counts.Requests += out.requests
			counts.StreamsOpened += out.streamsOpened
			counts.Lines += out.lines
			if out.opened {
				counts.Opened++
			}
			switch {
			case out.err != nil:
				counts.Failed++
			case out.complete:
				counts.Completed++
				if out.lines == 0 {
					counts.CompletedEmpty++
				}
			case out.canceled:
				counts.Canceled++
			default:
				counts.Failed++
			}
		}
		f.mu.Lock()
		f.stats.Lifecycle = counts
		f.mu.Unlock()
		finalCtx, finalCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer finalCancel()
		retErr = errors.Join(retErr, f.writeContext(finalCtx, StreamObservation{Type: "captureWindowEnded", Resource: "pods/log", PotentialGap: true}))
	}()
	for ctx.Err() == nil && !f.failed() {
		for _, ns := range spec.Namespaces {
			continuation := ""
			for ctx.Err() == nil && !f.failed() {
				callCtx, callCancel := context.WithTimeout(ctx, 30*time.Second)
				pods, err := client.CoreV1().Pods(ns).List(callCtx, metav1.ListOptions{Limit: 500, Continue: continuation})
				callCancel()
				if err != nil {
					if ctx.Err() == nil {
						detail := classifyError(err)
						detail.Stage = "discoverPods"
						_ = f.write(StreamObservation{Type: "error", Namespace: ns, Resource: "pods/log", Error: detail})
					}
					break
				}
				for _, p := range pods.Items {
					for _, statuses := range [][]corev1.ContainerStatus{p.Status.InitContainerStatuses, p.Status.ContainerStatuses, p.Status.EphemeralContainerStatuses} {
						for _, status := range statuses {
							if status.State.Running == nil && status.State.Terminated == nil && status.RestartCount == 0 {
								continue
							}
							for _, previous := range []bool{true, false} {
								if ctx.Err() != nil {
									break
								}
								if previous && status.RestartCount == 0 {
									continue
								}
								if !previous && status.State.Running == nil && status.State.Terminated == nil {
									continue
								}
								restart := status.RestartCount
								if previous {
									restart--
								}
								key := logKey{p.UID, status.Name, restart, previous}
								base := StreamObservation{Namespace: ns, Resource: "pods/log", Pod: p.Name, PodUID: p.UID, Container: status.Name, Restart: restart, Previous: previous}
								for len(completed) > 0 {
									collect(<-completed)
								}
								state, exists := cache[key]
								if state.active || state.done {
									continue
								}
								admitted := exists || (p.UID != "" && len(cache) < 4096)
								if admitted && !exists {
									cache[key] = state
								}
								if !admitted || active >= limit {
									base.Type, base.Error = "gap", &APIError{Reason: "logCapacity"}
									if err := f.write(base); err != nil {
										return err
									}
									continue
								}
								if !state.attempted && previous && restart > 0 {
									base.Type, base.Error = "gap", &APIError{Reason: "olderRestartsUnavailable"}
									if err := f.write(base); err != nil {
										return err
									}
								}
								cache[key] = progress{active: true, since: state.since}
								active++
								task := logTask{key: key, namespace: ns, pod: p.Name, observedRestart: status.RestartCount,
									terminal: previous || status.State.Terminated != nil, since: state.since}
								go func() {
									defer k8sutilruntime.HandleCrash()
									completed <- followPodLogs(ctx, logClient, task, f, open)
								}()
							}
						}
					}
				}
				continuation = pods.Continue
				if continuation == "" {
					break
				}
			}
		}
		timer := time.NewTimer(time.Duration(spec.IntervalSeconds) * time.Second)
		waiting := true
		for waiting && ctx.Err() == nil && !f.failed() {
			select {
			case out := <-completed:
				collect(out)
			case <-ctx.Done():
				waiting = false
			case <-timer.C:
				waiting = false
			}
		}
		timer.Stop()
	}
	// Join before evaluating retErr, including errors from cancellation cleanup.
	cancel()
	for active > 0 {
		collect(<-completed)
	}
	if f.failed() {
		retErr = errors.Join(retErr, errors.New("log storage failed"))
	}
	return retErr
}

func followPodLogs(ctx context.Context, client kubernetes.Interface, task logTask, f *liveFile, open podLogOpener) logOutcome {
	return followPodLogsWithRenewal(ctx, client, task, f, open, 30*time.Second)
}

func followPodLogsWithRenewal(ctx context.Context, client kubernetes.Interface, task logTask, f *liveFile, open podLogOpener, renewal time.Duration) (out logOutcome) {
	out = logOutcome{key: task.key, since: task.since}
	base := StreamObservation{Namespace: task.namespace, Resource: "pods/log", Pod: task.pod, PodUID: task.key.uid,
		Container: task.key.container, Restart: task.key.restart, Previous: task.key.previous}
	defer func() {
		out.canceled = ctx.Err() != nil && !out.done && out.err == nil
		record := base
		record.Type, record.Lines = "streamFailed", out.lines
		if out.complete {
			record.Type = "streamCompleted"
		} else if out.canceled {
			record.Type = "streamCanceled"
		}
		finalCtx, finalCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer finalCancel()
		out.err = errors.Join(out.err, f.writeContext(finalCtx, record))
	}()
	for attempt := 0; attempt < 3 && ctx.Err() == nil && !f.failed(); attempt++ {
		if attempt > 0 && !streamPause(ctx, time.Duration(1<<uint(attempt-1))*100*time.Millisecond) {
			break
		}
		callCtx, cancel := context.WithTimeoutCause(ctx, renewal, errStreamRenewal)
		out.requests++
		requested := base
		requested.Type = "streamRequested"
		if err := f.writeContext(callCtx, requested); err != nil {
			cancel()
			if ctx.Err() == nil {
				out.err = err
			}
			return out
		}
		streamOpened := false
		stage := "verifyPodIdentity"
		p, err := client.CoreV1().Pods(task.namespace).Get(callCtx, task.pod, metav1.GetOptions{})
		if err == nil {
			matches := false
			for _, statuses := range [][]corev1.ContainerStatus{p.Status.InitContainerStatuses, p.Status.ContainerStatuses, p.Status.EphemeralContainerStatuses} {
				for _, s := range statuses {
					if s.Name == task.key.container && s.RestartCount == task.observedRestart && (!task.key.previous || s.RestartCount > 0) {
						matches = true
						task.terminal = task.key.previous || s.State.Terminated != nil
					}
				}
			}
			if p.UID != task.key.uid || !matches {
				cancel()
				base.Type, base.Error = "gap", &APIError{Reason: "podIdentityChanged"}
				out.err, out.done = f.write(base), true
				return out
			}
		}
		if err == nil {
			opts := &corev1.PodLogOptions{Container: task.key.container, Follow: !task.key.previous, Previous: task.key.previous, Timestamps: true}
			if out.since == nil {
				opts.TailLines = sessionPtr(int64(1000))
			} else {
				since := metav1.NewTime(out.since.Add(-time.Second))
				opts.SinceTime = &since
			}
			var body io.ReadCloser
			stage = "openLogs"
			body, err = open(callCtx, client, task.namespace, task.pod, opts)
			if err == nil {
				streamOpened = true
				out.opened = true
				out.streamsOpened++
				stage = "streamRead"
				// Closing the response on cancellation also supports injected readers
				// that do not themselves know about the request context.
				closed := make(chan struct{})
				stopClose := context.AfterFunc(callCtx, func() {
					defer k8sutilruntime.HandleCrash()
					_ = body.Close()
					close(closed)
				})
				opened := base
				opened.Type = "streamOpened"
				finalCtx, finalCancel := context.WithTimeout(context.Background(), 5*time.Second)
				err = f.writeContext(finalCtx, opened)
				finalCancel()
				if err == nil {
					err = readLogLines(callCtx, body, base, f, &out.since, &out.lines)
				} else {
					out.err = err
				}
				if !stopClose() {
					<-closed
				} else {
					err = errors.Join(err, body.Close())
				}
				if callCtx.Err() != nil && err == nil {
					err = callCtx.Err()
				}
			}
		}
		plannedRenewal := streamOpened && !task.terminal && errors.Is(context.Cause(callCtx), errStreamRenewal)
		cancel()
		var evidenceErr *logEvidenceError
		if errors.As(err, &evidenceErr) {
			out.err = errors.Join(out.err, evidenceErr)
		}
		if out.err != nil {
			return out
		}
		if err == nil && ctx.Err() == nil && !f.failed() {
			stage = "verifyLogEOF"
			// A container may terminate while Follow is open. Verify clean EOF
			// against fresh status, including identity, before claiming completion.
			checkCtx, checkCancel := context.WithTimeout(ctx, renewal)
			p, checkErr := client.CoreV1().Pods(task.namespace).Get(checkCtx, task.pod, metav1.GetOptions{})
			checkCancel()
			err = checkErr
			if err == nil {
				matches, terminal := false, false
				for _, statuses := range [][]corev1.ContainerStatus{p.Status.InitContainerStatuses, p.Status.ContainerStatuses, p.Status.EphemeralContainerStatuses} {
					for _, s := range statuses {
						if s.Name == task.key.container && s.RestartCount == task.observedRestart {
							matches = true
							terminal = task.key.previous || s.State.Terminated != nil
						}
					}
				}
				if p.UID != task.key.uid || !matches {
					base.Type, base.Error = "gap", &APIError{Reason: "podIdentityChanged"}
					out.err, out.done = f.write(base), true
					return out
				}
				if terminal {
					out.done, out.complete = true, true
					return out
				}
			}
		}
		if f.failed() {
			out.err = errors.New("log storage failed")
			return out
		}
		if ctx.Err() != nil {
			return out
		}
		// Periodically reopen even idle follows so UID/restart identity is
		// rechecked. Planned renewals do not consume the failure retry budget.
		if plannedRenewal {
			attempt = -1
			record := base
			record.Type, record.Error, record.PotentialGap = "reconnect", &APIError{Reason: "logRenewal"}, true
			if writeErr := f.write(record); writeErr != nil {
				out.err = writeErr
				return out
			}
			continue
		}
		record := base
		record.Type, record.Error = "gap", &APIError{Reason: "logEOF"}
		if err != nil {
			record.Type, record.Error = "error", classifyError(err)
			record.Error.Stage = stage
		}
		if writeErr := f.write(record); writeErr != nil {
			out.err = writeErr
			return out
		}
	}
	if ctx.Err() == nil && !f.failed() {
		base.Type, base.Error = "gap", &APIError{Reason: "logRetriesExhausted"}
		out.err, out.done = f.write(base), true
	}
	return out
}

type contextLogReader struct {
	ctx  context.Context
	body io.Reader
}

// A failed cancellation marker must survive the normal suppression of the
// request's cancellation error, including when storage's writer-wait expires.
type logEvidenceError struct{ error }

func (r contextLogReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.body.Read(p)
}

func readLogLines(ctx context.Context, body io.Reader, base StreamObservation, f *liveFile, since **time.Time, lines *int64) (retErr error) {
	reader := bufio.NewReaderSize(contextLogReader{ctx, body}, maxLogLineBytes+1)
	var pending []byte
	defer func() {
		if ctx.Err() == nil {
			return
		}
		retErr = errors.Join(retErr, ctx.Err())
		if len(pending)+reader.Buffered() == 0 {
			return
		}
		record := base
		record.Type, record.Error = "gap", &APIError{Reason: "canceledWithBufferedData", Stage: "streamRead"}
		record.BufferedBytes = len(pending) + reader.Buffered()
		buffered, _ := reader.Peek(reader.Buffered())
		last := pending
		if len(buffered) > 0 {
			last = buffered
		}
		record.PartialLine = len(last) > 0 && last[len(last)-1] != '\n'
		finalCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := f.writeContext(finalCtx, record); err != nil {
			retErr = errors.Join(retErr, &logEvidenceError{err})
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := reader.ReadSlice('\n')
		pending = line
		if err := ctx.Err(); err != nil {
			return err
		}
		if errors.Is(err, bufio.ErrBufferFull) || len(line) > maxLogLineBytes {
			record := base
			record.Type, record.Error = "gap", &APIError{Reason: "logLineTooLarge"}
			if writeErr := f.writeContext(ctx, record); writeErr != nil {
				return writeErr
			}
			for errors.Is(err, bufio.ErrBufferFull) {
				if err := ctx.Err(); err != nil {
					return err
				}
				pending, err = reader.ReadSlice('\n')
			}
			pending = nil // The oversized line is already accounted for by its gap.
			if err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
			continue
		}
		if len(line) > 0 {
			if err != nil && line[len(line)-1] != '\n' {
				gap := base
				gap.Type, gap.Error = "gap", &APIError{Reason: "logPartialLine"}
				if writeErr := f.writeContext(ctx, gap); writeErr != nil {
					return writeErr
				}
			}
			record := base
			record.Type = "line"
			text := strings.TrimSuffix(string(line), "\n")
			stamp, message, found := strings.Cut(text, " ")
			at, parseErr := time.Parse(time.RFC3339Nano, stamp)
			if found && parseErr == nil {
				record.LogTime, record.Body = &at, message
			} else {
				record.Body = text
				gap := base
				gap.Type, gap.Error = "gap", &APIError{Reason: "logTimestampMissing"}
				if writeErr := f.writeContext(ctx, gap); writeErr != nil {
					return writeErr
				}
			}
			if writeErr := f.writeContext(ctx, record); writeErr != nil {
				return writeErr
			}
			pending = nil
			*lines++
			if record.LogTime != nil && (*since == nil || at.After(**since)) {
				*since = &at
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

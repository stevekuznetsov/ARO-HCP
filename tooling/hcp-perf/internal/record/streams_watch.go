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
	"context"
	"errors"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

type watchSource struct {
	list  func(context.Context, metav1.ListOptions) (runtime.Object, error)
	watch func(context.Context, metav1.ListOptions) (watch.Interface, error)
}

func typedWatchSource[T runtime.Object](list func(context.Context, metav1.ListOptions) (T, error), watcher func(context.Context, metav1.ListOptions) (watch.Interface, error)) watchSource {
	return watchSource{list: func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) { return list(ctx, opts) }, watch: watcher}
}

func sourceFor(client kubernetes.Interface, ns, resource string) watchSource {
	switch resource {
	case "pods":
		c := client.CoreV1().Pods(ns)
		return typedWatchSource(c.List, c.Watch)
	case "events":
		c := client.CoreV1().Events(ns)
		return typedWatchSource(c.List, c.Watch)
	case "deployments":
		c := client.AppsV1().Deployments(ns)
		return typedWatchSource(c.List, c.Watch)
	case "statefulsets":
		c := client.AppsV1().StatefulSets(ns)
		return typedWatchSource(c.List, c.Watch)
	default:
		panic("unsupported recording resource")
	}
}

// Never serialize the object itself, including on unexpected watch event types.
func sanitizeWatchObject(obj runtime.Object) (any, error) {
	switch o := obj.(type) {
	case *corev1.Pod:
		return sanitizePod(*o), nil
	case *corev1.Event:
		ref := o.InvolvedObject
		e := event{Metadata: objectMetadata(o.ObjectMeta), Reason: o.Reason, Type: o.Type, Count: o.Count,
			InvolvedObject: objectReference{ref.APIVersion, ref.Kind, ref.Namespace, ref.Name, ref.UID},
			FirstTimestamp: o.FirstTimestamp, LastTimestamp: o.LastTimestamp, EventTime: o.EventTime}
		if o.Series != nil {
			e.SeriesCount, e.LastObservedAt = o.Series.Count, o.Series.LastObservedTime
		}
		return e, nil
	case *appsv1.Deployment:
		w := workload{Metadata: objectMetadata(o.ObjectMeta), DesiredReplicas: o.Spec.Replicas, ObservedGeneration: o.Status.ObservedGeneration,
			Replicas: o.Status.Replicas, ReadyReplicas: o.Status.ReadyReplicas, AvailableReplicas: o.Status.AvailableReplicas, UpdatedReplicas: o.Status.UpdatedReplicas}
		for _, c := range o.Status.Conditions {
			w.Conditions = append(w.Conditions, condition{string(c.Type), c.Status, c.Reason, c.LastTransitionTime})
		}
		return w, nil
	case *appsv1.StatefulSet:
		w := workload{Metadata: objectMetadata(o.ObjectMeta), DesiredReplicas: o.Spec.Replicas, ObservedGeneration: o.Status.ObservedGeneration,
			Replicas: o.Status.Replicas, ReadyReplicas: o.Status.ReadyReplicas, AvailableReplicas: o.Status.AvailableReplicas, UpdatedReplicas: o.Status.UpdatedReplicas}
		for _, c := range o.Status.Conditions {
			w.Conditions = append(w.Conditions, condition{string(c.Type), c.Status, c.Reason, c.LastTransitionTime})
		}
		return w, nil
	default:
		return nil, errors.New("unexpected watch object")
	}
}

func recordWatch(ctx context.Context, client kubernetes.Interface, ns, resource string, f *liveFile) error {
	return recordWatchWithRenewal(ctx, client, ns, resource, f, 5*time.Minute)
}

func recordWatchWithRenewal(ctx context.Context, client kubernetes.Interface, ns, resource string, f *liveFile, renewal time.Duration) error {
	source := sourceFor(client, ns, resource)
	base := StreamObservation{Namespace: ns, Resource: resource}
	rv := ""
	initialized := false
	backoff := 100 * time.Millisecond
	for ctx.Err() == nil && !f.failed() {
		var callErr error
		plannedRenewal := false
		if rv == "" {
			continuation := ""
			for ctx.Err() == nil {
				callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				list, err := source.list(callCtx, metav1.ListOptions{Limit: 500, Continue: continuation})
				cancel()
				if err != nil {
					callErr = err
					break
				}
				m, err := meta.ListAccessor(list)
				if err != nil {
					callErr = err
					break
				}
				objects, err := meta.ExtractList(list)
				if err != nil {
					callErr = err
					break
				}
				for _, obj := range objects {
					body, err := sanitizeWatchObject(obj)
					if err != nil {
						callErr = err
						break
					}
					metadata, err := meta.Accessor(obj)
					if err != nil {
						callErr = err
						break
					}
					record := base
					record.Type, record.Initial, record.ObjectRV, record.Body = "added", true, metadata.GetResourceVersion(), body
					if err := f.write(record); err != nil {
						return err
					}
				}
				if callErr != nil {
					break
				}
				continuation = m.GetContinue()
				if continuation == "" {
					rv = m.GetResourceVersion()
					if rv == "" {
						callErr = errors.New("missing list resource version")
						break
					}
					record := base
					record.Type, record.Initial, record.ObjectRV = "bookmark", true, rv
					if err := f.write(record); err != nil {
						return err
					}
					break
				}
			}
		}
		if ctx.Err() != nil {
			break
		}
		if callErr == nil {
			// A client-side deadline bounds both watch establishment and reading.
			watchCtx, cancel := context.WithTimeoutCause(ctx, renewal, errStreamRenewal)
			w, err := source.watch(watchCtx, metav1.ListOptions{ResourceVersion: rv, AllowWatchBookmarks: true, TimeoutSeconds: sessionPtr(int64(240))})
			callErr = err
			if err == nil {
				if !initialized && watchCtx.Err() == nil {
					f.mu.Lock()
					f.stats.Initialized++
					f.mu.Unlock()
					initialized = true
				}
				callErr = consumeWatch(watchCtx, w, base, f, &rv)
				plannedRenewal = errors.Is(context.Cause(watchCtx), errStreamRenewal)
				w.Stop()
			}
			cancel()
		}
		if ctx.Err() != nil {
			break
		}
		if f.failed() {
			return errors.New("watch storage failed")
		}
		record := base
		record.Type, record.ObjectRV = "error", rv
		if callErr == nil {
			record.Type, record.Error = "reconnect", &APIError{Reason: "watchEOF"}
		} else if plannedRenewal {
			record.Type, record.Error = "reconnect", &APIError{Reason: "watchRenewal"}
		} else {
			record.Error = classifyError(callErr)
			if apierrors.IsResourceExpired(callErr) || apierrors.IsGone(callErr) {
				record.Type, record.Error = "gap", &APIError{Reason: "expired", Code: 410}
				rv = ""
			}
		}
		if err := f.write(record); err != nil {
			return err
		}
		if !streamPause(ctx, backoff) {
			break
		}
		backoff = min(backoff*2, 5*time.Second)
	}
	return nil
}

func consumeWatch(ctx context.Context, w watch.Interface, base StreamObservation, f *liveFile, rv *string) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case e, ok := <-w.ResultChan():
			if !ok {
				return nil
			}
			if e.Type == watch.Error {
				return apierrors.FromObject(e.Object)
			}
			m, err := meta.Accessor(e.Object)
			if err != nil {
				return errors.New("invalid watch metadata")
			}
			record := base
			record.Type, record.ObjectRV = strings.ToLower(string(e.Type)), m.GetResourceVersion()
			if record.ObjectRV == "" {
				return errors.New("missing object resource version")
			}
			switch e.Type {
			case watch.Bookmark:
			case watch.Added, watch.Modified, watch.Deleted:
				record.Body, err = sanitizeWatchObject(e.Object)
				if err != nil {
					return err
				}
			default:
				return errors.New("unexpected watch event")
			}
			if err := f.write(record); err != nil {
				return err
			}
			*rv = record.ObjectRV
		}
	}
}

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
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/adversary"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/metrics"
)

type competitorWorker struct {
	Node    string    `json:"node"`
	Job     string    `json:"job"`
	JobUID  types.UID `json:"jobUID"`
	Pod     string    `json:"pod"`
	PodUID  types.UID `json:"podUID"`
	Started time.Time `json:"started"`
}

func stoppedWorkers(status adversary.RunStatus, spec Spec, workers []competitorWorker, namespace string) error {
	expected := spec.Competitor.Run
	expected.TargetNamespace = namespace
	if expected.CPUFillPercent == 0 && expected.ReserveCPUMilli == nil {
		expected.CPUFillPercent = 90
	}
	if status.RegistrationUID == "" || !reflect.DeepEqual(status.Spec, expected) || !reflect.DeepEqual(status.Plan.Nodes, spec.Competitor.Nodes) || status.Phase != "stopped" || !status.StopRequested || status.NonterminalPods != 0 || status.SubmissionIncomplete || len(workers) == 0 || len(status.Jobs) != len(workers) {
		return errors.New("registered competitor stop unverified")
	}
	for i, job := range status.Jobs {
		worker := workers[i]
		if job.UID == "" || job.UID != worker.JobUID || job.Name != worker.Job || job.Node != worker.Node || job.Deleting || job.Phase != "suspended" || len(job.Pods) != 0 {
			return errors.New("stopped competitor Job identity or zero-Pod evidence changed")
		}
	}
	return nil
}

func runningWorkers(status, expected adversary.RunStatus) ([]competitorWorker, error) {
	if expected.RegistrationUID == "" || status.RegistrationUID != expected.RegistrationUID || status.StopRequested || status.SubmissionIncomplete || !status.PlacementValid || status.Phase != "running" || !reflect.DeepEqual(status.Plan.Nodes, expected.Plan.Nodes) || !reflect.DeepEqual(status.Snapshot.TargetPods, expected.Snapshot.TargetPods) || len(status.Jobs) != len(expected.Jobs) || len(expected.Jobs) == 0 || status.NonterminalPods != len(expected.Jobs) {
		return nil, errors.New("competitor registration, placement or inventory unverified")
	}
	workers := make([]competitorWorker, 0, len(status.Jobs))
	for i, job := range status.Jobs {
		bound := expected.Jobs[i]
		if bound.UID == "" || job.Name != bound.Name || job.UID != bound.UID || job.Node != bound.Node || job.Deleting || job.Phase != "running" || len(job.Pods) != 1 {
			return nil, errors.New("every frozen competitor Job must have exactly one running worker")
		}
		pod := job.Pods[0]
		if pod.UID == "" || pod.Node != bound.Node || pod.Phase != corev1.PodRunning || !pod.Ready || pod.Deleting || len(pod.Containers) != 1 {
			return nil, errors.New("every competitor Pod must be Running, Ready and non-deleting on its frozen node")
		}
		container := pod.Containers[0]
		if container.Name != "cpu-burn" || !container.Ready || container.State.Running == nil || container.State.Running.StartedAt.IsZero() || container.RestartCount != 0 {
			return nil, errors.New("competitor container is not continuously running")
		}
		workers = append(workers, competitorWorker{Node: job.Node, Job: job.Name, JobUID: job.UID, Pod: pod.Name, PodUID: pod.UID, Started: container.State.Running.StartedAt.Time})
	}
	return workers, nil
}

func competitorReady(ctx context.Context, kube kubernetes.Interface, client *http.Client, spec Spec, expected adversary.RunStatus, frozen []competitorWorker, j *journal) ([]competitorWorker, error) {
	status, err := adversary.Status(ctx, kube, spec.Competitor.Run.Namespace, spec.Competitor.Run.ID)
	if err != nil {
		return nil, errors.New("competitor status unavailable")
	}
	workers, err := runningWorkers(status, expected)
	if err != nil {
		return nil, err
	}
	if len(frozen) != 0 && !reflect.DeepEqual(workers, frozen) {
		return nil, errors.New("frozen competitor worker identity changed")
	}
	for _, worker := range workers {
		if err := workerCPU(ctx, client, spec, worker, j); err != nil {
			return workers, err
		}
	}
	return workers, nil
}

func workerCPU(ctx context.Context, client *http.Client, spec Spec, worker competitorWorker, j *journal) error {
	// Select the exact registered Pod/container, never a node-wide aggregate.
	// timestamp(counter) returns source time as its VALUE; instant sample At is
	// only Prometheus evaluation time and cannot establish source freshness.
	selector := fmt.Sprintf("container_cpu_usage_seconds_total{job=%q,namespace=%q,pod=%q,container=\"cpu-burn\"}", spec.Competitor.CPUJob, spec.Competitor.Run.Namespace, worker.Pod)
	rates, rateErr := metrics.Instant(ctx, client, spec.Capture.Endpoint, "rate("+selector+"[1m])")
	stamps, stampErr := metrics.Instant(ctx, client, spec.Capture.Endpoint, "timestamp("+selector+")")
	if err := j.event("competitor-cpu", struct {
		Worker      competitorWorker `json:"worker"`
		Selector    string           `json:"selector"`
		Rates       []metrics.Point  `json:"rates"`
		SourceTimes []metrics.Point  `json:"sourceTimes"`
	}{worker, selector, rates, stamps}); err != nil {
		return err
	}
	if rateErr != nil || stampErr != nil || len(rates) != 1 || len(stamps) != 1 || rates[0].Value <= 0 {
		return errors.New("competitor requires one positive CPU series per worker")
	}
	rate, stamp := rates[0], stamps[0]
	delete(rate.Labels, "__name__")
	delete(stamp.Labels, "__name__")
	if labels.Set(rate.Labels).String() != labels.Set(stamp.Labels).String() || stamp.Labels["namespace"] != spec.Competitor.Run.Namespace || stamp.Labels["pod"] != worker.Pod || stamp.Labels["container"] != "cpu-burn" || stamp.Labels["job"] != spec.Competitor.CPUJob {
		return errors.New("competitor CPU series identity mismatch")
	}
	sourceAt := time.UnixMilli(int64(stamp.Value * 1000))
	age := time.Since(sourceAt)
	if age < -5*time.Second || age > spec.Capture.maxAge(spec.Competitor.CPUJob) || sourceAt.Before(worker.Started) {
		return errors.New("competitor CPU source is stale or predates worker")
	}
	return nil
}

func stopCompetitor(ctx context.Context, client kubernetes.Interface, spec adversary.RunSpec, identity *adversary.RunStatus) error {
	if identity.RegistrationUID == "" {
		return errors.New("competitor registration was not confirmed; cleanup cannot adopt a run")
	}
	var last error
	for {
		status, err := adversary.Stop(ctx, client, spec.Namespace, spec.ID, identity)
		last = err
		if err == nil && status.Phase == "stopped" && status.StopRequested && status.NonterminalPods == 0 && !status.SubmissionIncomplete {
			return nil
		}
		if last == nil {
			last = errors.New("competitor suspension or zero-load verification incomplete")
		}
		if err := wait(ctx, time.Second); err != nil {
			return errors.Join(errors.New("competitor cleanup budget exhausted; remote load unverified"), last, err)
		}
	}
}

// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package startup

import (
	"context"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// A generated Job is covered only by its actual CronJob controller UID. Exact
// Job profiles take precedence; no prefix, generateName or label matches are used.
func inspectBatch(ctx context.Context, clients Clients, components []Component, namespace string, pods []corev1.Pod, matchedPods map[string]bool, sample *Sample, history *healthHistory) error {
	jobs, err := clients.Kubernetes.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	crons, err := clients.Kubernetes.BatchV1().CronJobs(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	profiles := map[string]Component{}
	present := map[string]bool{}
	cronProfiles := map[types.UID]Component{}
	jobProfiles := map[types.UID]Component{}
	issue := func(message string, violation bool) {
		sample.Issues = append(sample.Issues, message)
		history.violated = history.violated || violation
	}
	for _, component := range components {
		if component.Kind == "Job" || component.Kind == "CronJob" || component.Kind == "Pod" {
			profiles[component.Kind+"/"+component.Name] = component
		}
	}
	for _, cron := range crons.Items {
		key := "CronJob/" + cron.Name
		profile, found := profiles[key]
		if !found {
			issue("undeclared workload: "+key, true)
			continue
		}
		present[key] = true
		cronProfiles[cron.UID] = profile
		if !matchesProfile(cron.Spec.JobTemplate.Spec.Template.Spec, profile) {
			issue("template resources mismatch: "+key, true)
		}
		suspended := cron.Spec.Suspend != nil && *cron.Spec.Suspend
		if suspended != (profile.Lifecycle.Mode == "disabled") {
			issue("CronJob suspension mismatch: "+key, true)
		}
		if profile.Lifecycle.Mode == "disabled" && len(cron.Status.Active) != 0 {
			issue("disabled CronJob has active Jobs: "+key, true)
		}
	}
	for _, job := range jobs.Items {
		key := "Job/" + job.Name
		profile, found := profiles[key]
		if found {
			present[key] = true
		}
		var cronProfile Component
		cronOwned := false
		for _, owner := range job.OwnerReferences {
			if owner.Controller == nil || !*owner.Controller || owner.Kind != "CronJob" || owner.UID == "" {
				continue
			}
			cronProfile, cronOwned = cronProfiles[owner.UID]
			if cronOwned && cronProfile.Name != owner.Name {
				cronOwned = false
			}
			if !found && cronOwned {
				profile, found = cronProfile, true
			}
		}
		if !found {
			issue("undeclared workload: "+key, true)
			continue
		}
		jobProfiles[job.UID] = profile
		if profile.ExpectedUID != "" {
			ownerMatches := false
			for _, owner := range job.OwnerReferences {
				ownerMatches = ownerMatches || (owner.Controller != nil && *owner.Controller && owner.Kind == "CronJob" && string(owner.UID) == profile.ExpectedOwnerUID && cronOwned)
			}
			if string(job.UID) != profile.ExpectedUID || !ownerMatches {
				issue("historical Job identity mismatch: "+key, true)
			}
		}
		if !matchesProfile(job.Spec.Template.Spec, profile) {
			issue("template resources mismatch: "+key, true)
		}
		complete, failed := false, false
		for _, condition := range job.Status.Conditions {
			if condition.Status != corev1.ConditionTrue {
				continue
			}
			complete = complete || condition.Type == batchv1.JobComplete
			failed = failed || condition.Type == batchv1.JobFailed
		}
		if failed {
			issue("failed Job: "+key, true)
		}
		if profile.Kind == "Job" && profile.Lifecycle.Mode == "completed" && !complete {
			issue("Job not completed: "+key, false)
		}
		if cronOwned && cronProfile.Lifecycle.Mode == "disabled" && (!complete && !failed || job.Status.Active > 0) {
			issue("disabled CronJob has unfinished Job: "+key, true)
		}
	}
	for _, pod := range pods {
		profile, found := profiles["Pod/"+pod.Name]
		if found {
			// A standalone declaration must not hide an unknown workload owner.
			if len(pod.OwnerReferences) != 0 {
				issue("standalone Pod has owner: "+pod.Name, true)
				continue
			}
			present["Pod/"+pod.Name] = true
		} else {
			for _, owner := range pod.OwnerReferences {
				if owner.Controller != nil && *owner.Controller && owner.Kind == "Job" && owner.UID != "" {
					profile, found = jobProfiles[owner.UID]
				}
			}
		}
		if !found {
			continue
		}
		matchedPods[string(pod.UID)] = true
		if !matchesProfile(pod.Spec, profile) {
			issue("observed pod resources mismatch: "+pod.Name, true)
		}
		if pod.Status.Phase == corev1.PodFailed {
			issue("failed batch/standalone Pod: "+pod.Name, true)
		}
		if profile.Kind == "Pod" {
			if profile.Lifecycle.Mode == "completed" {
				if pod.Status.Phase != corev1.PodSucceeded {
					issue("standalone Pod not completed: "+pod.Name, false)
				}
			} else {
				ready := false
				for _, condition := range pod.Status.Conditions {
					ready = ready || condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue
				}
				if !ready || pod.DeletionTimestamp != nil {
					issue("standalone Pod not ready: "+pod.Name, false)
				}
			}
		}
	}
	for key, profile := range profiles {
		if present[key] && sample.Coverage != nil {
			sample.Coverage.ObservedWorkloads++
		}
		if !present[key] && profile.Lifecycle.Mode != "optional" {
			issue("missing "+key, false)
		}
	}
	return nil
}

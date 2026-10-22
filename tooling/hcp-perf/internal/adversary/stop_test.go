// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package adversary

import (
	"errors"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ktesting "k8s.io/client-go/testing"
)

func TestStopRetriesAndContinuesAllJobs(t *testing.T) {
	for _, mode := range []string{"conflict", "transient", "permanent", "replacement", "partial"} {
		t.Run(mode, func(t *testing.T) {
			client := runtimeClient(runtimeNode("worker-a"), runtimeNode("worker-b"), runtimePod("hcp", "a", "worker-a", "500m"), runtimePod("hcp", "b", "worker-b", "500m"))
			if mode == "partial" {
				client.PrependReactor("create", "jobs", func(action ktesting.Action) (bool, runtime.Object, error) {
					if action.(ktesting.CreateAction).GetObject().(*batchv1.Job).Name == jobName("test-run", "worker-b") {
						return true, nil, errors.New("submission failed")
					}
					return false, nil, nil
				})
			}
			bound, startErr := Start(t.Context(), client, runtimeSpec())
			if (startErr != nil) != (mode == "partial") {
				t.Fatal(startErr)
			}
			attempts := 0
			client.PrependReactor("patch", "jobs", func(action ktesting.Action) (bool, runtime.Object, error) {
				if action.(ktesting.PatchAction).GetName() != bound.Jobs[0].Name {
					return false, nil, nil
				}
				attempts++
				switch mode {
				case "conflict":
					if attempts < 3 {
						return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "batch", Resource: "jobs"}, bound.Jobs[0].Name, errors.New("conflict"))
					}
				case "transient":
					if attempts < 3 {
						return true, nil, apierrors.NewServiceUnavailable("retry")
					}
				case "permanent":
					return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "jobs"}, bound.Jobs[0].Name, errors.New("denied"))
				case "replacement":
					object, err := client.Tracker().Get(batchv1.SchemeGroupVersion.WithResource("jobs"), "capture", bound.Jobs[0].Name)
					if err != nil {
						t.Fatal(err)
					}
					job := object.(*batchv1.Job).DeepCopy()
					job.UID = "replacement"
					if err := client.Tracker().Update(batchv1.SchemeGroupVersion.WithResource("jobs"), job, "capture"); err != nil {
						t.Fatal(err)
					}
					return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "jobs"}, job.Name, errors.New("changed"))
				}
				return false, nil, nil
			})
			status, err := Stop(t.Context(), client, "capture", "test-run", &bound)
			wantErr := mode == "permanent" || mode == "replacement"
			if (err != nil) != wantErr {
				t.Fatalf("stop: %v", err)
			}
			if mode == "conflict" || mode == "transient" {
				if attempts != 3 || status.Phase != "stopped" {
					t.Fatalf("retry did not converge: %d %+v", attempts, status)
				}
			}
			if mode == "partial" {
				if !status.SubmissionIncomplete || status.Phase == "stopped" {
					t.Fatal("partial submission declared stopped")
				}
			} else {
				job, err := client.BatchV1().Jobs("capture").Get(t.Context(), bound.Jobs[1].Name, metav1.GetOptions{})
				if err != nil || job.Spec.Suspend == nil || !*job.Spec.Suspend {
					t.Fatal("first job error prevented second suspension", err)
				}
			}
		})
	}
}

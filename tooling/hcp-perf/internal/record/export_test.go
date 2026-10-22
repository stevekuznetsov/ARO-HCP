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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

func exportSession(t *testing.T) (*fake.Clientset, SessionSpec) {
	t.Helper()
	c, spec := startSession(t)
	job, err := c.BatchV1().Jobs(spec.Namespace).Get(t.Context(), "record-"+spec.ID, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	job.Spec.Suspend = sessionPtr(true)
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobSuspended, Status: corev1.ConditionTrue}}
	if _, err := c.BatchV1().Jobs(spec.Namespace).Update(t.Context(), job, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	pvc, err := c.CoreV1().PersistentVolumeClaims(spec.Namespace).Get(t.Context(), job.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pvc.Status.Phase = corev1.ClaimBound
	if _, err := c.CoreV1().PersistentVolumeClaims(spec.Namespace).Update(t.Context(), pvc, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	c.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		pod.Status.Phase = corev1.PodRunning
		return false, nil, nil
	})
	c.ClearActions()
	return c, spec
}

func exportArchiveFixture(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()
	for name, contents := range map[string]string{
		"pods.ndjson": "{\"type\":\"complete\"}\n", "watch.ndjson": "{\"type\":\"added\"}\n",
		"logs.ndjson": "{\"body\":\"SECRET raw log\"}\n", "stream-summary.json": "{\"rawLogs\":true}\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var archive bytes.Buffer
	if err := WriteArchive(dir, &archive); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func TestExportRenderingAndArchive(t *testing.T) {
	for _, phase := range []string{"stopped", "completed", "failed"} {
		t.Run(phase, func(t *testing.T) {
			c, spec := exportSession(t)
			if phase == "completed" || phase == "failed" {
				job, _ := c.BatchV1().Jobs(spec.Namespace).Get(t.Context(), "record-"+spec.ID, metav1.GetOptions{})
				job.Spec.Suspend = nil
				job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
				if phase == "failed" {
					job.Status.Conditions[0].Type = batchv1.JobFailed
				}
				if _, err := c.BatchV1().Jobs(spec.Namespace).Update(t.Context(), job, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
				c.ClearActions()
			}
			data := exportArchiveFixture(t)
			config := &rest.Config{Host: "https://unused.invalid"}
			output := filepath.Join(t.TempDir(), "export")
			calls := 0
			manifest, err := exportWithExec(t.Context(), c, config, spec.Namespace, spec.ID, output,
				func(ctx context.Context, got *rest.Config, namespace, pod string, out io.Writer) error {
					calls++
					if got != config || namespace != spec.Namespace || pod != "record-export-"+spec.ID {
						t.Fatal("exec config or target changed")
					}
					deadline, ok := ctx.Deadline()
					if !ok || time.Until(deadline) > 5*time.Minute {
						t.Fatal("exec has no bounded context")
					}
					status, err := Status(ctx, c, namespace, spec.ID)
					if err != nil || status.Phase != phase || status.NonterminalPods != 0 || status.ExportHelper == nil || status.ExportHelper.Phase != corev1.PodRunning {
						t.Fatalf("helper confused writer status: %+v, %v", status, err)
					}
					for _, chunk := range [][]byte{data[:123], data[123:]} {
						if _, err := out.Write(chunk); err != nil {
							return err
						}
					}
					return nil
				})
			if err != nil || calls != 1 || manifest.Helper == nil || manifest.Helper.UID == "" || manifest.Status.Phase != phase {
				t.Fatalf("export failed: %+v, %v, calls=%d", manifest, err, calls)
			}
			if manifest.ConfiguredLogQPS != 20 || manifest.ConfiguredLogBurst != 40 {
				t.Fatalf("export lost registered request budgets: %+v", manifest)
			}
			hash := sha256.Sum256(data)
			if manifest.SHA256 != hex.EncodeToString(hash[:]) || manifest.Bytes != int64(len(data)) || manifest.Archive != "archive.tar" {
				t.Fatalf("incorrect archive digest/size: %+v", manifest)
			}
			if manifest.MaxArchiveBytes != 32<<30 || manifest.MaxArchiveFileBytes != 8<<30 || !strings.Contains(manifest.Limitations, "CAN CONTAIN SECRETS") {
				t.Fatalf("manifest lacks export limits or raw-log warning: %+v", manifest)
			}
			for path, mode := range map[string]os.FileMode{output: 0700, filepath.Join(output, "archive.tar"): 0600, filepath.Join(output, "manifest.json"): 0600} {
				info, err := os.Stat(path)
				if err != nil || info.Mode().Perm() != mode {
					t.Fatalf("unsafe output permissions: %s: %v", path, err)
				}
			}
			actual, err := os.ReadFile(filepath.Join(output, "archive.tar"))
			if err != nil || !bytes.Equal(actual, data) {
				t.Fatal("archive bytes changed")
			}
			encoded, err := os.ReadFile(filepath.Join(output, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			var saved ExportManifest
			if err := json.Unmarshal(encoded, &saved); err != nil || !reflect.DeepEqual(saved, manifest) {
				t.Fatalf("manifest differs: %v", err)
			}
			if bytes.Contains(encoded, []byte("SECRET raw log")) {
				t.Fatal("raw log content leaked into export manifest")
			}
			creates, deletes := 0, 0
			for _, action := range c.Actions() {
				switch action.GetVerb() {
				case "create":
					creates++
					if action.GetResource().Resource != "pods" {
						t.Fatal("export creates resources other than helper")
					}
					pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
					owner := metav1.GetControllerOf(pod)
					if pod.Name != manifest.Helper.Name || owner == nil || owner.Kind != "ConfigMap" || owner.Name != "record-"+spec.ID || owner.UID == "" || pod.Labels[componentLabel] != "record-export" || pod.Labels[RecordExportIDLabel] != spec.ID || pod.Labels[RecordIDLabel] != "" {
						t.Fatal("helper identity incorrectly shares writer controller/labels")
					}
					ps := pod.Spec
					if ps.RestartPolicy != corev1.RestartPolicyNever || *ps.ActiveDeadlineSeconds != 300 || *ps.AutomountServiceAccountToken || *ps.EnableServiceLinks || ps.ServiceAccountName != "" || ps.NodeSelector["aro-hcp.azure.com/role"] != "infra" || len(ps.Tolerations) != 1 || ps.Tolerations[0].Key != "infra" || ps.Tolerations[0].Value != "true" || ps.Tolerations[0].Effect != corev1.TaintEffectNoSchedule {
						t.Fatal("unsafe helper lifetime, credentials or placement")
					}
					if !*ps.SecurityContext.RunAsNonRoot || *ps.SecurityContext.RunAsUser != 1000 || *ps.SecurityContext.RunAsGroup != 2000 || ps.SecurityContext.FSGroup != nil || ps.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
						t.Fatal("unsafe helper pod security")
					}
					if len(ps.Containers) != 1 || len(ps.InitContainers) != 0 || len(ps.Volumes) != 1 || ps.Volumes[0].PersistentVolumeClaim.ClaimName != "record-"+spec.ID || !ps.Volumes[0].PersistentVolumeClaim.ReadOnly {
						t.Fatal("helper must only mount recorder PVC read-only")
					}
					container := ps.Containers[0]
					if container.Image != spec.Image || !reflect.DeepEqual(container.Command, []string{"/hcp-perf"}) || !reflect.DeepEqual(container.Args, []string{"record", "hold", "--duration", "5m"}) || len(container.VolumeMounts) != 1 || !container.VolumeMounts[0].ReadOnly || container.VolumeMounts[0].MountPath != "/data" || len(container.Env) != 0 {
						t.Fatal("helper invocation or mount differs")
					}
					if *container.SecurityContext.AllowPrivilegeEscalation || !*container.SecurityContext.ReadOnlyRootFilesystem || !reflect.DeepEqual(container.SecurityContext.Capabilities.Drop, []corev1.Capability{"ALL"}) || container.Resources.Limits.Memory().Value() != spec.MemoryLimitMi*1024*1024 {
						t.Fatal("unsafe helper container")
					}
				case "delete":
					deletes++
					delete := action.(k8stesting.DeleteAction)
					options := delete.GetDeleteOptions()
					if action.GetResource().Resource != "pods" || delete.GetName() != manifest.Helper.Name || options.Preconditions == nil || *options.Preconditions.UID != manifest.Helper.UID {
						t.Fatal("cleanup lacks helper UID precondition or deletes evidence")
					}
				case "get", "list":
				default:
					t.Fatalf("unexpected export mutation: %v", action)
				}
			}
			if creates != 1 || deletes != 1 {
				t.Fatalf("helper lifecycle: creates=%d deletes=%d", creates, deletes)
			}
			if _, err := c.CoreV1().Pods(spec.Namespace).Get(t.Context(), manifest.Helper.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
				t.Fatal("helper retained")
			}
			if _, err := c.CoreV1().PersistentVolumeClaims(spec.Namespace).Get(t.Context(), "record-"+spec.ID, metav1.GetOptions{}); err != nil {
				t.Fatal("PVC not retained")
			}
		})
	}
}

func TestExportPreflight(t *testing.T) {
	for _, mode := range []string{"running", "suspend unacknowledged", "active", "nonterminal", "terminating", "orphan", "missing job", "lost PVC", "pending PVC", "foreign PVC", "failed", "canceled", "existing output", "nil config"} {
		t.Run(mode, func(t *testing.T) {
			c, spec := exportSession(t)
			job, _ := c.BatchV1().Jobs(spec.Namespace).Get(t.Context(), "record-"+spec.ID, metav1.GetOptions{})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			config := &rest.Config{}
			output := filepath.Join(t.TempDir(), "export")
			switch mode {
			case "running":
				job.Spec.Suspend = nil
				job.Status.Conditions = nil
			case "suspend unacknowledged":
				job.Status.Conditions = nil
			case "active":
				job.Status.Active = 1
			case "failed":
				job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionUnknown}}
			case "nonterminal", "terminating", "orphan":
				job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
				pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "writer", Namespace: spec.Namespace, Labels: sessionLabels(spec.ID), OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, batchv1.SchemeGroupVersion.WithKind("Job"))}},
					Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: job.Name}}}}}}
				if mode == "orphan" {
					pod.OwnerReferences, pod.Labels = nil, nil
				}
				if mode == "terminating" {
					now := metav1.Now()
					pod.DeletionTimestamp = &now
				}
				if _, err := c.CoreV1().Pods(spec.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			case "lost PVC", "pending PVC", "foreign PVC":
				pvc, _ := c.CoreV1().PersistentVolumeClaims(spec.Namespace).Get(ctx, job.Name, metav1.GetOptions{})
				if mode == "lost PVC" {
					pvc.Status.Phase = corev1.ClaimLost
				} else if mode == "pending PVC" {
					pvc.Status.Phase = corev1.ClaimPending
				} else {
					pvc.OwnerReferences[0].UID = "foreign"
				}
				if _, err := c.CoreV1().PersistentVolumeClaims(spec.Namespace).Update(ctx, pvc, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				cancel()
			case "existing output":
				if err := os.Mkdir(output, 0700); err != nil {
					t.Fatal(err)
				}
			case "nil config":
				config = nil
			}
			if _, err := c.BatchV1().Jobs(spec.Namespace).Update(t.Context(), job, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			if mode == "missing job" {
				if err := c.BatchV1().Jobs(spec.Namespace).Delete(t.Context(), job.Name, metav1.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			c.ClearActions()
			_, err := exportWithExec(ctx, c, config, spec.Namespace, spec.ID, output, func(context.Context, *rest.Config, string, string, io.Writer) error {
				t.Fatal("unsafe export reached exec")
				return nil
			})
			if err == nil {
				t.Fatal("unsafe export passed preflight")
			}
			for _, action := range c.Actions() {
				if action.GetVerb() != "get" && action.GetVerb() != "list" {
					t.Fatalf("preflight mutated Kubernetes: %v", action)
				}
			}
			_, statErr := os.Stat(output)
			if mode == "existing output" {
				if statErr != nil {
					t.Fatal("existing directory removed")
				}
			} else if !os.IsNotExist(statErr) {
				t.Fatal("preflight left output directory")
			}
		})
	}
}

func TestExportErrorsCleanUpHelper(t *testing.T) {
	for _, mode := range []string{"exec", "invalid archive", "canceled", "waiting canceled", "terminal", "replaced", "collision", "cleanup failure"} {
		t.Run(mode, func(t *testing.T) {
			c, spec := exportSession(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "waiting canceled" || mode == "terminal" || mode == "replaced" {
				c.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
					name := action.(k8stesting.GetAction).GetName()
					if mode == "waiting canceled" {
						if ctx.Err() != nil {
							return false, nil, nil
						}
						cancel()
						return true, nil, context.Canceled
					}
					object, err := c.Tracker().Get(corev1.SchemeGroupVersion.WithResource("pods"), spec.Namespace, name)
					if err != nil {
						return true, nil, err
					}
					pod := object.(*corev1.Pod)
					if mode == "replaced" {
						pod.UID = "replacement"
					} else {
						pod.Status.Phase = corev1.PodFailed
					}
					return true, pod, nil
				})
			}
			if mode == "collision" {
				c.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewAlreadyExists(action.GetResource().GroupResource(), "collision")
				})
			}
			if mode == "cleanup failure" {
				c.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("cleanup unavailable SECRET")
				})
			}
			output := filepath.Join(t.TempDir(), "export")
			manifest, err := exportWithExec(ctx, c, &rest.Config{}, spec.Namespace, spec.ID, output,
				func(_ context.Context, _ *rest.Config, _, _ string, out io.Writer) error {
					if mode == "canceled" {
						cancel()
						return context.Canceled
					}
					if _, err := out.Write([]byte("partial")); err != nil {
						return err
					}
					if mode == "invalid archive" {
						return nil
					}
					return errors.New("SECRET exec failure")
				})
			if err == nil || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("failure lost or leaked raw error: %v", err)
			}
			if (mode == "canceled" || mode == "waiting canceled") && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
			deletes := 0
			for _, action := range c.Actions() {
				if action.GetVerb() == "delete" {
					deletes++
					delete := action.(k8stesting.DeleteAction)
					if manifest.Helper == nil || action.GetResource().Resource != "pods" || delete.GetDeleteOptions().Preconditions == nil || *delete.GetDeleteOptions().Preconditions.UID != manifest.Helper.UID {
						t.Fatal("cleanup is not guarded by confirmed helper UID")
					}
				}
			}
			wantDeletes := 1
			if mode == "collision" {
				wantDeletes = 0
			}
			if deletes != wantDeletes {
				t.Fatalf("cleanup calls=%d, want %d", deletes, wantDeletes)
			}
			if mode == "cleanup failure" {
				if !manifest.HelperCleanupFailed {
					t.Fatal("cleanup failure not reported")
				}
				data, err := os.ReadFile(filepath.Join(output, "archive.tar"))
				if err != nil || string(data) != "partial" {
					t.Fatal("cleanup failure lost partial evidence")
				}
			} else if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatal("failed export retained local partial output")
			}
		})
	}
}

func TestExportLostCreateResponse(t *testing.T) {
	for _, mode := range []string{"accepted", "accepted canceled", "foreign attempt", "foreign owner", "lookup failure"} {
		t.Run(mode, func(t *testing.T) {
			c, spec := exportSession(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			c.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
				pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod).DeepCopy()
				pod.UID = "accepted-helper-uid"
				if mode == "foreign attempt" {
					pod.Annotations[exportAttemptAnnotation] = "different-attempt"
				}
				if mode == "foreign owner" {
					pod.OwnerReferences[0].UID = "foreign"
				}
				if err := c.Tracker().Create(corev1.SchemeGroupVersion.WithResource("pods"), pod, spec.Namespace); err != nil {
					t.Fatal(err)
				}
				if mode == "accepted canceled" {
					cancel()
					return true, nil, context.Canceled
				}
				return true, nil, errors.New("SECRET lost transport response")
			})
			if mode == "lookup failure" {
				c.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("SECRET lookup error")
				})
			}
			manifest, err := exportWithExec(ctx, c, &rest.Config{}, spec.Namespace, spec.ID, filepath.Join(t.TempDir(), "export"),
				func(context.Context, *rest.Config, string, string, io.Writer) error {
					t.Fatal("lost create response must never proceed to exec")
					return nil
				})
			if err == nil || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("lost response error missing or unsafe: %v", err)
			}
			deletes := 0
			for _, action := range c.Actions() {
				if action.GetVerb() == "delete" {
					deletes++
					if *action.(k8stesting.DeleteAction).GetDeleteOptions().Preconditions.UID != "accepted-helper-uid" {
						t.Fatal("recovery deleted without observed UID")
					}
				}
			}
			wantDeletes := 0
			if mode == "accepted" || mode == "accepted canceled" {
				wantDeletes = 1
				if manifest.Helper == nil || manifest.Helper.UID != "accepted-helper-uid" || manifest.HelperCleanupFailed {
					t.Fatalf("confirmed recovery lost: %+v", manifest)
				}
			}
			if deletes != wantDeletes || manifest.HelperCleanupFailed != (mode == "lookup failure") {
				t.Fatalf("unsafe recovery: deletes=%d, manifest=%+v", deletes, manifest)
			}
		})
	}
}

func TestExportCleanupFailureRetryAndStatus(t *testing.T) {
	c, spec := exportSession(t)
	denyDelete := true
	c.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		if denyDelete {
			return true, nil, errors.New("SECRET delete denied")
		}
		return false, nil, nil
	})
	data := exportArchiveFixture(t)
	exec := func(_ context.Context, _ *rest.Config, _, _ string, out io.Writer) error {
		_, err := out.Write(data)
		return err
	}
	output := filepath.Join(t.TempDir(), "export")
	manifest, err := exportWithExec(t.Context(), c, &rest.Config{}, spec.Namespace, spec.ID, output, exec)
	if err == nil || !manifest.HelperCleanupFailed || manifest.SHA256 == "" || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("successful tar hid cleanup failure: %+v, %v", manifest, err)
	}
	encoded, err := os.ReadFile(filepath.Join(output, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved ExportManifest
	if err := json.Unmarshal(encoded, &saved); err != nil || !reflect.DeepEqual(saved, manifest) {
		t.Fatalf("cleanup failure manifest not persisted: %v", err)
	}
	actual, err := os.ReadFile(filepath.Join(output, "archive.tar"))
	if err != nil || !bytes.Equal(actual, data) {
		t.Fatal("cleanup failure discarded complete archive")
	}
	c.ClearActions()
	_, err = exportWithExec(t.Context(), c, &rest.Config{}, spec.Namespace, spec.ID, filepath.Join(t.TempDir(), "live-retry"), exec)
	if err == nil {
		t.Fatal("retry accepted live helper")
	}
	for _, action := range c.Actions() {
		if action.GetVerb() != "get" && action.GetVerb() != "list" {
			t.Fatal("retry mutated live helper")
		}
	}
	pod, err := c.CoreV1().Pods(spec.Namespace).Get(t.Context(), manifest.Helper.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = corev1.PodFailed
	if _, err := c.CoreV1().Pods(spec.Namespace).Update(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	status, err := Status(t.Context(), c, spec.Namespace, spec.ID)
	if err != nil || status.Phase != "stopped" || status.NonterminalPods != 0 || status.ExportHelper == nil || status.ExportHelper.Phase != corev1.PodFailed {
		t.Fatalf("terminal helper polluted writer status: %+v, %v", status, err)
	}
	denied, err := exportWithExec(t.Context(), c, &rest.Config{}, spec.Namespace, spec.ID, filepath.Join(t.TempDir(), "denied-retry"), exec)
	if err == nil || !denied.HelperCleanupFailed || denied.Helper == nil {
		t.Fatal("terminal helper cleanup denial not reported")
	}
	denyDelete = false
	c.ClearActions()
	retried, err := exportWithExec(t.Context(), c, &rest.Config{}, spec.Namespace, spec.ID, filepath.Join(t.TempDir(), "retry"), exec)
	if err != nil || retried.HelperCleanupFailed || retried.SHA256 != manifest.SHA256 {
		t.Fatalf("terminal helper retry failed: %+v, %v", retried, err)
	}
	deletes := 0
	for _, action := range c.Actions() {
		if action.GetVerb() == "delete" {
			deletes++
		}
	}
	if deletes != 2 {
		t.Fatalf("expected cleanup of stale and new helpers, got %d", deletes)
	}
}

func TestExportHelperIdentityCannotHideWriter(t *testing.T) {
	c, spec := exportSession(t)
	c.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("retain helper for identity tests")
	})
	_, _ = exportWithExec(t.Context(), c, &rest.Config{}, spec.Namespace, spec.ID, filepath.Join(t.TempDir(), "export"),
		func(context.Context, *rest.Config, string, string, io.Writer) error { return errors.New("stop") })
	before, err := c.CoreV1().Pods(spec.Namespace).Get(t.Context(), "record-export-"+spec.ID, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cm, err := c.CoreV1().ConfigMaps(spec.Namespace).Get(t.Context(), "record-"+spec.ID, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*corev1.Pod){
		"name":           func(p *corev1.Pod) { p.Name += "-other" },
		"namespace":      func(p *corev1.Pod) { p.Namespace = "other" },
		"UID":            func(p *corev1.Pod) { p.UID = "" },
		"labels":         func(p *corev1.Pod) { p.Labels = nil },
		"owner UID":      func(p *corev1.Pod) { p.OwnerReferences[0].UID = "foreign" },
		"job owner":      func(p *corev1.Pod) { p.OwnerReferences[0].Kind = "Job" },
		"digest":         func(p *corev1.Pod) { p.Annotations[exportRegistrationAnnotation] = "foreign" },
		"attempt":        func(p *corev1.Pod) { delete(p.Annotations, exportAttemptAnnotation) },
		"writable PVC":   func(p *corev1.Pod) { p.Spec.Volumes[0].PersistentVolumeClaim.ReadOnly = false },
		"writable mount": func(p *corev1.Pod) { p.Spec.Containers[0].VolumeMounts[0].ReadOnly = false },
	} {
		t.Run(name, func(t *testing.T) {
			pod := before.DeepCopy()
			mutate(pod)
			if exportHelperOwned(pod, cm, spec.ID) {
				t.Fatal("foreign or writable helper recognized")
			}
			if name == "name" || name == "namespace" {
				return
			}
			if _, err := c.CoreV1().Pods(spec.Namespace).Update(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := Status(t.Context(), c, spec.Namespace, spec.ID); err == nil {
				t.Fatal("untrusted helper concealed potential writer")
			}
		})
	}
}

func TestExportFailedJobRequiresIntactEvidence(t *testing.T) {
	for _, mode := range []string{"active", "nonterminal pod", "missing PVC", "empty PVC UID", "empty Job UID", "lost PVC", "condition false", "condition removed during preflight", "Job replaced during preflight"} {
		t.Run(mode, func(t *testing.T) {
			c, spec := exportSession(t)
			job, err := c.BatchV1().Jobs(spec.Namespace).Get(t.Context(), "record-"+spec.ID, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			job.Spec.Suspend = nil
			job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
			switch mode {
			case "active":
				job.Status.Active = 1
			case "empty Job UID":
				job.UID = ""
			case "condition false":
				job.Status.Conditions[0].Status = corev1.ConditionFalse
			case "nonterminal pod":
				pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "writer", Namespace: spec.Namespace, Labels: sessionLabels(spec.ID),
					OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, batchv1.SchemeGroupVersion.WithKind("Job"))}}}
				if _, err := c.CoreV1().Pods(spec.Namespace).Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			case "missing PVC":
				if err := c.CoreV1().PersistentVolumeClaims(spec.Namespace).Delete(t.Context(), job.Name, metav1.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
			case "empty PVC UID", "lost PVC":
				pvc, err := c.CoreV1().PersistentVolumeClaims(spec.Namespace).Get(t.Context(), job.Name, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if mode == "lost PVC" {
					pvc.Status.Phase = corev1.ClaimLost
				} else {
					pvc.UID = ""
				}
				if _, err := c.CoreV1().PersistentVolumeClaims(spec.Namespace).Update(t.Context(), pvc, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := c.BatchV1().Jobs(spec.Namespace).Update(t.Context(), job, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			if mode == "condition removed during preflight" || mode == "Job replaced during preflight" {
				gets := 0
				c.PrependReactor("get", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
					gets++
					if gets == 2 {
						changed := job.DeepCopy()
						if mode == "condition removed during preflight" {
							changed.Status.Conditions = nil
						} else {
							changed.UID = "replacement"
						}
						return true, changed, nil
					}
					return false, nil, nil
				})
			}
			c.ClearActions()
			_, err = exportWithExec(t.Context(), c, &rest.Config{}, spec.Namespace, spec.ID, filepath.Join(t.TempDir(), "export"),
				func(context.Context, *rest.Config, string, string, io.Writer) error {
					t.Fatal("unsafe failed Job reached exec")
					return nil
				})
			if err == nil {
				t.Fatal("unsafe failed Job export admitted")
			}
			for _, action := range c.Actions() {
				if action.GetVerb() != "get" && action.GetVerb() != "list" {
					t.Fatalf("failed preflight mutated resources: %v", action)
				}
			}
		})
	}
	if _, err := Export(t.Context(), nil, &rest.Config{}, "", "", filepath.Join(t.TempDir(), "export")); err == nil {
		t.Fatal("nil client and empty identity admitted")
	}
}

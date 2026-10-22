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

package adversary

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func runtimeSpec() RunSpec {
	return RunSpec{Version: "v1", ID: "test-run", Namespace: "capture", TargetNamespace: "hcp", Image: "registry.example/hcp-perf@sha256:" + strings.Repeat("a", 64), ReserveCPUMilli: reservePtr(500), MemoryRequestMi: 16, MemoryLimitMi: 32, DurationSeconds: 10}
}

func reservePtr(value int64) *int64 { return &value }

func runtimeNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name), Labels: map[string]string{workerRoleLabel: "worker", "kubernetes.azure.com/agentpool": "workers"}},
		Status:     corev1.NodeStatus{Capacity: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("8500m")}, Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("8"), corev1.ResourceMemory: resource.MustParse("8Gi"), corev1.ResourcePods: resource.MustParse("110")}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	}
}

func runtimePod(namespace, name, node, cpu string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID(namespace + "-" + name)}, Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "main", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}}}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
}

func runtimeClient(objects ...runtime.Object) *fake.Clientset {
	objects = append(objects, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "capture", Labels: map[string]string{managedByLabel: "hcp-perf"}}})
	client := fake.NewClientset(objects...)
	// The fake API does not assign UIDs/resourceVersions. Populate them to test
	// real ownership and JSON-patch preconditions rather than empty identities.
	client.PrependReactor("create", "*", func(action ktesting.Action) (bool, runtime.Object, error) {
		object := action.(ktesting.CreateAction).GetObject().(metav1.Object)
		object.SetUID(types.UID("uid-" + object.GetName()))
		object.SetResourceVersion("1")
		return false, nil, nil
	})
	return client
}

func TestStartDiscoveryAndScheduling(t *testing.T) {
	target := runtimePod("hcp", "api", "worker-a", "500m")
	other := runtimePod("other", "sidecars", "worker-a", "100m")
	always := corev1.ContainerRestartPolicyAlways
	other.Spec.InitContainers = []corev1.Container{
		{Name: "sidecar", RestartPolicy: &always, Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")}}},
		{Name: "init", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}}},
	}
	other.Spec.Overhead = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")}
	terminal := runtimePod("other", "done", "worker-a", "100")
	terminal.Status.Phase = corev1.PodSucceeded
	client := runtimeClient(runtimeNode("worker-a"), target, other, terminal)
	before := time.Now()
	status, err := Start(context.Background(), client, runtimeSpec())
	if err != nil {
		t.Fatal(err)
	}
	if status.Phase != "submitted" || status.Snapshot.CollectedAt.Before(before) || status.Snapshot.CollectedAt.After(time.Now()) {
		t.Fatalf("incorrect submission metadata: %#v", status)
	}
	// max(100+200, 200+2000)+50 + target(500) = 2750m.
	if got := status.Snapshot.Nodes[0].CommittedCPUMilli; got != 2750 {
		t.Fatalf("scheduler commitments = %d, want 2750", got)
	}
	if got := status.Plan.Nodes[0]; got.CPURequestMilli != 4750 || got.Workers != 9 {
		t.Fatalf("wrong headroom/capacity rounding: %#v", got)
	}
	job, err := client.BatchV1().Jobs("capture").Get(context.Background(), status.Jobs[0].Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod := job.Spec.Template.Spec
	container := pod.Containers[0]
	if pod.NodeName != "" || len(pod.Tolerations) != 0 || pod.RestartPolicy != corev1.RestartPolicyNever || *pod.AutomountServiceAccountToken {
		t.Fatalf("unsafe scheduler/pod configuration: %#v", pod)
	}
	term := pod.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0]
	if len(term.MatchFields) != 1 || term.MatchFields[0].Key != "metadata.name" || term.MatchFields[0].Values[0] != "worker-a" || term.MatchExpressions[0].Key != workerRoleLabel || term.MatchExpressions[0].Values[0] != "worker" {
		t.Fatalf("incorrect required affinity: %#v", term)
	}
	if container.Resources.Requests.Cpu().MilliValue() != 4750 || container.Resources.Requests.Memory().Value() != 16*1024*1024 || container.Resources.Limits.Memory().Value() != 32*1024*1024 {
		t.Fatalf("incorrect resources: %#v", container.Resources)
	}
	if _, ok := container.Resources.Limits[corev1.ResourceCPU]; ok {
		t.Fatal("CPU limit must be absent")
	}
	if !reflect.DeepEqual(container.Command, []string{"/hcp-perf"}) || !reflect.DeepEqual(container.Args, []string{"cpu", "burn", "--workers", "9", "--duration", "10s"}) || *job.Spec.ActiveDeadlineSeconds != 70 || *job.Spec.BackoffLimit != 0 {
		t.Fatalf("incorrect bounded workload: %#v", job.Spec)
	}
	if !*pod.SecurityContext.RunAsNonRoot || *pod.SecurityContext.RunAsUser == 0 || pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault || *container.SecurityContext.AllowPrivilegeEscalation || !*container.SecurityContext.ReadOnlyRootFilesystem || !reflect.DeepEqual(container.SecurityContext.Capabilities.Drop, []corev1.Capability{"ALL"}) {
		t.Fatal("missing restricted security settings")
	}
	cm, err := client.CoreV1().ConfigMaps("capture").Get(context.Background(), status.Registration, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var saved registration
	if err := json.Unmarshal([]byte(cm.Data["run.json"]), &saved); err != nil {
		t.Fatal(err)
	}
	if !*cm.Immutable || !reflect.DeepEqual(saved.Plan, status.Plan) || !reflect.DeepEqual(saved.Spec, runtimeSpec()) {
		t.Fatalf("registration did not retain resolved plan/spec: %#v", saved)
	}
	if owner := metav1.GetControllerOf(job); owner == nil || owner.UID != cm.UID {
		t.Fatal("Job not bound to registration UID")
	}
}

func TestStartPodRequests(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*corev1.Pod)
		want   int64
	}{
		{"pod level", func(p *corev1.Pod) {
			p.Spec.Resources = &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}}
		}, 2000},
		{"resize allocation", func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "main", Resources: &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}}, AllocatedResources: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3")}}}
		}, 3000},
		{"terminating still committed", func(p *corev1.Pod) { now := metav1.Now(); p.DeletionTimestamp = &now; p.Finalizers = []string{"test"} }, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := runtimePod("hcp", "api", "worker-a", "500m")
			tc.mutate(pod)
			status, err := Start(context.Background(), runtimeClient(runtimeNode("worker-a"), pod), runtimeSpec())
			if err != nil || status.Snapshot.Nodes[0].CommittedCPUMilli != tc.want {
				t.Fatalf("commitments want %d: %#v, %v", tc.want, status, err)
			}
		})
	}
}

func TestStartPreflight(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*RunSpec, *corev1.Node, *corev1.Pod)
		want   string
	}{
		{"infra", func(_ *RunSpec, n *corev1.Node, _ *corev1.Pod) { n.Labels[workerRoleLabel] = "infra" }, "not a worker"},
		{"wrong worker label", func(_ *RunSpec, n *corev1.Node, _ *corev1.Pod) {
			delete(n.Labels, workerRoleLabel)
			n.Labels["node-role.kubernetes.io/worker"] = ""
		}, "invalid role"},
		{"unready", func(_ *RunSpec, n *corev1.Node, _ *corev1.Pod) {
			n.Status.Conditions[0].Status = corev1.ConditionUnknown
		}, "Ready"},
		{"cordoned", func(_ *RunSpec, n *corev1.Node, _ *corev1.Pod) { n.Spec.Unschedulable = true }, "schedulable"},
		{"pressure", func(_ *RunSpec, n *corev1.Node, _ *corev1.Pod) {
			n.Status.Conditions = append(n.Status.Conditions, corev1.NodeCondition{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue})
		}, "MemoryPressure"},
		{"unscheduled", func(_ *RunSpec, _ *corev1.Node, p *corev1.Pod) {
			p.Spec.NodeName = ""
			p.Status.Phase = corev1.PodPending
		}, "unscheduled"},
		{"no live targets", func(_ *RunSpec, _ *corev1.Node, p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded }, "no live pods"},
		{"no targets", func(s *RunSpec, _ *corev1.Node, _ *corev1.Pod) { s.TargetNamespace = "absent" }, "no live pods"},
		{"headroom", func(s *RunSpec, _ *corev1.Node, _ *corev1.Pod) { s.ReserveCPUMilli = reservePtr(8000) }, "headroom"},
		{"workers bound", func(_ *RunSpec, n *corev1.Node, _ *corev1.Pod) {
			n.Status.Capacity[corev1.ResourceCPU] = resource.MustParse("1025")
		}, "1024"},
		{"mutable image", func(s *RunSpec, _ *corev1.Node, _ *corev1.Pod) { s.Image = "image:latest" }, "pinned"},
		{"missing namespace", func(s *RunSpec, _ *corev1.Node, _ *corev1.Pod) { s.Namespace = "" }, "explicit"},
		{"target namespace collision", func(s *RunSpec, _ *corev1.Node, _ *corev1.Pod) { s.Namespace = s.TargetNamespace }, "distinct"},
		{"bad id", func(s *RunSpec, _ *corev1.Node, _ *corev1.Pod) { s.ID = "../bad" }, "id"},
		{"version", func(s *RunSpec, _ *corev1.Node, _ *corev1.Pod) { s.Version = "v2" }, "version"},
		{"duration", func(s *RunSpec, _ *corev1.Node, _ *corev1.Pod) { s.DurationSeconds = 86401 }, "duration"},
		{"memory", func(s *RunSpec, _ *corev1.Node, _ *corev1.Pod) { s.MemoryLimitMi = 1 }, "memory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, node, pod := runtimeSpec(), runtimeNode("worker-a"), runtimePod("hcp", "api", "worker-a", "500m")
			tc.mutate(&spec, node, pod)
			client := runtimeClient(node, pod)
			if _, err := Start(context.Background(), client, spec); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			for _, action := range client.Actions() {
				if action.GetVerb() == "create" {
					t.Fatalf("preflight failure created %s", action.GetResource())
				}
			}
		})
	}
}

func TestRunCollisionAndPartialFailure(t *testing.T) {
	client := runtimeClient(runtimeNode("worker-a"), runtimeNode("worker-b"), runtimePod("hcp", "api", "worker-a", "500m"), runtimePod("hcp", "etcd", "worker-b", "500m"))
	creates := 0
	client.PrependReactor("create", "jobs", func(action ktesting.Action) (bool, runtime.Object, error) {
		creates++
		if creates == 2 {
			return true, nil, errors.New("quota exceeded")
		}
		return false, nil, nil
	})
	status, err := Start(context.Background(), client, runtimeSpec())
	if err == nil || status.Phase != "partial" || len(status.Jobs) != 2 || status.Jobs[0].Phase != "submitted" || status.Jobs[1].Phase != "creation-failed" {
		t.Fatalf("partial creation not recorded: %#v, %v", status, err)
	}
	if _, err := Start(context.Background(), client, runtimeSpec()); !apierrors.IsAlreadyExists(err) || creates != 2 {
		t.Fatalf("collision error=%v job creates=%d", err, creates)
	}
	observed, err := Status(context.Background(), client, "capture", "test-run")
	if err != nil || observed.Phase != "incomplete" || observed.Jobs[1].Phase != "missing" {
		t.Fatalf("missing Job was not reported: %#v, %v", observed, err)
	}
	stopped, err := Stop(context.Background(), client, "capture", "test-run")
	if err != nil || stopped.Phase != "stopping" || !stopped.SubmissionIncomplete || !stopped.StopRequested {
		t.Fatalf("partial run must remain unresolved: %#v, %v", stopped, err)
	}
	jobs, err := client.BatchV1().Jobs("capture").List(context.Background(), metav1.ListOptions{})
	if err != nil || len(jobs.Items) != 1 || !*jobs.Items[0].Spec.Suspend {
		t.Fatalf("Stop did not retain/suspend Job: %#v, %v", jobs, err)
	}
}

func TestStatusAndStop(t *testing.T) {
	ctx := context.Background()
	client := runtimeClient(runtimeNode("worker-a"), runtimePod("hcp", "api", "worker-a", "500m"))
	submitted, err := Start(ctx, client, runtimeSpec())
	if err != nil {
		t.Fatal(err)
	}
	job, err := client.BatchV1().Jobs("capture").Get(ctx, submitted.Jobs[0].Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	yes := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "burner-pod", Namespace: "capture", Labels: runLabels("test-run"), Annotations: job.Annotations, OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: &yes}}},
		Spec:       job.Spec.Template.Spec,
		Status:     corev1.PodStatus{Phase: corev1.PodPending, Reason: "Unschedulable", Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "Insufficient cpu"}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "cpu-burn", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}},
	}
	pod.Labels["batch.kubernetes.io/controller-uid"] = string(job.UID)
	if _, err := client.CoreV1().Pods("capture").Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	status, err := Status(ctx, client, "capture", "test-run")
	if err != nil || status.Phase != "pending" || status.Jobs[0].Pods[0].Ready || status.Jobs[0].Pods[0].Conditions[0].Message != "Insufficient cpu" || status.Jobs[0].Pods[0].Containers[0].State.Waiting.Reason != "ImagePullBackOff" {
		t.Fatalf("pending diagnostics lost: %#v, %v", status, err)
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}}
	if _, err := client.CoreV1().Pods("capture").UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	status, err = Status(ctx, client, "capture", "test-run")
	if err != nil || status.Phase != "running" || !status.Jobs[0].Pods[0].Ready || status.Jobs[0].Pods[0].Containers[0].State.Terminated.ExitCode != 137 || !strings.Contains(status.Validity, "not measured") {
		t.Fatalf("observations missing or mislabeled: %#v, %v", status, err)
	}
	status, err = Stop(ctx, client, "capture", "test-run")
	if err != nil || status.Phase != "stopping" {
		t.Fatalf("must wait for pods to disappear: %#v, %v", status, err)
	}
	if err := client.CoreV1().Pods("capture").Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	status, err = Stop(ctx, client, "capture", "test-run")
	if err != nil || status.Phase != "stopped" {
		t.Fatalf("stop not idempotent: %#v, %v", status, err)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "delete" && action.GetResource().Resource != "pods" {
			t.Fatalf("Stop deleted retained resources: %#v", action)
		}
		if action.GetVerb() == "patch" {
			var patch []map[string]any
			if err := json.Unmarshal(action.(ktesting.PatchAction).GetPatch(), &patch); err != nil {
				t.Fatal(err)
			}
			if len(patch) < 3 || patch[0]["path"] != "/metadata/uid" || patch[0]["op"] != "test" || patch[1]["path"] != "/metadata/resourceVersion" || patch[1]["op"] != "test" {
				t.Fatalf("unguarded patch: %#v", patch)
			}
		}
	}
}

func TestOwnershipRefusal(t *testing.T) {
	for _, kind := range []string{"namespace", "registration", "digest", "job-label", "job-owner", "job-digest"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			client := runtimeClient(runtimeNode("worker-a"), runtimePod("hcp", "api", "worker-a", "500m"))
			status, err := Start(ctx, client, runtimeSpec())
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "namespace":
				ns, _ := client.CoreV1().Namespaces().Get(ctx, "capture", metav1.GetOptions{})
				ns.Labels[managedByLabel] = "someone-else"
				_, err = client.CoreV1().Namespaces().Update(ctx, ns, metav1.UpdateOptions{})
			case "registration", "digest":
				cm, _ := client.CoreV1().ConfigMaps("capture").Get(ctx, status.Registration, metav1.GetOptions{})
				if kind == "digest" {
					cm.Data["run.json"] += " "
				} else {
					cm.Labels[managedByLabel] = "someone-else"
				}
				_, err = client.CoreV1().ConfigMaps("capture").Update(ctx, cm, metav1.UpdateOptions{})
			default:
				job, _ := client.BatchV1().Jobs("capture").Get(ctx, status.Jobs[0].Name, metav1.GetOptions{})
				switch kind {
				case "job-label":
					job.Labels[runIDLabel] = "other-run"
				case "job-owner":
					job.OwnerReferences[0].UID = "hijacked"
				case "job-digest":
					job.Annotations[digestAnnotation] = "hijacked"
				}
				_, err = client.BatchV1().Jobs("capture").Update(ctx, job, metav1.UpdateOptions{})
			}
			if err != nil {
				t.Fatal(err)
			}
			client.ClearActions()
			if _, err := Status(ctx, client, "capture", "test-run"); err == nil {
				t.Fatal("Status accepted hijacked ownership")
			}
			if _, err := Stop(ctx, client, "capture", "test-run"); err == nil {
				t.Fatal("Stop accepted hijacked ownership")
			}
			for _, action := range client.Actions() {
				// Stop may record intent on the owned registration before finding
				// a hijacked Job, but must never mutate that Job.
				if strings.HasPrefix(kind, "job-") && action.GetVerb() == "patch" && action.GetResource().Resource == "configmaps" {
					continue
				}
				if action.GetVerb() == "patch" || action.GetVerb() == "delete" {
					t.Fatalf("mutated hijacked resources: %#v", action)
				}
			}
		})
	}
}

func TestStopDuringJobSubmission(t *testing.T) {
	for _, nodeCount := range []int{1, 2} {
		t.Run(fmt.Sprint(nodeCount), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			objects := []runtime.Object{runtimeNode("worker-a"), runtimePod("hcp", "a", "worker-a", "500m")}
			if nodeCount == 2 {
				objects = append(objects, runtimeNode("worker-b"), runtimePod("hcp", "b", "worker-b", "500m"))
			}
			client := runtimeClient(objects...)
			// Fake holds its action lock while running reactors. A second client
			// sharing the tracker allows Stop to run while Create is paused.
			stopClient := fake.NewClientset()
			stopClient.PrependReactor("*", "*", ktesting.ObjectReaction(client.Tracker()))
			entered, release := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			client.PrependReactor("create", "jobs", func(action ktesting.Action) (bool, runtime.Object, error) {
				close(entered)
				select {
				case <-release:
					return false, nil, nil
				case <-ctx.Done():
					return true, nil, ctx.Err()
				}
			})
			type result struct {
				status RunStatus
				err    error
			}
			finished := make(chan result, 1)
			go func() { status, err := Start(ctx, client, runtimeSpec()); finished <- result{status, err} }()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("Start never reached Job creation")
			}
			stopping, err := Stop(ctx, stopClient, "capture", "test-run")
			if err != nil || stopping.Phase != "stopping" || !stopping.SubmissionIncomplete || !stopping.StopRequested {
				t.Fatalf("acknowledged stop with in-flight create: %#v, %v", stopping, err)
			}
			actions := stopClient.Actions()
			for i, action := range actions {
				if action.GetResource().Resource == "jobs" {
					found := false
					for _, earlier := range actions[:i] {
						found = found || earlier.GetVerb() == "patch" && earlier.GetResource().Resource == "configmaps"
					}
					if !found {
						t.Fatal("Stop read Jobs before persisting intent")
					}
					break
				}
			}
			close(release)
			var completed result
			select {
			case completed = <-finished:
			case <-ctx.Done():
				t.Fatal("Start did not finish after release")
			}
			wantPhase := "stopped"
			if nodeCount == 2 {
				wantPhase = "stopping"
			}
			if completed.err == nil || !strings.Contains(completed.err.Error(), "interrupted") || completed.status.Phase != wantPhase {
				t.Fatalf("Start ignored stop request: %#v, %v", completed.status, completed.err)
			}
			jobs, err := stopClient.BatchV1().Jobs("capture").List(ctx, metav1.ListOptions{})
			if err != nil || len(jobs.Items) != 1 || jobs.Items[0].Spec.Suspend == nil || !*jobs.Items[0].Spec.Suspend {
				t.Fatalf("racing Job not suspended or remaining Job submitted: %#v, %v", jobs, err)
			}
		})
	}
}

func TestStopRetainsTerminalPodDiagnostics(t *testing.T) {
	for _, phase := range []corev1.PodPhase{corev1.PodSucceeded, corev1.PodFailed, corev1.PodPending, corev1.PodRunning, corev1.PodUnknown} {
		t.Run(string(phase), func(t *testing.T) {
			ctx := context.Background()
			client := runtimeClient(runtimeNode("worker-a"), runtimePod("hcp", "api", "worker-a", "500m"))
			submitted, err := Start(ctx, client, runtimeSpec())
			if err != nil {
				t.Fatal(err)
			}
			job, err := client.BatchV1().Jobs("capture").Get(ctx, submitted.Jobs[0].Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			yes, now := true, metav1.Now()
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "retained", Namespace: "capture", Labels: runLabels("test-run"), Annotations: job.Annotations, DeletionTimestamp: &now, Finalizers: []string{"test"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: &yes}}},
				Status:     corev1.PodStatus{Phase: phase, ContainerStatuses: []corev1.ContainerStatus{{Name: "cpu-burn", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}}}}},
			}
			if _, err := client.CoreV1().Pods("capture").Create(ctx, pod, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			status, err := Stop(ctx, client, "capture", "test-run")
			wantPhase, wantCount := "stopping", 1
			if phase == corev1.PodSucceeded || phase == corev1.PodFailed {
				wantPhase, wantCount = "stopped", 0
			}
			if err != nil || status.Phase != wantPhase || status.NonterminalPods != wantCount {
				t.Fatalf("unexpected stop status: %#v, %v", status, err)
			}
			if len(status.Jobs[0].Pods) != 1 || status.Jobs[0].Pods[0].Containers[0].State.Terminated.ExitCode != 137 {
				t.Fatal("retained pod diagnostics lost")
			}
		})
	}
}

func TestStatusJobAdmissionEvents(t *testing.T) {
	ctx := context.Background()
	client := runtimeClient(runtimeNode("worker-a"), runtimePod("hcp", "api", "worker-a", "500m"))
	submitted, err := Start(ctx, client, runtimeSpec())
	if err != nil {
		t.Fatal(err)
	}
	job := submitted.Jobs[0]
	client.PrependReactor("list", "events", func(action ktesting.Action) (bool, runtime.Object, error) {
		list := action.(ktesting.ListActionImpl)
		if list.GetListRestrictions().Fields.String() != "involvedObject.uid="+string(job.UID) || list.GetListOptions().Limit != maxJobEvents {
			t.Fatalf("event query is not UID-scoped and bounded: %#v", list.GetListOptions())
		}
		result := &corev1.EventList{}
		for i := 0; i < maxJobEvents+2; i++ {
			uid := job.UID
			if i == 0 {
				uid = "old-job-uid"
			}
			result.Items = append(result.Items, corev1.Event{
				ObjectMeta:     metav1.ObjectMeta{Name: fmt.Sprintf("admission-%d", i), Namespace: "capture"},
				InvolvedObject: corev1.ObjectReference{APIVersion: "batch/v1", Kind: "Job", Namespace: "capture", Name: job.Name, UID: uid},
				Type:           "Warning", Reason: "FailedCreate", Message: "pods forbidden: violates PodSecurity", Count: 3,
			})
		}
		return true, result, nil
	})
	status, err := Status(ctx, client, "capture", "test-run")
	if err != nil {
		t.Fatal(err)
	}
	js := status.Jobs[0]
	if status.Phase != "pending" || len(js.Pods) != 0 || len(js.Events) != maxJobEvents || !js.EventsTruncated {
		t.Fatalf("missing admission diagnostics: %#v", status)
	}
	for _, event := range js.Events {
		if event.InvolvedObject.UID != job.UID || event.Reason != "FailedCreate" || event.Message != "pods forbidden: violates PodSecurity" || event.Count != 3 {
			t.Fatalf("unrelated or incomplete event: %#v", event)
		}
	}
}

func TestStatusMissingIsNotCompletion(t *testing.T) {
	ctx := context.Background()
	client := runtimeClient(runtimeNode("worker-a"), runtimeNode("worker-b"), runtimePod("hcp", "a", "worker-a", "500m"), runtimePod("hcp", "b", "worker-b", "500m"))
	status, err := Start(ctx, client, runtimeSpec())
	if err != nil {
		t.Fatal(err)
	}
	for _, js := range status.Jobs {
		job, _ := client.BatchV1().Jobs("capture").Get(ctx, js.Name, metav1.GetOptions{})
		job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
		if _, err := client.BatchV1().Jobs("capture").UpdateStatus(ctx, job, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	complete, err := Status(ctx, client, "capture", "test-run")
	if err != nil || complete.Phase != "completed" {
		t.Fatalf("complete = %#v, %v", complete, err)
	}
	job, err := client.BatchV1().Jobs("capture").Get(ctx, status.Jobs[0].Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "DeadlineExceeded"}}
	if _, err := client.BatchV1().Jobs("capture").UpdateStatus(ctx, job, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	failed, err := Status(ctx, client, "capture", "test-run")
	if err != nil || failed.Phase != "failed" || failed.Jobs[0].Conditions[0].Reason != "DeadlineExceeded" {
		t.Fatalf("failed = %#v, %v", failed, err)
	}
	if err := client.BatchV1().Jobs("capture").Delete(ctx, status.Jobs[1].Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	missing, err := Status(ctx, client, "capture", "test-run")
	if err != nil || missing.Phase != "incomplete" {
		t.Fatalf("missing = %#v, %v", missing, err)
	}
}

func TestStopUIDRace(t *testing.T) {
	ctx := context.Background()
	client := runtimeClient(runtimeNode("worker-a"), runtimePod("hcp", "api", "worker-a", "500m"))
	status, err := Start(ctx, client, runtimeSpec())
	if err != nil {
		t.Fatal(err)
	}
	client.PrependReactor("patch", "jobs", func(action ktesting.Action) (bool, runtime.Object, error) {
		job, err := client.Tracker().Get(batchv1.SchemeGroupVersion.WithResource("jobs"), "capture", status.Jobs[0].Name)
		if err != nil {
			return true, nil, err
		}
		replacement := job.(*batchv1.Job).DeepCopy()
		replacement.UID = "replacement"
		if err := client.Tracker().Update(batchv1.SchemeGroupVersion.WithResource("jobs"), replacement, "capture"); err != nil {
			return true, nil, err
		}
		return false, nil, nil
	})
	if _, err := Stop(ctx, client, "capture", "test-run"); err == nil {
		t.Fatal("UID replacement was not refused")
	}
	job, err := client.BatchV1().Jobs("capture").Get(ctx, status.Jobs[0].Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if job.Spec.Suspend != nil && *job.Spec.Suspend {
		t.Fatal("replacement was suspended")
	}
}

func TestJobNames(t *testing.T) {
	for i := 0; i < 10; i++ {
		name := jobName(strings.Repeat("a", 40), fmt.Sprintf("node-%d.example", i))
		if len(name) > 63 || name != jobName(strings.Repeat("a", 40), fmt.Sprintf("node-%d.example", i)) {
			t.Fatalf("unsafe or unstable job name: %q", name)
		}
	}
}

func TestStartRequiresOwnedNamespace(t *testing.T) {
	ctx := context.Background()
	client := runtimeClient(runtimeNode("worker-a"), runtimePod("hcp", "api", "worker-a", "500m"))
	ns, err := client.CoreV1().Namespaces().Get(ctx, "capture", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ns.Labels = nil
	if _, err := client.CoreV1().Namespaces().Update(ctx, ns, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Start(ctx, client, runtimeSpec()); err == nil {
		t.Fatal("Start accepted unowned namespace")
	}
}

func TestStatusDoesNotHidePodsWithChangedLabels(t *testing.T) {
	ctx := context.Background()
	client := runtimeClient(runtimeNode("worker-a"), runtimePod("hcp", "api", "worker-a", "500m"))
	status, err := Start(ctx, client, runtimeSpec())
	if err != nil {
		t.Fatal(err)
	}
	job, err := client.BatchV1().Jobs("capture").Get(ctx, status.Jobs[0].Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	yes := true
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "hidden", Namespace: "capture", OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: &yes}}}}
	if _, err := client.CoreV1().Pods("capture").Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Status(ctx, client, "capture", "test-run"); err == nil {
		t.Fatal("Status hid an owned pod with missing labels")
	}
	pod.Labels, pod.Annotations = runLabels("test-run"), job.Annotations
	if _, err := client.CoreV1().Pods("capture").Update(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := client.BatchV1().Jobs("capture").Delete(ctx, job.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if status, err := Stop(ctx, client, "capture", "test-run"); err == nil || status.Phase == "stopped" {
		t.Fatalf("Stop claimed stopped with orphaned pods: %#v, %v", status, err)
	}
}

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
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

const RecordExportIDLabel = "hcp-perf.azure.com/record-export-id"
const exportRegistrationAnnotation = "hcp-perf.azure.com/export-registration"
const exportAttemptAnnotation = "hcp-perf.azure.com/export-attempt"

const exportLimitations = "Archives are not sanitized. logs.ndjson contains RAW, UNSANITIZED logs and CAN CONTAIN SECRETS. Treat archives as sensitive; do not publish them as public artifacts. " +
	"SHA256 verifies archive bytes, not recording completeness. Files exceeding the declared limits fail export; they are never silently truncated."

// ExportManifest authenticates neither the recorder nor its data. SHA256 covers
// the complete validated archive, not recording completeness. The helper reference
// is retained on error for cleanup diagnostics. Cleanup failures preserve even a
// partial archive and write a manifest; only a nonempty SHA256 claims validation.
type ExportManifest struct {
	Version             string             `json:"version"`
	ID                  string             `json:"id"`
	Namespace           string             `json:"namespace"`
	OutputDir           string             `json:"outputDir"`
	Archive             string             `json:"archive"`
	Bytes               int64              `json:"bytes"`
	SHA256              string             `json:"sha256"`
	Helper              *ResourceReference `json:"helper,omitempty"`
	Status              SessionStatus      `json:"status"`
	MaxArchiveBytes     int64              `json:"maxArchiveBytes"`
	MaxArchiveFileBytes int64              `json:"maxArchiveFileBytes"`
	Limitations         string             `json:"limitations"`
	HelperCleanupFailed bool               `json:"helperCleanupFailed"`
	ConfiguredLogQPS    int64              `json:"configuredLogQPS,omitempty"`
	ConfiguredLogBurst  int64              `json:"configuredLogBurst,omitempty"`
}

// Export requires a completed, terminal failed or acknowledged suspended writer and no nonterminal
// recorder pods, as confirmed by Status. It never stops writers or deletes the
// PVC. It creates a read-only helper and streams the same pinned binary via the
// Kubernetes exec API, with the supplied REST config's TLS/authentication setup.
// outputDir must not exist; success leaves archive.tar and manifest.json, both
// 0600, in a 0700 directory. No remote paths are extracted locally.
// The archive is bounded to 32 GiB including tar headers, with an 8 GiB per-file
// limit; oversize data fails rather than truncates. Raw logs can contain secrets:
// private file permissions do not make archives safe to publish or share.
//
// Status reports the helper separately from writers. A live helper blocks export;
// an exactly owned terminal helper is cleaned up on retry. Do not resume/replace
// writers during export: Status is non-atomic. Cleanup is UID guarded and separately
// time bounded even after cancellation; failures are returned and recorded in the
// manifest without removing local evidence. The helper has a five-minute deadline.
func Export(ctx context.Context, client kubernetes.Interface, config *rest.Config, namespace, id, outputDir string) (ExportManifest, error) {
	return exportWithExec(ctx, client, config, namespace, id, outputDir, execExportArchive)
}

type exportExecFunc func(context.Context, *rest.Config, string, string, io.Writer) error

func exportWithExec(ctx context.Context, client kubernetes.Interface, config *rest.Config, namespace, id, outputDir string, exec exportExecFunc) (manifest ExportManifest, retErr error) {
	manifest = ExportManifest{Version: "v1", ID: id, Namespace: namespace, Archive: "archive.tar",
		MaxArchiveBytes: MaxArchiveBytes, MaxArchiveFileBytes: MaxArchiveFileBytes, Limitations: exportLimitations}
	if config == nil || outputDir == "" || exec == nil {
		return manifest, errors.New("REST config, new output directory and exec transport are required")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	s, err := Status(ctx, client, namespace, id)
	manifest.Status = s
	if err != nil {
		return manifest, err
	}
	failed := false
	for _, condition := range s.Conditions {
		failed = failed || condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue
	}
	if (s.Phase != "completed" && s.Phase != "stopped" && !(s.Phase == "failed" && failed)) || s.NonterminalPods != 0 || s.Storage == nil || s.Storage.UID == "" || s.Storage.Phase != corev1.ClaimBound {
		return manifest, errors.New("export requires a stopped or terminal recorder, no nonterminal pods and bound storage")
	}
	for _, ref := range s.Inventory {
		if ref.UID == "" {
			return manifest, errors.New("export requires confirmed resource UIDs")
		}
	}
	if s.ExportHelper != nil && s.ExportHelper.Phase != corev1.PodSucceeded && s.ExportHelper.Phase != corev1.PodFailed {
		return manifest, errors.New("an export helper is still active")
	}
	registrationStatus := sessionStatus(namespace, id)
	_, cm, err := loadSession(ctx, client, &registrationStatus)
	if err != nil {
		return manifest, err
	}
	for _, ref := range registrationStatus.Inventory {
		matched := false
		for _, original := range s.Inventory {
			if ref == original {
				matched = true
			}
		}
		if !matched {
			return manifest, errors.New("session identity changed during export preflight")
		}
	}
	job, err := client.BatchV1().Jobs(namespace).Get(ctx, cm.Name, metav1.GetOptions{})
	if err != nil {
		return manifest, sessionError("validate export writer Job", err)
	}
	confirmedJob := false
	for _, ref := range s.Inventory {
		confirmedJob = confirmedJob || ref.Kind == "Job" && ref.Name == job.Name && ref.UID == job.UID
	}
	terminal, suspended := false, false
	for _, condition := range job.Status.Conditions {
		if condition.Status == corev1.ConditionTrue {
			terminal = terminal || condition.Type == batchv1.JobComplete || condition.Type == batchv1.JobFailed
			suspended = suspended || condition.Type == batchv1.JobSuspended
		}
	}
	if !confirmedJob || !sessionOwned(job, id, cm, "ConfigMap", "v1") || job.DeletionTimestamp != nil || job.Status.Active != 0 ||
		(!terminal && !(suspended && job.Spec.Suspend != nil && *job.Spec.Suspend)) {
		return manifest, errors.New("writer Job identity or terminal/suspended state changed during export preflight")
	}
	var spec SessionSpec
	if err := json.Unmarshal([]byte(cm.Data["session.json"]), &spec); err != nil {
		return manifest, errors.New("invalid session registration")
	}
	manifest.ConfiguredLogQPS, manifest.ConfiguredLogBurst = spec.LogRequestQPS, spec.LogRequestBurst
	if s.ExportHelper != nil {
		manifest.Helper = &ResourceReference{Kind: "Pod", Name: s.ExportHelper.Name, Namespace: namespace, UID: s.ExportHelper.UID}
		if err := cleanupExportHelper(ctx, client, *manifest.Helper); err != nil {
			manifest.HelperCleanupFailed = true
			return manifest, err
		}
	}
	path, err := filepath.Abs(outputDir)
	if err != nil {
		return manifest, err
	}
	manifest.OutputDir = path
	if err := os.Mkdir(path, 0700); err != nil {
		return manifest, err
	}
	archive, err := os.OpenFile(filepath.Join(path, "archive.tar"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return manifest, errors.Join(err, os.Remove(path))
	}
	defer func() {
		retErr = errors.Join(retErr, archive.Sync(), archive.Close())
		if retErr == nil || manifest.HelperCleanupFailed {
			retErr = errors.Join(retErr, writeNewJSON(filepath.Join(path, "manifest.json"), manifest), syncDirectory(path), syncDirectory(filepath.Dir(path)))
		} else {
			retErr = errors.Join(retErr, os.Remove(filepath.Join(path, "archive.tar")), os.Remove(path))
		}
	}()
	attempt := rand.Text()
	helper, err := client.CoreV1().Pods(namespace).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "record-export-" + id, Namespace: namespace,
			Labels:          map[string]string{managedByLabel: "hcp-perf", RecordExportIDLabel: id, componentLabel: "record-export"},
			Annotations:     map[string]string{exportRegistrationAnnotation: exportRegistrationDigest(cm, id), exportAttemptAnnotation: attempt},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(cm, corev1.SchemeGroupVersion.WithKind("ConfigMap"))}},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever, ActiveDeadlineSeconds: sessionPtr(int64(300)),
			AutomountServiceAccountToken: sessionPtr(false), EnableServiceLinks: sessionPtr(false), TerminationGracePeriodSeconds: sessionPtr(int64(1)),
			NodeSelector: map[string]string{"aro-hcp.azure.com/role": "infra"},
			Tolerations:  []corev1.Toleration{{Key: "infra", Operator: corev1.TolerationOpEqual, Value: "true", Effect: corev1.TaintEffectNoSchedule}},
			SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: sessionPtr(true), RunAsUser: sessionPtr(int64(1000)), RunAsGroup: sessionPtr(int64(2000)),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			Containers: []corev1.Container{{Name: "export", Image: spec.Image, Command: []string{"/hcp-perf"}, Args: []string{"record", "hold", "--duration", "5m"},
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU: *resource.NewMilliQuantity(spec.CPURequestMilli, resource.DecimalSI), corev1.ResourceMemory: *resource.NewQuantity(spec.MemoryRequestMi*1024*1024, resource.BinarySI)},
					Limits: corev1.ResourceList{corev1.ResourceMemory: *resource.NewQuantity(spec.MemoryLimitMi*1024*1024, resource.BinarySI)}},
				SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: sessionPtr(false), ReadOnlyRootFilesystem: sessionPtr(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
				VolumeMounts:    []corev1.VolumeMount{{Name: "data", MountPath: "/data", ReadOnly: true}},
			}},
			Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: s.Storage.Name, ReadOnly: true}}}},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		retErr = sessionError("create export helper (create-only)", err)
		if apierrors.IsAlreadyExists(err) {
			return manifest, retErr
		}
		// A transport error can hide an accepted create. Only this invocation's
		// unpredictable attempt token establishes permission to clean it up.
		recoverCtx, recoverCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer recoverCancel()
		observed, getErr := client.CoreV1().Pods(namespace).Get(recoverCtx, "record-export-"+id, metav1.GetOptions{})
		if apierrors.IsNotFound(getErr) {
			return manifest, retErr
		}
		if getErr != nil {
			manifest.HelperCleanupFailed = true
			return manifest, errors.Join(retErr, errors.New("export helper cleanup failed: create outcome unresolved"))
		}
		if !exportHelperOwned(observed, cm, id) || observed.Annotations[exportAttemptAnnotation] != attempt {
			return manifest, errors.Join(retErr, errors.New("export helper create outcome does not match this attempt; refusing cleanup"))
		}
		helper = observed
	}
	manifest.Helper = &ResourceReference{Kind: "Pod", Name: helper.Name, Namespace: namespace, UID: helper.UID}
	if helper.UID == "" {
		manifest.HelperCleanupFailed = true
		return manifest, errors.New("created helper has no UID; refusing unguarded cleanup")
	}
	defer func() {
		if err := cleanupExportHelper(ctx, client, *manifest.Helper); err != nil {
			manifest.HelperCleanupFailed = true
			retErr = errors.Join(retErr, err)
		}
	}()
	if retErr != nil {
		return manifest, retErr
	}
	err = wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		pod, err := client.CoreV1().Pods(namespace).Get(ctx, helper.Name, metav1.GetOptions{})
		if err != nil {
			return false, sessionError("get export helper", err)
		}
		if pod.UID != helper.UID || pod.DeletionTimestamp != nil || pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			return false, errors.New("export helper replaced, terminating or terminal")
		}
		return pod.Status.Phase == corev1.PodRunning, nil
	})
	if err != nil {
		return manifest, err
	}
	hash := sha256.New()
	bounded := &exportWriter{ctx: ctx, cancel: cancel, out: io.MultiWriter(archive, hash), remaining: MaxArchiveBytes}
	if err := exec(ctx, config, namespace, helper.Name, bounded); err != nil {
		return manifest, errors.Join(bounded.err, sessionError("exec export archive", err))
	}
	if err := errors.Join(bounded.err, ctx.Err(), archive.Sync()); err != nil {
		return manifest, err
	}
	size := MaxArchiveBytes - bounded.remaining
	if err := validateExportArchive(archive, size); err != nil {
		return manifest, err
	}
	if err := ctx.Err(); err != nil {
		return manifest, err
	}
	manifest.Bytes, manifest.SHA256 = size, hex.EncodeToString(hash.Sum(nil))
	return manifest, nil
}

func exportRegistrationDigest(cm *corev1.ConfigMap, id string) string {
	digest := sha256.Sum256([]byte(cm.Namespace + "/" + string(cm.UID) + "/" + id + "/" + cm.Data["session.json"]))
	return hex.EncodeToString(digest[:])
}

func exportHelperOwned(pod *corev1.Pod, cm *corev1.ConfigMap, id string) bool {
	owner := metav1.GetControllerOf(pod)
	if pod.UID == "" || cm.UID == "" || pod.Namespace != cm.Namespace || pod.Name != "record-export-"+id ||
		pod.Labels[managedByLabel] != "hcp-perf" || pod.Labels[RecordExportIDLabel] != id || pod.Labels[componentLabel] != "record-export" || pod.Labels[RecordIDLabel] != "" ||
		owner == nil || owner.UID != cm.UID || owner.Name != cm.Name || owner.Kind != "ConfigMap" || owner.APIVersion != "v1" ||
		pod.Annotations[exportRegistrationAnnotation] != exportRegistrationDigest(cm, id) || pod.Annotations[exportAttemptAnnotation] == "" {
		return false
	}
	// Identity alone must not hide a writable PVC consumer from writer status.
	if len(pod.Spec.Volumes) != 1 || len(pod.Spec.Containers) != 1 || len(pod.Spec.InitContainers) != 0 || len(pod.Spec.EphemeralContainers) != 0 {
		return false
	}
	volume := pod.Spec.Volumes[0]
	mounts := pod.Spec.Containers[0].VolumeMounts
	return volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == cm.Name && volume.PersistentVolumeClaim.ReadOnly &&
		len(mounts) == 1 && mounts[0].Name == volume.Name && mounts[0].ReadOnly && mounts[0].MountPath == "/data" && len(pod.Spec.Containers[0].VolumeDevices) == 0
}

func cleanupExportHelper(ctx context.Context, client kubernetes.Interface, helper ResourceReference) error {
	if helper.UID == "" {
		return errors.New("export helper cleanup failed: missing UID")
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	err := client.CoreV1().Pods(helper.Namespace).Delete(cleanupCtx, helper.Name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &helper.UID}, GracePeriodSeconds: sessionPtr(int64(0)),
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return errors.New("export helper cleanup failed")
	}
	// Delete acceptance is not deletion: finalizers can retain even terminal pods.
	err = wait.PollUntilContextCancel(cleanupCtx, 100*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		pod, err := client.CoreV1().Pods(helper.Namespace).Get(ctx, helper.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		// A replacement is not ours and must never be deleted.
		return pod.UID != helper.UID, nil
	})
	if err != nil {
		return errors.New("export helper cleanup failed: deletion not confirmed")
	}
	return nil
}

func execExportArchive(ctx context.Context, config *rest.Config, namespace, pod string, stdout io.Writer) error {
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}
	request := client.CoreV1().RESTClient().Post().Namespace(namespace).Resource("pods").Name(pod).SubResource("exec").VersionedParams(&corev1.PodExecOptions{
		Container: "export", Command: []string{"/hcp-perf", "record", "archive", "--input", SessionOutputDir}, Stdout: true, Stderr: true,
	}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(config, http.MethodPost, request.URL())
	if err != nil {
		return err
	}
	// Never surface arbitrary remote stderr (which may contain credentials).
	return executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: stdout, Stderr: io.Discard})
}

type exportWriter struct {
	ctx       context.Context
	cancel    context.CancelFunc
	out       io.Writer
	remaining int64
	err       error
}

func (w *exportWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if err := w.ctx.Err(); err != nil {
		w.err = err
		return 0, err
	}
	if int64(len(p)) > w.remaining {
		w.err = errors.New("export exceeds archive byte limit")
		w.cancel()
		return 0, w.err
	}
	n, err := w.out.Write(p)
	w.remaining -= int64(n)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.err = err
		w.cancel()
	}
	return n, err
}

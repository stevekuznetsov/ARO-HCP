// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0.
package performance

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"text/template"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/seed"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/workload"
)

func populationKey(r schema.GroupVersionResource, namespace, name string) string {
	return r.Resource + "/" + namespace + "/" + name
}

// Use the creation renderers, not an independently maintained list of allowed
// names/APF/RBAC policies. Only screened checkpoints require this stricter proof.
func screenedTopology(s Spec) (map[string]*unstructured.Unstructured, error) {
	bundle, err := seed.Render(s.Seed)
	if err != nil {
		return nil, err
	}
	objects, err := workload.SetupObjects(s.setup())
	if err != nil {
		return nil, err
	}
	for n := int64(0); n < bundle.Namespaces; n++ {
		objects = append(objects, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%d", s.Seed.NamespacePrefix, n), Labels: map[string]string{seed.RunIDLabel: s.Seed.ID, "kube-burner-uuid": s.Seed.ID, "kube-burner-job": s.Seed.NamespacePrefix}}})
		identity, err := seed.IdentityObjects(s.Seed, n)
		if err != nil {
			return nil, err
		}
		objects = append(objects, identity...)
	}
	for name, source := range bundle.Templates {
		t, err := template.New(name).Parse(source)
		if err != nil {
			return nil, err
		}
		for i := int64(0); i < s.Seed.Applications; i++ {
			var b bytes.Buffer
			if err := t.Execute(&b, map[string]any{"Iteration": i}); err != nil {
				return nil, err
			}
			var o unstructured.Unstructured
			raw, err := yaml.YAMLToJSON(b.Bytes())
			if err != nil {
				return nil, err
			}
			if err := o.UnmarshalJSON(raw); err != nil {
				return nil, err
			}
			o.SetNamespace(fmt.Sprintf("%s-%d", s.Seed.NamespacePrefix, i/20))
			if o.GetKind() == "Secret" {
				o.Object["data"] = map[string]any{"synthetic": base64.StdEncoding.EncodeToString([]byte("not-a-credential"))}
				delete(o.Object, "stringData")
			}
			objects = append(objects, &o)
		}
	}
	result := map[string]*unstructured.Unstructured{}
	resources := map[string]string{"Namespace": "namespaces", "ServiceAccount": "serviceaccounts", "Role": "roles", "RoleBinding": "rolebindings", "Deployment": "deployments", "Service": "services", "ConfigMap": "configmaps", "Secret": "secrets", "FlowSchema": "flowschemas", "PriorityLevelConfiguration": "prioritylevelconfigurations"}
	for _, object := range objects {
		u, ok := object.(*unstructured.Unstructured)
		if !ok {
			gvks, _, err := scheme.Scheme.ObjectKinds(object)
			if err != nil {
				return nil, err
			}
			object.GetObjectKind().SetGroupVersionKind(gvks[0])
			m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(object)
			if err != nil {
				return nil, err
			}
			u = &unstructured.Unstructured{Object: m}
		}
		r := schema.GroupVersionResource{Group: u.GroupVersionKind().Group, Version: u.GroupVersionKind().Version, Resource: resources[u.GetKind()]}
		result[populationKey(r, u.GetNamespace(), u.GetName())] = u
	}
	return result, nil
}

var screenedRollout = regexp.MustCompile(`^[a-f0-9]{32}-[0-9]{6}$`)

func screenedObject(s Spec, actual, expected *unstructured.Unstructured) error {
	if actual.GetAPIVersion() != expected.GetAPIVersion() || actual.GetKind() != expected.GetKind() || len(actual.GetOwnerReferences()) != 0 {
		return errors.New("object type or owner differs from native contract")
	}
	for k, v := range expected.GetLabels() {
		if actual.GetLabels()[k] != v {
			return fmt.Errorf("native label %s changed", k)
		}
	}
	if actual.GetKind() == "Deployment" {
		var got, want appsv1.Deployment
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(actual.Object, &got); err != nil {
			return err
		}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(expected.Object, &want); err != nil {
			return err
		}
		labels := got.Spec.Template.Labels
		// Kube-burner copies its tracking labels onto the application template.
		if got.Namespace != s.setup().CanaryNamespace() {
			for _, key := range []string{"kube-burner-index", "kube-burner-job", "kube-burner-runid", "kube-burner-uuid", "kube-burner.io/job-iteration", "kube-burner.io/replica"} {
				if value, ok := labels[key]; ok {
					if actual.GetLabels()[key] != value {
						return errors.New("kube-burner template identity mismatch")
					}
					delete(labels, key)
				}
			}
		}
		if marker := got.Spec.Template.Annotations[workload.RolloutIDAnnotation]; marker != "" {
			if got.Namespace == s.setup().CanaryNamespace() || !screenedRollout.MatchString(marker) {
				return errors.New("invalid native rollout marker")
			}
			delete(got.Spec.Template.Annotations, workload.RolloutIDAnnotation)
		}
		if len(got.Spec.Template.Annotations) == 0 {
			got.Spec.Template.Annotations = nil
		}
		defaultScreenedDeployment(&got)
		defaultScreenedDeployment(&want)
		if !equality.Semantic.DeepEqual(got.Spec, want.Spec) {
			return errors.New("Deployment differs from native rendered seed/setup")
		}
		return nil
	}
	got, want := actual.DeepCopy().Object, expected.DeepCopy().Object
	for _, m := range []map[string]any{got, want} {
		delete(m, "metadata")
		delete(m, "status")
	}
	switch actual.GetKind() {
	case "Namespace":
		// The namespace controller adds only its standard finalizer here.
		if v, ok, _ := unstructured.NestedSlice(got, "spec", "finalizers"); ok && !reflect.DeepEqual(v, []any{"kubernetes"}) {
			return errors.New("unexpected namespace finalizers")
		}
		if spec, ok, err := unstructured.NestedMap(got, "spec"); err != nil || ok && len(spec) > 1 {
			return errors.New("unexpected namespace spec")
		}
		delete(got, "spec")
		delete(want, "spec")
	case "ServiceAccount":
		// Admission/controllers attach pull secrets; their exact content is still
		// bound by the checkpoint hash. They do not change benchmark API authority.
		delete(got, "imagePullSecrets")
		// OpenShift also adds dockercfg references (and older Kubernetes adds
		// token references) to secrets. Normalize only this unspecified list,
		// never automount policy or the original object used for inventory hashing.
		if _, specified := want["secrets"]; !specified {
			refs, _, err := unstructured.NestedSlice(got, "secrets")
			if err != nil {
				return errors.New("invalid ServiceAccount secret references")
			}
			seen := map[string]bool{}
			for _, ref := range refs {
				entry, ok := ref.(map[string]any)
				name, named := entry["name"].(string)
				if !ok || !named || len(entry) != 1 || len(validation.IsDNS1123Subdomain(name)) != 0 || seen[name] {
					return errors.New("ServiceAccount secrets require unique name-only references")
				}
				seen[name] = true
			}
			delete(got, "secrets")
		}
	case "Service":
		var a, b corev1.Service
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(actual.Object, &a); err != nil {
			return err
		}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(expected.Object, &b); err != nil {
			return err
		}
		a.Spec.ClusterIP = ""
		a.Spec.ClusterIPs = nil
		a.Spec.IPFamilies = nil
		a.Spec.IPFamilyPolicy = nil
		if a.Spec.SessionAffinity == corev1.ServiceAffinityNone {
			a.Spec.SessionAffinity = ""
		}
		if a.Spec.InternalTrafficPolicy != nil && *a.Spec.InternalTrafficPolicy == corev1.ServiceInternalTrafficPolicyCluster {
			a.Spec.InternalTrafficPolicy = nil
		}
		if !equality.Semantic.DeepEqual(a.Spec, b.Spec) {
			return errors.New("Service differs from native seed")
		}
		return nil
	}
	if !reflect.DeepEqual(got, want) {
		return errors.New("object differs from native rendered seed/setup")
	}
	return nil
}

// These are Kubernetes API defaults, not allowances for arbitrary admission
// changes. Comparing the full typed spec also rejects added sidecars/volumes/env.
func defaultScreenedDeployment(d *appsv1.Deployment) {
	if d.Spec.ProgressDeadlineSeconds == nil {
		d.Spec.ProgressDeadlineSeconds = ptr.To(int32(600))
	}
	p := &d.Spec.Template.Spec
	if p.RestartPolicy == "" {
		p.RestartPolicy = corev1.RestartPolicyAlways
	}
	if p.DNSPolicy == "" {
		p.DNSPolicy = corev1.DNSClusterFirst
	}
	if p.SchedulerName == "" {
		p.SchedulerName = "default-scheduler"
	}
	if p.TerminationGracePeriodSeconds == nil {
		p.TerminationGracePeriodSeconds = ptr.To(int64(30))
	}
	if p.SecurityContext == nil {
		p.SecurityContext = &corev1.PodSecurityContext{}
	}
	if p.DeprecatedServiceAccount == "" {
		p.DeprecatedServiceAccount = p.ServiceAccountName
	}
	for i := range p.Containers {
		c := &p.Containers[i]
		if c.TerminationMessagePath == "" {
			c.TerminationMessagePath = "/dev/termination-log"
		}
		if c.TerminationMessagePolicy == "" {
			c.TerminationMessagePolicy = corev1.TerminationMessageReadFile
		}
	}
}

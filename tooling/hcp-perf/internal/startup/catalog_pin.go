// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0.
package startup

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

type CatalogPin struct {
	Kubeconfig              string            `json:"kubeconfig"`
	AllowExecCredential     bool              `json:"allowExecCredential,omitempty"`
	Identity                Identity          `json:"identity"`
	ExpectedResourceVersion string            `json:"expectedResourceVersion"`
	ExpectedAnnotations     map[string]string `json:"expectedAnnotations"`
	Images                  map[string]string `json:"images"`
}

func (s CatalogPin) Validate() error {
	if s.Kubeconfig == "" || s.Identity.HCUID == "" || s.Identity.HCPUID == "" || s.Identity.NamespaceUID == "" || s.Identity.ResourceID == "" || s.ExpectedResourceVersion == "" || len(s.Images) != 3 {
		return errors.New("catalog pin requires exact target, resourceVersion and three digest images")
	}
	for _, catalog := range []string{"certified", "community", "redhat"} {
		if !pinnedImage.MatchString(s.Images[catalog]) {
			return errors.New("all three catalog images must be digest pinned")
		}
	}
	return nil
}

func catalogPinPatch(current *unstructured.Unstructured, s CatalogPin) ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if string(current.GetUID()) != s.Identity.HCUID || current.GetResourceVersion() != s.ExpectedResourceVersion || !reflect.DeepEqual(current.GetAnnotations(), s.ExpectedAnnotations) {
		return nil, errors.New("catalog pin HC preconditions changed")
	}
	patch := []map[string]any{{"op": "test", "path": "/metadata/uid", "value": s.Identity.HCUID}, {"op": "test", "path": "/metadata/resourceVersion", "value": s.ExpectedResourceVersion}}
	if current.GetAnnotations() == nil {
		patch = append(patch, map[string]any{"op": "add", "path": "/metadata/annotations", "value": map[string]string{}})
	}
	for _, catalog := range []string{"certified", "community", "redhat"} {
		key := "hypershift.openshift.io/" + catalog + "-operators-catalog-image"
		if old := s.ExpectedAnnotations[key]; old != "" && old != s.Images[catalog] {
			return nil, errors.New("refusing to replace an existing different catalog pin")
		}
		patch = append(patch, map[string]any{"op": "add", "path": "/metadata/annotations/" + strings.ReplaceAll(key, "/", "~1"), "value": s.Images[catalog]})
	}
	return json.Marshal(patch)
}

// PinCatalogs changes only the supported HC configuration annotations. CPO owns
// downstream image selection and trigger removal. Never replay an uncertain patch.
func PinCatalogs(ctx context.Context, clients Clients, s CatalogPin) (retErr error) {
	sent := false
	defer func() {
		if retErr != nil && !sent {
			retErr = &NoMutationError{Err: retErr}
		}
	}()
	if err := s.Validate(); err != nil {
		return err
	}
	verify := func() error {
		list, err := clients.Dynamic.Resource(HostedClusters).List(ctx, metav1.ListOptions{})
		if err != nil {
			return err
		}
		if len(list.Items) != 1 || string(list.Items[0].GetUID()) != s.Identity.HCUID {
			return errors.New("catalog pin requires exactly one pinned HC")
		}
		id, err := Discover(ctx, clients, s.Identity.ResourceID, s.Identity)
		if err != nil {
			return err
		}
		if id != s.Identity {
			return errors.New("catalog pin HCP identity changed")
		}
		return nil
	}
	if err := verify(); err != nil {
		return err
	}
	r := clients.Dynamic.Resource(HostedClusters).Namespace(s.Identity.HCNamespace)
	current, err := r.Get(ctx, s.Identity.HCName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	// HC status writers advance RV continuously. The reviewed annotation map
	// remains exact; bind the atomic patch to this immediate GET's RV.
	if !reflect.DeepEqual(current.GetAnnotations(), s.ExpectedAnnotations) {
		return errors.New("catalog annotations changed since review")
	}
	s.ExpectedResourceVersion = current.GetResourceVersion()
	// Pin the currently selected bytes only, including the extraction image.
	for catalog, image := range s.Images {
		d, err := clients.Kubernetes.AppsV1().Deployments(s.Identity.HCPNamespace).Get(ctx, catalog+"-operators-catalog", metav1.GetOptions{})
		if err != nil {
			return err
		}
		main, init := false, false
		for _, c := range d.Spec.Template.Spec.Containers {
			if c.Name == "registry" {
				main = c.Image == image
			}
		}
		for _, c := range d.Spec.Template.Spec.InitContainers {
			if c.Name == "extract-content" {
				init = c.Image == image
			}
		}
		if !main || !init {
			return errors.New("catalog pin differs from current selected bytes")
		}
	}
	patch, err := catalogPinPatch(current, s)
	if err != nil {
		return err
	}
	desired := current.DeepCopy()
	annotations := desired.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	for name, image := range s.Images {
		annotations["hypershift.openshift.io/"+name+"-operators-catalog-image"] = image
	}
	desired.SetAnnotations(annotations)
	dry, err := r.Patch(ctx, s.Identity.HCName, types.JSONPatchType, patch, metav1.PatchOptions{DryRun: []string{metav1.DryRunAll}, FieldManager: "hcp-perf-catalog-pin"})
	if err != nil {
		return err
	}
	if dry.GetUID() != current.GetUID() || !reflect.DeepEqual(dry.Object["spec"], current.Object["spec"]) || !reflect.DeepEqual(dry.GetAnnotations(), desired.GetAnnotations()) {
		return errors.New("catalog pin dry-run changed desired metadata/spec")
	}
	if err := verify(); err != nil {
		return err
	}
	sent = true
	if _, err := r.Patch(ctx, s.Identity.HCName, types.JSONPatchType, patch, metav1.PatchOptions{FieldManager: "hcp-perf-catalog-pin"}); err != nil {
		return err
	}
	final, err := r.Get(ctx, s.Identity.HCName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if final.GetUID() != current.GetUID() || !reflect.DeepEqual(final.Object["spec"], current.Object["spec"]) || !reflect.DeepEqual(final.GetAnnotations(), desired.GetAnnotations()) {
		return errors.New("catalog pin post-write verification mismatch")
	}
	return nil
}

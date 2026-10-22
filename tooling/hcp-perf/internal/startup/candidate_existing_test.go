// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0.
package startup

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestExistingCandidateInventoryGuard(t *testing.T) {
	for _, scenario := range []string{"valid", "extra-hc", "hc-uid", "hcp-uid", "namespace-uid", "rv", "selector"} {
		t.Run(scenario, func(t *testing.T) {
			hc := object("HostedCluster", "hc", "cluster", "hc-uid", map[string]any{"clusterID": "140ce363-e35f-4513-b031-cc7e754685b5"})
			hc.SetAnnotations(map[string]string{ResourceIDAnnotation: testResourceID})
			hcp := object("HostedControlPlane", "cp", "cluster", "hcp-uid", map[string]any{})
			hcp.SetAnnotations(map[string]string{HostedClusterAnnotation: "hc/cluster"})
			client := dynamicClient(candidateObject(), hc, hcp)
			clients := Clients{Dynamic: client, Kubernetes: kubefake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "cp", UID: "ns-uid"}})}
			identity, err := Discover(t.Context(), clients, testResourceID, Identity{})
			if err != nil {
				t.Fatal(err)
			}
			candidate := Candidate{Kubeconfig: "/external/kube", Name: "cluster", UID: "csc-uid", ResourceVersion: "7", ExpectedSpec: json.RawMessage(`{"value":"old"}`), DesiredSpec: json.RawMessage(`{"value":"new"}`)}
			switch scenario {
			case "extra-hc":
				_, err = client.Resource(HostedClusters).Namespace("other").Create(t.Context(), object("HostedCluster", "other", "other", "extra", map[string]any{}), metav1.CreateOptions{})
				if err != nil {
					t.Fatal(err)
				}
			case "hc-uid":
				identity.HCUID = "wrong"
			case "hcp-uid":
				identity.HCPUID = "wrong"
			case "namespace-uid":
				identity.NamespaceUID = "wrong"
			case "rv":
				candidate.ResourceVersion = "wrong"
			case "selector":
				candidate.EmptyHostedClusterSelector = "selected=true"
			}
			writes := 0
			client.PrependReactor("update", "clustersizingconfigurations", func(a clienttesting.Action) (bool, runtime.Object, error) {
				u := a.(clienttesting.UpdateActionImpl)
				if len(u.UpdateOptions.DryRun) > 0 {
					return true, u.GetObject().DeepCopyObject(), nil
				}
				writes++
				return false, nil, nil
			})
			err = ApplyExisting(t.Context(), clients, candidate, identity)
			if scenario == "valid" {
				if err != nil || writes != 1 {
					t.Fatalf("valid: %v writes%d", err, writes)
				}
			} else if err == nil || writes != 0 {
				t.Fatalf("unsafe: %v writes%d", err, writes)
			}
		})
	}
}

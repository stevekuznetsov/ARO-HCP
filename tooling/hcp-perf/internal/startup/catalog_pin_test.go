// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0.
package startup

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCatalogPinPatchGuards(t *testing.T) {
	for _, mode := range []string{"valid", "uid", "rv", "annotations", "partial", "tag", "otherpin"} {
		t.Run(mode, func(t *testing.T) {
			current := object("HostedCluster", "hc", "cluster", "uid", map[string]any{})
			current.SetResourceVersion("1")
			current.SetAnnotations(map[string]string{"unrelated": "preserved"})
			image := "registry.example/catalog@sha256:" + strings.Repeat("a", 64)
			s := CatalogPin{Kubeconfig: "/kube", Identity: Identity{HCUID: "uid", HCPUID: "hcp", NamespaceUID: "ns", ResourceID: testResourceID}, ExpectedResourceVersion: "1", ExpectedAnnotations: current.GetAnnotations(), Images: map[string]string{"certified": image, "community": image, "redhat": image}}
			switch mode {
			case "uid":
				s.Identity.HCUID = "other"
			case "rv":
				s.ExpectedResourceVersion = "2"
			case "annotations":
				s.ExpectedAnnotations = map[string]string{}
			case "partial":
				delete(s.Images, "redhat")
			case "tag":
				s.Images["redhat"] = "registry.example/catalog:latest"
			case "otherpin":
				s.ExpectedAnnotations["hypershift.openshift.io/redhat-operators-catalog-image"] = "other"
				current.SetAnnotations(s.ExpectedAnnotations)
			}
			patch, err := catalogPinPatch(current, s)
			if (err == nil) != (mode == "valid") {
				t.Fatalf("%s: %v", mode, err)
			}
			if mode == "valid" {
				var ops []map[string]any
				if json.Unmarshal(patch, &ops) != nil || len(ops) != 5 {
					t.Fatal("patch must contain UID/RV tests and exactly3annotation additions")
				}
				for _, op := range ops[2:] {
					if op["op"] != "add" || !strings.HasPrefix(op["path"].(string), "/metadata/annotations/hypershift.openshift.io~1") {
						t.Fatal("unexpected mutation")
					}
				}
			}
		})
	}
}

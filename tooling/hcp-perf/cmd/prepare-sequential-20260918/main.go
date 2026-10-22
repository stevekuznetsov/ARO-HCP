// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Preparation only: GET/LIST and exclusive local files. No controller submission.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/iteration"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/startup"
)

const root = "/home/stevekuznetsov/code/Azure/ARO-HCP/src/github.com/Azure/ARO-HCP-perf-runner"
const out = root + "/_artifacts/sequential-medium-20260918T1125Z"
const prior = root + "/_artifacts/go-small-controller-20260914"
const frozen = root + "/_artifacts/medium150-20260918-v3"
const sub = "1d3378d3-5a3f-4712-85a1-2485495dfc4b"

func must(err error) {
	if err != nil {
		panic(err)
	}
}
func load(path string, v any) { b, e := os.ReadFile(path); must(e); must(json.Unmarshal(b, v)) }
func save(name string, v any) {
	b, e := json.MarshalIndent(v, "", "  ")
	must(e)
	f, e := os.OpenFile(filepath.Join(out, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0400)
	must(e)
	_, e = f.Write(append(b, '\n'))
	must(e)
	must(f.Sync())
	must(f.Close())
}
func command(name string, args ...string) []byte {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, name, args...)
	c.Env = append(os.Environ(), "AZURE_CONFIG_DIR="+os.Getenv("HOME")+"/.azure-redhat")
	b, err := c.Output()
	if err != nil {
		panic(fmt.Sprintf("read-only command %s failed: %v", name, err))
	}
	return b
}
func record(name, cmd string, args ...string) json.RawMessage {
	b := command(cmd, args...)
	var v json.RawMessage
	must(json.Unmarshal(b, &v))
	save(name, v)
	return v
}
func kube(name, resource string, extra ...string) json.RawMessage {
	a := []string{"--kubeconfig", prior + "/private/management.kubeconfig", "--request-timeout=30s", "get", resource}
	a = append(a, extra...)
	a = append(a, "-o", "json")
	return record(name, "kubectl", a...)
}
func main() {
	if len(os.Args) != 2 {
		panic("require delete, inspect, medium, or seal")
	}
	switch os.Args[1] {
	case "delete":
		var s iteration.Spec
		load(prior+"/inputs/cleanup75-02.json", &s)
		s.Environment.Incarnation = "sequential-small-delete-20260918T1125Z"
		s.Actions[0].Retention.Duration = 90 * time.Minute
		s.Actions[0].Timeout = 95 * time.Minute
		s.Actions[0].Retention.OptionalResourceGroups = []string{"/subscriptions/" + sub + "/resourceGroups/hcp-perf-stev-go-small-75-03-20260914-managed"}
		s.Actions[1].RP.Resource.Cluster = "perf-go-small-75-03"
		must(s.Validate())
		save("small-delete.spec.json", s)
		fmt.Println("Validated and froze small-delete.spec.json; NOT submitted")
	case "inspect":
		save("started.json", time.Now().UTC())
		record("account.json", "az", "account", "show", "--query", "{subscription:id,tenant:tenantId,state:state,user:user.name}", "-o", "json")
		record("cli-token-expiry.json", "az", "account", "get-access-token", "--query", "{subscription:subscription,tenant:tenant,expires_on:expires_on}", "-o", "json")
		token, e := os.ReadFile(prior + "/private/arm-token")
		must(e)
		parts := strings.Split(strings.TrimSpace(string(token)), ".")
		if len(parts) != 3 {
			panic("invalid token")
		}
		claims, e := base64.RawURLEncoding.DecodeString(parts[1])
		must(e)
		var all map[string]any
		must(json.Unmarshal(claims, &all))
		safe := map[string]any{}
		for _, k := range []string{"tid", "oid", "exp", "nbf", "aud", "unique_name"} {
			safe[k] = all[k]
		}
		save("retention-token-claims.json", safe)
		record("iterations.json", frozen+"/bin/hcp-perf", "iteration", "status", "--root", prior+"/state")
		for _, r := range []string{"hostedclusters", "hostedcontrolplanes", "nodes", "pods", "nodepools", "namespaces", "persistentvolumes", "manifestworks", "managedclusters"} {
			kube("management-"+r+".json", r, "-A")
		}
		kube("csc.json", "clustersizingconfiguration", "cluster")
		for _, r := range []string{"nodes", "deployments", "pods"} {
			record("guest-"+r+".json", "kubectl", "--kubeconfig", prior+"/private/guest-observe5-001.kubeconfig", "--request-timeout=30s", "get", r, "-A", "-o", "json")
		}
		var s iteration.Spec
		load(out+"/small-delete.spec.json", &s)
		for i, id := range append(append([]string{}, s.Actions[0].Retention.Required.ResourceGroups...), s.Actions[0].Retention.OptionalResourceGroups...) {
			record(fmt.Sprintf("group-%02d.json", i), "az", "group", "show", "--subscription", sub, "--name", filepath.Base(id), "-o", "json")
		}
		record("aks.json", "az", "aks", "show", "--subscription", sub, "-g", "hcp-underlay-pers-usw3stev-mgmt-1", "-n", "pers-usw3stev-mgmt-1", "--query", "{id:id,nodeResourceGroup:nodeResourceGroup,agentPoolProfiles:agentPoolProfiles}", "-o", "json")
		record("customer-vnet.json", "az", "network", "vnet", "show", "--subscription", sub, "-g", "hcp-perf-stev-unified-20260914", "-n", "customer-vnet", "-o", "json")
		p := s.Actions[1].RP
		for _, cluster := range []string{"perf-go-small-75-03", "perf-go-medium-150-01"} {
			url := p.Config.Endpoint + "/subscriptions/" + sub + "/resourceGroups/" + p.Resource.ResourceGroup + "/providers/Microsoft.RedHatOpenShift/hcpOpenShiftClusters/" + cluster + "?api-version=" + p.Config.APIVersion
			req, e := http.NewRequest(http.MethodGet, url, nil)
			must(e)
			req.Header.Set("X-Ms-Home-Tenant-Id", p.Config.TenantID)
			req.Header.Set("X-Ms-Client-Tenant-Id", p.Config.TenantID)
			req.Header.Set("X-Ms-Client-Object-Id", p.Config.ObjectID)
			req.Header.Set("X-Ms-Client-Principal-Name", p.Config.Principal)
			res, e := (&http.Client{Timeout: 30 * time.Second}).Do(req)
			must(e)
			var body json.RawMessage
			must(json.NewDecoder(res.Body).Decode(&body))
			must(res.Body.Close())
			save(cluster+".json", map[string]any{"status": res.StatusCode, "body": body})
		}
		for _, repo := range []string{"certified", "community", "redhat"} {
			record("catalog-"+repo+".json", "az", "acr", "manifest", "show-metadata", "--registry", "arohcpocpdev", "--name", "redhat/"+repo+"-operator-index:v4.21", "--query", "{digest:digest,attributes:changeableAttributes}", "-o", "json")
		}
		save("completed.json", time.Now().UTC())
	case "medium":
		var s iteration.Spec
		load(prior+"/state/small75-03/spec.json", &s)
		var draft struct {
			RPCreate     *iteration.RPParameters `json:"rpCreate"`
			WorkerCreate *iteration.RPParameters `json:"workerCreate"`
			Startup      startup.Spec            `json:"startupObserverDraft"`
		}
		load(root+"/_artifacts/medium-live-prerequisites-20260918T0300Z/medium-native-plan.draft.json", &draft)
		var csc struct {
			Metadata struct {
				UID string `json:"uid"`
				RV  string `json:"resourceVersion"`
			} `json:"metadata"`
			Spec json.RawMessage `json:"spec"`
		}
		load(out+"/csc.json", &csc)
		var desired struct {
			Spec json.RawMessage `json:"spec"`
		}
		load(frozen+"/native/hypershift.json", &desired)
		var live struct {
			Status int `json:"status"`
			Body   struct {
				Tags map[string]string `json:"tags"`
			} `json:"body"`
		}
		load(out+"/perf-go-small-75-03.json", &live)
		if live.Status != 200 {
			panic("Small RP identity not present")
		}
		var payload map[string]any
		must(json.Unmarshal(draft.RPCreate.Payload, &payload))
		payload["tags"] = live.Body.Tags
		if live.Body.Tags["aro-hcp.experimental.cluster.control-plane-exact-version"] != "4.21.32" {
			panic("exact release tag drift")
		}
		draft.RPCreate.Payload, _ = json.Marshal(payload)
		draft.Startup.CandidateSpec = desired.Spec
		for i := range draft.Startup.Components {
			c := &draft.Startup.Components[i]
			for _, family := range [][]startup.Container{c.Containers, c.InitContainers} {
				for j := range family {
					for _, repo := range []string{"certified", "community", "redhat"} {
						prefix := "arohcpocpdev.azurecr.io/redhat/" + repo + "-operator-index@"
						if strings.HasPrefix(family[j].Image, prefix) {
							var m struct {
								Digest string `json:"digest"`
							}
							load(out+"/catalog-"+repo+".json", &m)
							family[j].Image = prefix + m.Digest
						}
					}
				}
			}
		}
		s.Environment.Incarnation = "sequential-medium-20260918T1125Z"
		for i := range s.Actions {
			a := &s.Actions[i]
			switch a.Kind {
			case iteration.CostEstimate:
				load(frozen+"/native/cost-input.json", a.Cost)
			case iteration.RetentionRefresh:
				a.Retention.Duration = 3 * time.Hour
				a.Timeout = 3*time.Hour + 5*time.Minute
				a.Retention.OptionalResourceGroups = []string{"/subscriptions/" + sub + "/resourceGroups/hcp-perf-stev-go-medium-150-01-20260918-managed"}
			case iteration.CandidateApply:
				a.Candidate.UID = csc.Metadata.UID
				a.Candidate.ResourceVersion = csc.Metadata.RV
				a.Candidate.ExpectedSpec = csc.Spec
				a.Candidate.DesiredSpec = desired.Spec
				a.Candidate.EmptyHostedClusterSelector = ""
			case iteration.StartupObserve:
				a.Startup = &draft.Startup
				a.Timeout = 115 * time.Minute
			case iteration.RPCreateCluster:
				a.RP = draft.RPCreate
				a.Timeout = 110 * time.Minute
			case iteration.RPAdminCredential:
				a.RP.Resource = draft.RPCreate.Resource
				a.RP.CredentialPath = out + "/private/guest-medium150-001.kubeconfig"
			case iteration.RPCreateNodePool:
				a.RP = draft.WorkerCreate
				a.Timeout = 45 * time.Minute
			}
		}
		must(s.Validate())
		save("medium-startup.spec.json", s)
		fmt.Println("Validated and froze medium-startup.spec.json; NOT submitted; post-Small gates mandatory")
	case "seal":
		source, err := os.ReadFile(root + "/tooling/hcp-perf/cmd/prepare-sequential-20260918/main.go")
		must(err)
		save("preparation-source.json", string(source))
		hashes := map[string]string{}
		must(filepath.Walk(out, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				if p != out && (info.Name() == "tmp" || info.Name() == "private" || info.Name() == "go-cache") {
					return filepath.SkipDir
				}
				return nil
			}
			b, e := os.ReadFile(p)
			if e != nil {
				return e
			}
			rel, e := filepath.Rel(out, p)
			if e != nil {
				return e
			}
			hashes[rel] = fmt.Sprintf("%x", sha256.Sum256(b))
			return os.Chmod(p, 0400)
		}))
		save("SHA256.json", hashes)
	case "audit":
		var nodes corev1.NodeList
		var pods corev1.PodList
		load(out+"/management-nodes.json", &nodes)
		load(out+"/management-pods.json", &pods)
		type demand struct{ CPUMilli, MemoryBytes, SwiftNIC int64 }
		add := func(a, b demand) demand {
			return demand{a.CPUMilli + b.CPUMilli, a.MemoryBytes + b.MemoryBytes, a.SwiftNIC + b.SwiftNIC}
		}
		subtract := func(a, b demand) demand {
			return demand{a.CPUMilli - b.CPUMilli, a.MemoryBytes - b.MemoryBytes, a.SwiftNIC - b.SwiftNIC}
		}
		maximum := func(a, b demand) demand {
			return demand{max(a.CPUMilli, b.CPUMilli), max(a.MemoryBytes, b.MemoryBytes), max(a.SwiftNIC, b.SwiftNIC)}
		}
		requests := func(r corev1.ResourceList) demand {
			q := r["aro.openshift.io/swift-nic"]
			return demand{r.Cpu().MilliValue(), r.Memory().Value(), q.Value()}
		}
		type host struct {
			Name, Zone                                                      string
			Allocatable, Requests, SmallRequests, Free, ProjectedAfterSmall demand
		}
		hosts := map[string]*host{}
		for _, n := range nodes.Items {
			hosts[n.Name] = &host{Name: n.Name, Zone: n.Labels["topology.kubernetes.io/zone"], Allocatable: requests(n.Status.Allocatable)}
		}
		for _, p := range pods.Items {
			if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
				continue
			}
			h := hosts[p.Spec.NodeName]
			if h == nil {
				continue
			}
			steady, running, peak := demand{}, demand{}, demand{}
			for _, c := range p.Spec.Containers {
				steady = add(steady, requests(c.Resources.Requests))
			}
			for _, c := range p.Spec.InitContainers {
				r := requests(c.Resources.Requests)
				if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
					running = add(running, r)
					peak = maximum(peak, running)
				} else {
					peak = maximum(peak, add(running, r))
				}
			}
			d := add(maximum(add(steady, running), peak), requests(p.Spec.Overhead))
			h.Requests = add(h.Requests, d)
			if strings.HasPrefix(p.Namespace, "ocm-arohcppers-2srn8quu9sel8q2c83l9iaaeudkoou73") {
				h.SmallRequests = add(h.SmallRequests, d)
			}
		}
		for _, h := range hosts {
			h.Free = subtract(h.Allocatable, h.Requests)
			h.ProjectedAfterSmall = add(h.Free, h.SmallRequests)
		}
		save("capacity.json", hosts)
		var s iteration.Spec
		load(out+"/medium-startup.spec.json", &s)
		must(s.Validate())
		var d iteration.Spec
		load(out+"/small-delete.spec.json", &d)
		must(d.Validate())
		if len(d.Actions) != 2 || d.Actions[1].Kind != iteration.RPDelete || d.Actions[1].RP.Resource.NodePool != "" || d.Actions[1].RP.Resource.Cluster != "perf-go-small-75-03" {
			panic("delete target/action drift")
		}
		for _, a := range s.Actions {
			if a.Kind == iteration.RPDelete || a.Kind == iteration.RPUpdateClusterImage || a.Kind == iteration.CatalogPin || a.Kind == iteration.CandidateUpdate {
				panic("unexpected lifecycle mutation")
			}
		}
		// Verify the validator implementation used here is the reviewed V3 source.
		checked := 0
		must(filepath.Walk(frozen+"/source/tooling/hcp-perf", func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			rel, e := filepath.Rel(frozen+"/source/tooling/hcp-perf", p)
			if e != nil {
				return e
			}
			a, e := os.ReadFile(p)
			if e != nil {
				return e
			}
			b, e := os.ReadFile(root + "/tooling/hcp-perf/" + rel)
			if e != nil {
				return e
			}
			if sha256.Sum256(a) != sha256.Sum256(b) {
				return fmt.Errorf("V3 source drift: %s", rel)
			}
			checked++
			return nil
		}))
		save("validation.json", map[string]any{"at": time.Now().UTC(), "nativeSpecValidation": "both pass", "frozenSourceFilesMatched": checked, "submitted": false, "capacity": "projection only; recheck after Small removal"})
		for _, pair := range []struct{ Name, Image string }{
			{"cpo", "stevekuznetsov/control-plane-operator-perf@sha256:33e5c072b86237ce47e4cf8a0e87eb04e679b69bfc793d0e70efb1414f9903fb"},
			{"olm", "stevekuznetsov/package-server-perf@sha256:50b1a8603a52b59ca583c4b30d95d24efc3abb4e590473ddc2e626f835e9eb6c"},
			{"prober", "stevekuznetsov/availability-prober-perf@sha256:edc5ba19183cd4bb2924c43e75b353a6ba54810324da7bdf43637de67aae85e2"},
			{"release", "openshift-release-dev/ocp-release@sha256:afc68ed6ce1af80d6e43014d16ad7b3f2067be6bebd970aa556c6a445290c8ee"},
		} {
			record("registry-"+pair.Name+".json", "az", "acr", "manifest", "show-metadata", "--registry", "arohcpocpdev", "--name", pair.Image, "--query", "{digest:digest,attributes:changeableAttributes}", "-o", "json")
		}
		record("medium-managed-exists.json", "az", "group", "exists", "--subscription", sub, "-n", "hcp-perf-stev-go-medium-150-01-20260918-managed", "-o", "json")
	default:
		panic("unsupported preparation mode")
	}
}

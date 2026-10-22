// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package apiload generates bounded, open-loop reads of existing benchmark objects.
package apiload

import (
	"fmt"
	"math/rand"
	"net/url"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

const MaxAttempts = 1_000_000
const MaxResponseBytes = 32 << 20

type Spec struct {
	Version               string   `json:"version"`
	DurationSeconds       int      `json:"durationSeconds"`
	RatePerSecond         int      `json:"ratePerSecond"`
	MaxInFlight           int      `json:"maxInFlight"`
	RequestTimeoutSeconds int      `json:"requestTimeoutSeconds"`
	Seed                  *int64   `json:"seed,omitempty"`
	NamespacePrefix       string   `json:"namespacePrefix,omitempty"`
	Namespaces            []string `json:"namespaces"`
	Targets               []Target `json:"targets"`
}

type Target struct {
	Namespace  string `json:"namespace"`
	APIVersion string `json:"apiVersion"`
	Resource   string `json:"resource"`
	Verb       string `json:"verb"`
	Name       string `json:"name,omitempty"`
	Weight     int    `json:"weight"`
	Limit      *int64 `json:"limit,omitempty"`
}

// Validate also applies defaults to a copy, preserving explicit zero seed/limit.
func (s Spec) Validate() (Spec, error) {
	if s.Version != "v1" {
		return s, fmt.Errorf("version must be v1")
	}
	if s.DurationSeconds < 1 || s.DurationSeconds > 3600 || s.RatePerSecond < 1 || s.RatePerSecond > 10000 || s.MaxInFlight < 1 || s.MaxInFlight > 256 || s.RequestTimeoutSeconds < 1 || s.RequestTimeoutSeconds > 30 {
		return s, fmt.Errorf("require durationSeconds 1..3600, ratePerSecond 1..10000, maxInFlight 1..256, requestTimeoutSeconds 1..30")
	}
	if s.DurationSeconds*s.RatePerSecond > MaxAttempts {
		return s, fmt.Errorf("durationSeconds * ratePerSecond must not exceed %d", MaxAttempts)
	}
	if s.Seed == nil {
		seed := int64(42)
		s.Seed = &seed
	}
	if s.NamespacePrefix != "" && len(validation.IsDNS1123Label(s.NamespacePrefix)) != 0 {
		return s, fmt.Errorf("namespacePrefix must be a DNS label")
	}
	if len(s.Namespaces) == 0 || len(s.Namespaces) > 1000 || len(s.Targets) == 0 || len(s.Targets) > 1000 {
		return s, fmt.Errorf("require 1..1000 namespaces and targets")
	}
	namespaces := make(map[string]bool, len(s.Namespaces))
	for _, ns := range s.Namespaces {
		if namespaces[ns] || len(validation.IsDNS1123Label(ns)) != 0 || s.NamespacePrefix != "" && !strings.HasPrefix(ns, s.NamespacePrefix+"-") {
			return s, fmt.Errorf("namespaces must be unique DNS labels within namespacePrefix (when set)")
		}
		namespaces[ns] = true
	}
	s.Targets = append([]Target(nil), s.Targets...)
	used := map[string]bool{}
	for i := range s.Targets {
		t := &s.Targets[i]
		if !namespaces[t.Namespace] {
			return s, fmt.Errorf("target %d namespace is not in namespaces", i)
		}
		used[t.Namespace] = true
		if t.Weight < 1 || t.Weight > MaxAttempts {
			return s, fmt.Errorf("target %d weight must be 1..%d", i, MaxAttempts)
		}
		switch {
		case t.APIVersion == "apps/v1" && t.Resource == "deployments" && t.Verb == "get":
			if len(validation.IsDNS1123Subdomain(t.Name)) != 0 || t.Limit != nil {
				return s, fmt.Errorf("target %d deployment get requires a DNS-subdomain name and no limit", i)
			}
		case t.Verb == "list" && (t.APIVersion == "v1" && t.Resource == "pods" || t.APIVersion == "apps/v1" && t.Resource == "replicasets"):
			if t.Name != "" || t.Limit != nil && (*t.Limit < 0 || *t.Limit > 10000) {
				return s, fmt.Errorf("target %d list requires no name and limit 0..10000", i)
			}
			if t.Limit == nil {
				limit := int64(100)
				t.Limit = &limit
			}
		default:
			return s, fmt.Errorf("target %d: only apps/v1 deployments get, v1 pods list, and apps/v1 replicasets list are supported", i)
		}
	}
	if len(used) != len(namespaces) {
		return s, fmt.Errorf("each namespace must have at least one target")
	}
	return s, nil
}

func (t Target) requestURL(base *url.URL) string {
	u := *base
	prefix := "/api/v1"
	if t.APIVersion == "apps/v1" {
		prefix = "/apis/apps/v1"
	}
	u.Path = strings.TrimRight(u.Path, "/") + prefix + "/namespaces/" + t.Namespace + "/" + t.Resource
	if t.Verb == "get" {
		u.Path += "/" + t.Name
	} else {
		u.RawQuery = url.Values{"limit": {strconv.FormatInt(*t.Limit, 10)}}.Encode()
	}
	return u.String()
}

func chooseTarget(r *rand.Rand, targets []Target, total int) int {
	n := r.Intn(total)
	for i, t := range targets {
		if n < t.Weight {
			return i
		}
		n -= t.Weight
	}
	panic("invalid target weights")
}

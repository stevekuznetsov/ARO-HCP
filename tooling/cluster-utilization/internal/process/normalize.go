// Copyright 2025 Microsoft Corporation
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

package process

import (
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// Rule collapses namespaces matching Pattern into Replacement, so per-cluster
// namespaces (e.g. the per-HCP "ocm-<env>-<id>" namespaces, or per-cluster
// "klusterlet-<id>" namespaces) aggregate into a single stable identifier.
type Rule struct {
	Pattern     string `yaml:"pattern"`
	Replacement string `yaml:"replacement"`

	re *regexp.Regexp
}

// DefaultRules collapse the per-cluster namespace families we know about.
func DefaultRules() []Rule {
	return []Rule{
		{Pattern: `^ocm-.*`, Replacement: "ocm"},
		{Pattern: `^klusterlet-.*`, Replacement: "klusterlet"},
	}
}

// Normalizer applies namespace normalization rules (first match wins).
type Normalizer struct {
	rules []Rule
}

// NewNormalizer compiles rules. File rules (if any) take priority over the
// defaults, which are always appended as a fallback.
func NewNormalizer(rules []Rule) (*Normalizer, error) {
	all := append(append([]Rule{}, rules...), DefaultRules()...)
	for i := range all {
		re, err := regexp.Compile(all[i].Pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid namespace rule %q: %w", all[i].Pattern, err)
		}
		all[i].re = re
	}
	return &Normalizer{rules: all}, nil
}

// LoadRules reads normalization rules from a YAML file: a top-level `rules:`
// list of {pattern, replacement}.
func LoadRules(path string) ([]Rule, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Rules []Rule `yaml:"rules"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return doc.Rules, nil
}

// Apply returns the normalized namespace and a PromQL label matcher that selects
// the original namespace(s), suitable for a Grafana Explore deep link.
func (n *Normalizer) Apply(ns string) (norm, matcher string) {
	for _, r := range n.rules {
		if r.re.MatchString(ns) {
			return r.Replacement, fmt.Sprintf("namespace=~%q", r.Pattern)
		}
	}
	return ns, fmt.Sprintf("namespace=%q", ns)
}

// Matchers returns the map of normalized namespace name -> PromQL regex pattern
// for every collapsing rule, so the renderer can rebuild Explore links for
// normalized namespaces client-side.
func (n *Normalizer) Matchers() map[string]string {
	m := map[string]string{}
	for _, r := range n.rules {
		if _, ok := m[r.Replacement]; !ok {
			m[r.Replacement] = r.Pattern
		}
	}
	return m
}

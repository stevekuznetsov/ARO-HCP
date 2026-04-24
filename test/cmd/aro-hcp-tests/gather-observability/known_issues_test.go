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

package gatherobservability

import (
	"testing"
)

func TestParseKnownIssues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		wantErr bool
		wantLen int
	}{
		{
			name: "valid config",
			content: `knownIssues:
- name: "BackendOperationErrorRate"
  reason: "Known during provisioning"
- name: "BackendController.*"
  reason: "Controller churn known"
`,
			wantLen: 2,
		},
		{
			name:    "empty list",
			content: "knownIssues: []\n",
			wantLen: 0,
		},
		{
			name:    "missing name",
			content: "knownIssues:\n- reason: \"some reason\"\n",
			wantErr: true,
		},
		{
			name:    "missing reason",
			content: "knownIssues:\n- name: \"SomeAlert\"\n",
			wantErr: true,
		},
		{
			name:    "invalid yaml",
			content: "not: [valid: yaml",
			wantErr: true,
		},
		{
			name:    "invalid name regex",
			content: "knownIssues:\n- name: \"[invalid\"\n  reason: \"bad regex\"\n",
			wantErr: true,
		},
		{
			name: "invalid label regex",
			content: `knownIssues:
- name: "SomeAlert"
  reason: "test"
  labels:
    name: "[invalid"
`,
			wantErr: true,
		},
		{
			name: "with labels",
			content: `knownIssues:
- name: "BackendControllerRetryHotLoop"
  reason: "Known for delete controllers"
  labels:
    name: "operation.*delete"
`,
			wantLen: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result, err := parseKnownIssues([]byte(tt.content))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(result) != tt.wantLen {
				t.Errorf("got %d known issues, want %d", len(result), tt.wantLen)
			}
		})
	}
}

func TestLoadKnownIssuesEmbedded(t *testing.T) {
	issues, err := parseKnownIssues(defaultKnownIssuesData)
	if err != nil {
		t.Fatalf("embedded knownIssues.yaml should parse without error: %v", err)
	}
	if len(issues) == 0 {
		t.Fatal("embedded knownIssues.yaml should contain at least one known issue")
	}
}

func TestClassifyAlerts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		knownIssuesYAML string
		alerts          []alert
	}{
		{
			name: "basic",
			knownIssuesYAML: `knownIssues:
- name: "BackendOperationErrorRate"
  reason: "error rate known"
- name: "BackendController.*"
  reason: "controller churn"
`,
			alerts: []alert{
				{Alert: alertData{Name: "BackendOperationErrorRate"}},
				{Alert: alertData{Name: "BackendControllerRetryHotLoop"}},
				{Alert: alertData{Name: "BackendControllerQueueDepthHigh"}},
				{Alert: alertData{Name: "SomethingUnknown"}},
				{Alert: alertData{Name: "AnotherUnknown"}},
			},
		},
		{
			name: "no_known_issues",
			alerts: []alert{
				{Alert: alertData{Name: "SomeAlert"}},
			},
		},
		{
			name: "exact_match_only",
			knownIssuesYAML: `knownIssues:
- name: "Backend"
  reason: "exact match only"
`,
			alerts: []alert{
				{Alert: alertData{Name: "Backend"}},
				{Alert: alertData{Name: "BackendOperationErrorRate"}},
			},
		},
		{
			name: "first_match_wins",
			knownIssuesYAML: `knownIssues:
- name: "Backend.*"
  reason: "first pattern"
- name: "BackendControllerRetryHotLoop"
  reason: "second pattern"
`,
			alerts: []alert{
				{Alert: alertData{Name: "BackendControllerRetryHotLoop"}},
			},
		},
		{
			name: "label_matching",
			knownIssuesYAML: `knownIssues:
- name: "BackendControllerRetryHotLoop"
  reason: "known for delete controller"
  labels:
    name: "operationnodepooldelete"
`,
			alerts: []alert{
				{Alert: alertData{
					Name:   "BackendControllerRetryHotLoop",
					Labels: map[string]string{"name": "operationnodepooldelete", "severity": "warning"},
				}},
				{Alert: alertData{
					Name:   "BackendControllerRetryHotLoop",
					Labels: map[string]string{"name": "operationcreate", "severity": "warning"},
				}},
				{Alert: alertData{
					Name:   "BackendControllerRetryHotLoop",
					Labels: nil,
				}},
			},
		},
		{
			name: "label_regex",
			knownIssuesYAML: `knownIssues:
- name: "BackendControllerRetryHotLoop"
  reason: "known for delete controllers"
  labels:
    name: "operation.*delete"
`,
			alerts: []alert{
				{Alert: alertData{
					Name:   "BackendControllerRetryHotLoop",
					Labels: map[string]string{"name": "operationnodepooldelete"},
				}},
				{Alert: alertData{
					Name:   "BackendControllerRetryHotLoop",
					Labels: map[string]string{"name": "operationclusterdelete"},
				}},
				{Alert: alertData{
					Name:   "BackendControllerRetryHotLoop",
					Labels: map[string]string{"name": "operationcreate"},
				}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var issues []knownIssue
			if tt.knownIssuesYAML != "" {
				issues = mustParse(t, tt.knownIssuesYAML)
			}
			classified := classifyAlerts(tt.alerts, issues)
			CompareWithFixture(t, classified)
		})
	}
}

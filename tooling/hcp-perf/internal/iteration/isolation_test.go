// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package iteration

import "testing"

func TestStartupPlanRejectsFalseSizingIsolation(t *testing.T) {
	for _, mode := range []string{"selector", "renamed CSC", "renamed observer CSC"} {
		t.Run(mode, func(t *testing.T) {
			spec := startupIterationSpec()
			switch mode {
			case "selector":
				spec.Actions[1].Candidate.EmptyHostedClusterSelector = "api.openshift.com/name=medium"
			case "renamed CSC":
				spec.Actions[1].Candidate.Name = "medium"
				spec.Actions[2].Startup.CandidateName = "medium"
			case "renamed observer CSC":
				spec.Actions[2].Startup.CandidateName = "medium"
			}
			if err := spec.Validate(); err == nil {
				t.Fatal("unsupported sizing isolation passed submission validation")
			}
		})
	}
}

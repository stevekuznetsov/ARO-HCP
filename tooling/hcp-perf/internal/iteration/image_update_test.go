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

import (
	"encoding/json"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/startup"
	"testing"
	"time"
)

func TestExistingObservationRequiresPinnedIdentity(t *testing.T) {
	base := startupIterationSpec().Actions[2]
	base.Kind = StartupObserveExisting
	base.DependsOn = nil
	base.ObservationIdentity = &startup.Identity{ResourceID: base.Startup.ResourceID, HCUID: "hc", HCPUID: "hcp", NamespaceUID: "ns"}
	if err := base.validate(); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"absent", "replacement-resource", "missing-hcp"} {
		t.Run(scenario, func(t *testing.T) {
			a := base
			copy := *base.ObservationIdentity
			a.ObservationIdentity = &copy
			switch scenario {
			case "absent":
				a.ObservationIdentity = nil
			case "replacement-resource":
				copy.ResourceID += "-other"
			case "missing-hcp":
				copy.HCPUID = ""
			}
			if a.validate() == nil {
				t.Fatal("unpinned observation accepted")
			}
		})
	}
	data, _ := json.Marshal(base)
	var decoded Action
	if json.Unmarshal(data, &decoded) != nil || decoded.Timeout < time.Second || decoded.ObservationIdentity.HCPUID != "hcp" {
		t.Fatal("typed observation lost identity")
	}
}

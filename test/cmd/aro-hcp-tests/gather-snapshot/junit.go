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

package gathersnapshot

import (
	"fmt"

	"github.com/Azure/ARO-HCP/test/util/junit"
	"github.com/Azure/ARO-HCP/tooling/hcpctl/pkg/snapshot"
)

// reportsToJUnit converts a slice of VerificationReports into a jUnit TestSuites
// structure suitable for CI consumption.
func reportsToJUnit(reports []*snapshot.VerificationReport) *junit.TestSuites {
	testSuite := junit.TestSuite{
		Name: "aro-hcp-snapshot",
	}

	for _, report := range reports {
		for _, c := range report.Cases {
			tc := &junit.TestCase{
				Name:      fmt.Sprintf("[aro-hcp-snapshot] [%s] %s returns results", c.Suite, c.Query),
				Classname: c.Category,
			}

			switch c.Status {
			case snapshot.VerificationFail:
				tc.FailureOutput = &junit.FailureOutput{
					Message: "query returned no results",
					Output:  c.Message,
				}
				testSuite.NumFailed++
			case snapshot.VerificationSkipped:
				tc.SkipMessage = &junit.SkipMessage{
					Message: c.Message,
				}
				testSuite.NumSkipped++
			}

			testSuite.TestCases = append(testSuite.TestCases, tc)
			testSuite.NumTests++
		}
	}

	return &junit.TestSuites{
		Suites: []*junit.TestSuite{&testSuite},
	}
}

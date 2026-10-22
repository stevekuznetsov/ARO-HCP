// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package performance

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLegacySemanticValidationDoesNotAdmitRun(t *testing.T) {
	s := testSpec()
	s.ExpectedGuestNodes = nil
	if err := s.Validate(); err != nil {
		t.Fatal("historical three-node spec rejected", err)
	}
	if s.ValidateRun() == nil {
		t.Fatal("legacy spec admitted as new measurement")
	}
	dir := filepath.Join(t.TempDir(), "must-not-exist")
	if _, err := Run(t.Context(), s, dir, nil); err == nil {
		t.Fatal("direct Run bypassed admission")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("legacy Run touched artifacts or clients")
	}
	s.Seed.GuestNodes = 6
	if s.Validate() == nil {
		t.Fatal("legacy exception broadened beyond exact three")
	}
	s.Seed.GuestNodes = 3
	s.ExpectedGuestNodes = []GuestNodeIdentity{}
	if s.Validate() == nil {
		t.Fatal("explicit empty inventory silently treated as legacy")
	}
}

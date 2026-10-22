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
	"reflect"
	"strings"
	"testing"
)

func TestObservationImageRevisionPreservesResources(t *testing.T) {
	spec := startupIterationSpec().Actions[2].Startup
	if spec == nil {
		t.Fatal("missing startup fixture")
	}
	old := "registry.example/old@sha256:" + strings.Repeat("a", 64)
	next := "registry.example/new@sha256:" + strings.Repeat("b", 64)
	spec.Components[0].Containers[0].Image = old
	binding := ObservationImageBinding{Kind: spec.Components[0].Kind, Component: spec.Components[0].Name, Container: spec.Components[0].Containers[0].Name, PreviousImage: old, Image: next}
	revised, err := revisedObservation(*spec, []ObservationImageBinding{binding})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Components[0].Containers[0].Image != old {
		t.Fatal("mutated frozen original")
	}
	revised.Components[0].Containers[0].Image = old
	if !reflect.DeepEqual(revised, *spec) {
		t.Fatal("changed fields beyond image")
	}
	for _, bad := range []ObservationImageBinding{{Kind: "Deployment", Component: "missing", Container: "missing", PreviousImage: old, Image: next}, {Kind: binding.Kind, Component: binding.Component, Container: binding.Container, PreviousImage: next, Image: old}} {
		if _, err := revisedObservation(*spec, []ObservationImageBinding{bad}); err == nil {
			t.Fatal("accepted invalid binding")
		}
	}
	if _, err := revisedObservation(*spec, []ObservationImageBinding{binding, binding}); err == nil {
		t.Fatal("accepted duplicate")
	}
	binding.Image = ""
	if _, err := revisedObservation(*spec, []ObservationImageBinding{binding}); err == nil {
		t.Fatal("accepted removal of image pin")
	}
}

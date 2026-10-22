// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package startup

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"
)

// DecodeCandidateSpec preserves exact int64 values used by Kubernetes
// unstructured objects. CSC numeric fields are integers: fractions and overflow
// are rejected, never rounded through float64 before validation or comparison.
func DecodeCandidateSpec(data []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		return nil, err
	}
	if len(object) == 0 {
		return nil, errors.New("candidate spec must be a nonempty JSON object")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("candidate spec must contain exactly one JSON object")
	}
	var normalize func(any) (any, error)
	normalize = func(value any) (any, error) {
		switch typed := value.(type) {
		case json.Number:
			number, err := strconv.ParseInt(string(typed), 10, 64)
			if err != nil {
				return nil, errors.New("candidate numbers must be exact int64 integers")
			}
			return number, nil
		case map[string]any:
			for key, child := range typed {
				normalized, err := normalize(child)
				if err != nil {
					return nil, err
				}
				typed[key] = normalized
			}
		case []any:
			for i, child := range typed {
				normalized, err := normalize(child)
				if err != nil {
					return nil, err
				}
				typed[i] = normalized
			}
		}
		return value, nil
	}
	_, err := normalize(object)
	return object, err
}

func candidateSpecEqual(actual any, expected map[string]any) bool {
	// Encoding normalizes integer-valued float64 from JSON-only test/adapter
	// inputs; decoding retains exact int64 from the real Kubernetes client.
	data, err := json.Marshal(actual)
	if err != nil {
		return false
	}
	normalized, err := DecodeCandidateSpec(data)
	return err == nil && reflect.DeepEqual(normalized, expected)
}

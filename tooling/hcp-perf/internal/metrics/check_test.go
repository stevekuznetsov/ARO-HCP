// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package metrics

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRangeSourceHistory(t *testing.T) {
	start := time.Unix(1700000000, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query_range" || r.URL.Query().Get("step") != "5" || r.URL.Query().Get("query") != "timestamp(up)" {
			t.Error("incorrect history request")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": []any{map[string]any{"metric": map[string]string{"job": "scheduler", "instance": "one"}, "values": []any{[]any{start.Unix(), "1699999995"}, []any{start.Add(5 * time.Second).Unix(), "1700000000"}}}}}})
	}))
	defer server.Close()
	points, err := Range(t.Context(), server.Client(), server.URL, "timestamp(up)", start, start.Add(5*time.Second), 5*time.Second)
	if err != nil || len(points) != 2 || points[1].Value != 1700000000 || !points[1].At.Equal(start.Add(5*time.Second)) {
		t.Fatalf("source versus evaluation time lost: %+v %v", points, err)
	}
	if _, err := Range(t.Context(), server.Client(), server.URL, "up", start, start.Add(time.Hour), 5*time.Second); err == nil {
		t.Fatal("unbounded history accepted")
	}
}

func TestInstantCancellationAndInvalidEvidence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := Instant(ctx, server.Client(), server.URL, "up"); err == nil {
		t.Fatal("ignored timeout")
	}
	for _, body := range []string{
		`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"NaN"]}]}}`,
		`{"status":"success","warnings":["partial"],"data":{"resultType":"vector","result":[]}}`,
		`{"status":"success","data":{"resultType":"scalar","result":[1,"1"]}}`,
	} {
		t.Run(body, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer s.Close()
			if _, err := Instant(t.Context(), s.Client(), s.URL, "up"); err == nil {
				t.Fatal("accepted invalid evidence")
			}
		})
	}
}

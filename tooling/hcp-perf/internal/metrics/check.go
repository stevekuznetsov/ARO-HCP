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
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ValidateQueries checks export names/expressions without I/O.
func ValidateQueries(queries []Query) error {
	at := time.Unix(1, 0)
	return validate(queries, at, at, time.Second)
}

type Point struct {
	Labels map[string]string `json:"labels"`
	At     time.Time         `json:"at"`
	Value  float64           `json:"value"`
}

// Instant performs a bounded read-only query. Server messages and transport
// errors are not exported because authentication may be supplied by transport.
func Instant(ctx context.Context, client *http.Client, endpoint, expression string) ([]Point, error) {
	return queryPoints(ctx, client, endpoint, "/api/v1/query", url.Values{"query": {expression}}, "vector")
}

// Range reads a short bounded history in memory. Point.At is evaluation time,
// not source time; timestamp(metric) returns the source time in Point.Value.
// The same request/body limits and sanitized errors as Instant apply.
func Range(ctx context.Context, client *http.Client, endpoint, expression string, start, end time.Time, step time.Duration) ([]Point, error) {
	if start.IsZero() || end.Before(start) || end.Sub(start) > 20*time.Minute || step < 5*time.Second || step%time.Second != 0 {
		return nil, errors.New("range history requires at most 20m and whole-second step >=5s")
	}
	return queryPoints(ctx, client, endpoint, "/api/v1/query_range", url.Values{
		"query": {expression}, "start": {start.Format(time.RFC3339Nano)}, "end": {end.Format(time.RFC3339Nano)}, "step": {strconv.FormatFloat(step.Seconds(), 'f', -1, 64)},
	}, "matrix")
}

func queryPoints(ctx context.Context, client *http.Client, endpoint, path string, parameters url.Values, resultType string) ([]Point, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || parameters.Get("query") == "" {
		return nil, errors.New("invalid instant query inputs")
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawQuery = parameters.Encode()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.New("construct instant query failed")
	}
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, errors.New("instant query failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("instant query HTTP failure")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil || len(data) > 4<<20 {
		return nil, errors.New("instant query response incomplete")
	}
	var body struct {
		Status   string   `json:"status"`
		Warnings []string `json:"warnings"`
		Data     struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string   `json:"metric"`
				Value  []json.RawMessage   `json:"value"`
				Values [][]json.RawMessage `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &body) != nil || body.Status != "success" || body.Data.ResultType != resultType || len(body.Warnings) != 0 {
		return nil, errors.New("query lacks complete typed evidence")
	}
	points := make([]Point, 0, len(body.Data.Result))
	for _, result := range body.Data.Result {
		values := result.Values
		if resultType == "vector" {
			values = [][]json.RawMessage{result.Value}
		}
		for _, pair := range values {
			var stamp float64
			var value string
			if len(pair) != 2 || json.Unmarshal(pair[0], &stamp) != nil || json.Unmarshal(pair[1], &value) != nil {
				return nil, errors.New("invalid instant sample")
			}
			number, err := strconv.ParseFloat(value, 64)
			if err != nil || math.IsNaN(number) || math.IsInf(number, 0) || math.IsNaN(stamp) || math.IsInf(stamp, 0) {
				return nil, errors.New("nonfinite instant sample")
			}
			points = append(points, Point{Labels: result.Metric, At: time.UnixMilli(int64(stamp * 1000)), Value: number})
		}
	}
	return points, nil
}

// Copyright 2026 Microsoft Corporation
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

package metrics

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"time"
)

type Analysis struct {
	Manifest Manifest       `json:"manifest"`
	Queries  []QuerySummary `json:"queries"`
}

type QuerySummary struct {
	Query
	Status   string          `json:"status"`
	Error    string          `json:"error,omitempty"`
	Warnings []string        `json:"warnings,omitempty"`
	Series   []SeriesSummary `json:"series"`
}

// SeriesSummary preserves every label set separately. Nil extrema/timestamps
// mean no usable samples, not zero. Coverage is distinct valid grid positions /
// expected inclusive evaluation count, not a claim of continuous availability.
// Samples must follow the millisecond evaluation grid in increasing order.
// MissingApprox assumes one float sample per step; unseen series are unknowable.
type SeriesSummary struct {
	Labels         map[string]string `json:"labels"`
	Samples        int               `json:"samples"`
	ValidSamples   int               `json:"validSamples"`
	InvalidSamples int               `json:"invalidSamples"`
	Min            *float64          `json:"min"`
	Max            *float64          `json:"max"`
	FirstTimestamp *float64          `json:"firstTimestamp"`
	LastTimestamp  *float64          `json:"lastTimestamp"`
	Expected       int64             `json:"expected"`
	MissingApprox  int64             `json:"missingApprox"`
	Coverage       float64           `json:"coverage"`
}

// Analyze verifies filenames and SHA-256 evidence hashes before decoding any
// response. It returns partial summaries with errors for corrupt/failed captures.
// Hashes detect accidental modification, not authenticity of an untrusted manifest.
func Analyze(dir string) (Analysis, error) {
	var a Analysis
	root, err := os.OpenRoot(dir)
	if err != nil {
		return a, err
	}
	defer root.Close()
	body, err := readRegular(root, "manifest.json")
	if err != nil {
		return a, err
	}
	if err := json.Unmarshal(body, &a.Manifest); err != nil {
		return a, fmt.Errorf("invalid manifest: %w", err)
	}
	m := a.Manifest
	if m.Version != ManifestVersion {
		return a, fmt.Errorf("unsupported manifest version %d", m.Version)
	}
	queries := make([]Query, len(m.Queries))
	for i, r := range m.Queries {
		queries[i] = r.Query
	}
	if err := validate(queries, m.Start, m.End, m.Step); err != nil {
		return a, fmt.Errorf("invalid manifest: %w", err)
	}
	var errs []error
	for _, r := range m.Queries {
		s := QuerySummary{Query: r.Query, Status: StatusError, Warnings: r.Warnings}
		var queryErr error
		switch {
		case r.File == "":
			queryErr = errors.New("no captured response")
		case r.File != r.Name+".json":
			queryErr = errors.New("unsafe or unexpected response filename")
		default:
			want, hashErr := hex.DecodeString(r.SHA256)
			if hashErr != nil || len(want) != sha256.Size {
				queryErr = errors.New("invalid response hash")
				break
			}
			raw, readErr := readRegular(root, r.File)
			if readErr != nil {
				queryErr = readErr
				break
			}
			hash := sha256.Sum256(raw)
			if hex.EncodeToString(hash[:]) != r.SHA256 {
				queryErr = errors.New("response hash mismatch")
				break
			}
			s, queryErr = summarize(raw, m)
			s.Query = r.Query
		}
		if r.Status != StatusSuccess && r.Status != StatusIncomplete {
			queryErr = errors.Join(queryErr, fmt.Errorf("capture status %q: %s", r.Status, r.Error))
		}
		if r.HTTPStatus < 200 || r.HTTPStatus >= 300 {
			queryErr = errors.Join(queryErr, fmt.Errorf("HTTP status %d", r.HTTPStatus))
		}
		if r.Error != "" && (r.Status == StatusSuccess || r.Status == StatusIncomplete) {
			queryErr = errors.Join(queryErr, errors.New(r.Error))
		}
		if queryErr != nil {
			s.Status, s.Error = StatusError, queryErr.Error()
			errs = append(errs, fmt.Errorf("query %s: %w", r.Name, queryErr))
		}
		a.Queries = append(a.Queries, s)
	}
	return a, errors.Join(errs...)
}

func readRegular(root *os.Root, name string) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", name)
	}
	// Root also prevents a concurrent symlink replacement from escaping dir.
	return root.ReadFile(name)
}

func summarize(raw []byte, m Manifest) (QuerySummary, error) {
	s := QuerySummary{Status: StatusSuccess}
	var response struct {
		Status   string   `json:"status"`
		Error    string   `json:"error"`
		Warnings []string `json:"warnings"`
		Infos    []string `json:"infos"`
		Data     *struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric     map[string]string   `json:"metric"`
				Values     [][]json.RawMessage `json:"values"`
				Histograms json.RawMessage     `json:"histograms"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return s, fmt.Errorf("malformed Prometheus response: %w", err)
	}
	s.Warnings = append(response.Warnings, response.Infos...)
	if response.Status != "success" || response.Error != "" {
		return s, fmt.Errorf("Prometheus status %q: %s", response.Status, response.Error)
	}
	if response.Data == nil || response.Data.ResultType != "matrix" || len(response.Data.Result) == 0 {
		return s, errors.New("missing or empty matrix data")
	}
	// Prometheus evaluates at millisecond precision. Validation bounds the window
	// to a Duration and the step to whole milliseconds, so this arithmetic fits.
	// Never materialize the grid: even a sparse capture may span billions of steps.
	startMillis, endMillis := m.Start.UnixMilli(), m.End.UnixMilli()
	stepMillis := int64(m.Step / time.Millisecond)
	expected := (endMillis-startMillis)/stepMillis + 1
	var errs []error
	for i, series := range response.Data.Result {
		ss := SeriesSummary{Labels: series.Metric, Samples: len(series.Values), Expected: expected}
		if series.Metric == nil || len(series.Values) == 0 || len(series.Histograms) != 0 {
			errs = append(errs, fmt.Errorf("series %d: missing labels/samples or unsupported histograms", i))
		}
		previousPosition := int64(-1)
		for j, pair := range series.Values {
			var timestamp float64
			var text string
			valid := len(pair) == 2
			if valid {
				valid = string(pair[0]) != "null" && json.Unmarshal(pair[0], &timestamp) == nil && json.Unmarshal(pair[1], &text) == nil
			}
			value, valueErr := strconv.ParseFloat(text, 64)
			if !valid || valueErr != nil || math.IsNaN(value) || math.IsInf(value, 0) || math.IsNaN(timestamp) || math.IsInf(timestamp, 0) {
				ss.InvalidSamples++
				errs = append(errs, fmt.Errorf("series %d sample %d: invalid/nonfinite value or timestamp", i, j))
				continue
			}
			position := math.Round((timestamp*1000 - float64(startMillis)) / float64(stepMillis))
			if position < 0 || position >= float64(expected) {
				ss.InvalidSamples++
				errs = append(errs, fmt.Errorf("series %d sample %d: timestamp outside evaluation grid", i, j))
				continue
			}
			index := int64(position)
			gridTimestamp := float64(startMillis+index*stepMillis) / 1000
			// Allow one millisecond plus float conversion roundoff, including at
			// the range boundaries. Increasing positions prevent tolerance from
			// letting multiple timestamps count toward the same evaluation.
			tolerance := 0.001 + math.Abs(math.Nextafter(gridTimestamp, math.Inf(1))-gridTimestamp)
			if math.Abs(timestamp-gridTimestamp) > tolerance || index <= previousPosition {
				ss.InvalidSamples++
				errs = append(errs, fmt.Errorf("series %d sample %d: off-grid, duplicate, or unordered timestamp", i, j))
				continue
			}
			previousPosition = index
			ss.ValidSamples++
			if ss.Min == nil || value < *ss.Min {
				v := value
				ss.Min = &v
			}
			if ss.Max == nil || value > *ss.Max {
				v := value
				ss.Max = &v
			}
			if ss.FirstTimestamp == nil {
				v := timestamp
				ss.FirstTimestamp = &v
			}
			v := timestamp
			ss.LastTimestamp = &v
		}
		ss.MissingApprox = max(0, expected-int64(ss.ValidSamples))
		ss.Coverage = math.Min(1, float64(ss.ValidSamples)/float64(expected))
		if ss.MissingApprox > 0 {
			s.Warnings = append(s.Warnings, fmt.Sprintf("series %d: approximately %d of %d expected samples missing", i, ss.MissingApprox, expected))
		}
		s.Series = append(s.Series, ss)
	}
	if len(s.Warnings) > 0 {
		s.Status = StatusIncomplete
	}
	if len(errs) > 0 {
		s.Status = StatusError
	}
	return s, errors.Join(errs...)
}

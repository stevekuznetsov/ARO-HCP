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

// Package report composes local evidence into an offline, non-qualifying report.
// It never modifies captures or reads recordings, archives, or credentials.
package report

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/assessment"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/metrics"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/progress"
)

const (
	MaxPoints        = 500
	DefaultMaxPoints = 300
	DefaultMaxSeries = 200
	MaxTableSeries   = 1000
	maxQuerySeries   = 20
	maxWarnings      = 20
)

type Limits struct {
	MaxSeries int `json:"maxSeries"`
	MaxPoints int `json:"maxPoints"`
}

func (l Limits) Validate() error {
	if l.MaxSeries < 1 || l.MaxSeries > MaxTableSeries || l.MaxPoints < 2 || l.MaxPoints > MaxPoints {
		return fmt.Errorf("max-series must be 1..%d; max-points must be 2..%d", MaxTableSeries, MaxPoints)
	}
	return nil
}

// Totals describe every analyzed series, including those omitted from previews.
// Coverage is valid / expected grid positions, not an availability measurement.
// An unreadable/invalid source may have no analyzed series; that is not evidence
// that the original source contained zero series.
type Totals struct {
	Series           int      `json:"series"`
	Samples          int64    `json:"samples"`
	ValidSamples     int64    `json:"validSamples"`
	InvalidSamples   int64    `json:"invalidSamples"`
	Expected         int64    `json:"expected"`
	MissingApprox    int64    `json:"missingApprox"`
	IncompleteSeries int      `json:"incompleteSeries"`
	Coverage         *float64 `json:"coverage"`
}

type ResultPage struct {
	Version       string             `json:"version"`
	Assessment    *assessment.Result `json:"assessment,omitempty"`
	Progress      progress.Result    `json:"progress"`
	Sources       []MetricSource     `json:"sources"`
	Limits        Limits             `json:"limits"`
	TotalSeries   int                `json:"totalSeries"`
	PreviewSeries int                `json:"previewSeries"`
	OmittedSeries int                `json:"omittedSeries"`
	PlottedPoints int                `json:"plottedPoints"`
}

type MetricSource struct {
	Directory               string           `json:"directory"`
	Manifest                metrics.Manifest `json:"manifest"`
	Error                   string           `json:"error,omitempty"`
	Queries                 []Query          `json:"queries"`
	Limits                  Limits           `json:"limits"`
	ManifestWarningsOmitted int              `json:"manifestWarningsOmitted"`
}

type Query struct {
	Name            string   `json:"name"`
	Expr            string   `json:"expr"`
	Status          string   `json:"status"`
	Error           string   `json:"error,omitempty"`
	Warnings        []string `json:"warnings"`
	Series          []Series `json:"series"`
	Totals          Totals   `json:"totals"`
	OmittedSeries   int      `json:"omittedSeries"`
	OmittedWarnings int      `json:"omittedWarnings"`
	priority        int
	captureIndex    int
}

type Series struct {
	metrics.SeriesSummary
	LabelText   string  `json:"-"`
	Points      []Point `json:"points"`
	Gaps        int     `json:"gaps"`
	ZeroSamples int     `json:"zeroSamples"`
	sourceIndex int
	important   bool
}

// Segment is assigned before downsampling, so omitted samples or entire omitted
// segments cannot accidentally bridge a real gap. Timestamps are Unix seconds.
type Point struct {
	Time    float64 `json:"t"`
	Value   float64 `json:"v"`
	Segment int     `json:"segment"`
}

// Compact numeric tuples avoid repeating three property names for every point.
// The public Go fields remain available to callers; only preview points are embedded.
func (p Point) MarshalJSON() ([]byte, error) {
	return json.Marshal([3]float64{p.Time, p.Value, float64(p.Segment)})
}

func (p *Point) UnmarshalJSON(b []byte) error {
	var tuple [3]float64
	if err := json.Unmarshal(b, &tuple); err != nil {
		return err
	}
	p.Time, p.Value, p.Segment = tuple[0], tuple[1], int(tuple[2])
	return nil
}

// Compose takes ownership of sources; it sorts query and label sets for stable
// exploration without combining separate export windows or process identities.
func Compose(p progress.Result, sources []MetricSource) ResultPage {
	page := ResultPage{Version: "v2", Progress: p, Sources: sources, Limits: Limits{DefaultMaxSeries, DefaultMaxPoints}}
	for i := range sources {
		page.Limits = sources[i].Limits
		sort.SliceStable(sources[i].Queries, func(a, b int) bool {
			return sources[i].Queries[a].Name < sources[i].Queries[b].Name
		})
		for j := range sources[i].Queries {
			q := &sources[i].Queries[j]
			page.TotalSeries += q.Totals.Series
			page.PreviewSeries += len(q.Series)
			page.OmittedSeries += q.OmittedSeries
			series := q.Series
			for _, s := range series {
				page.PlottedPoints += len(s.Points)
			}
			sort.SliceStable(series, func(a, b int) bool { return series[a].LabelText < series[b].LabelText })
		}
	}
	return page
}

// LoadMetrics inspects exactly the supplied export roots, not their descendants.
// Source errors are report data, not fatal errors. Only cancellation aborts loading.
// Analyze validates names, hashes, the evaluation grid and response structure.
func LoadMetrics(ctx context.Context, dirs []string) ([]MetricSource, error) {
	return LoadMetricsWithLimits(ctx, dirs, Limits{DefaultMaxSeries, DefaultMaxPoints})
}

var upQuery = regexp.MustCompile(`(?i)(^|[-_])up($|[-_])`)
var resourceQuery = regexp.MustCompile(`(?i)cpu|memory|working.set|resident|gc|heap`)
var controlPlane = regexp.MustCompile(`(?i)(^|[^a-z])(kas|etcd|cpo)([^a-z]|$)|kube.apiserver|control.plane.operator`)

// LoadMetricsWithLimits bounds candidates before loading chart samples. Captures
// are analyzed serially, and only selected series are decoded for plotting.
func LoadMetricsWithLimits(ctx context.Context, dirs []string, limits Limits) ([]MetricSource, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	sources := make([]MetricSource, 0, len(dirs))
	for _, dir := range dirs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		a, err := metrics.Analyze(dir)
		source := MetricSource{Directory: dir, Manifest: a.Manifest, Queries: []Query{}, Limits: limits}
		source.Manifest.Queries = append([]metrics.QueryResult(nil), a.Manifest.Queries...)
		if err != nil {
			source.Error = previewText(err.Error())
		}
		for i := range source.Manifest.Queries {
			capture := &source.Manifest.Queries[i]
			source.ManifestWarningsOmitted += max(0, len(capture.Warnings)-maxWarnings)
			capture.Warnings = append([]string(nil), capture.Warnings[:min(len(capture.Warnings), maxWarnings)]...)
			for j := range capture.Warnings {
				capture.Warnings[j] = previewText(capture.Warnings[j])
			}
			capture.Error = previewText(capture.Error)
		}
		for i, summary := range a.Queries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			q := Query{Name: summary.Name, Expr: summary.Expr, Status: summary.Status,
				Error: previewText(summary.Error), Series: []Series{}, captureIndex: i, priority: 3}
			capture := a.Manifest.Queries[i]
			seenWarnings := map[string]bool{}
			for _, warnings := range [][]string{summary.Warnings, capture.Warnings} {
				for _, warning := range warnings {
					if seenWarnings[warning] {
						continue
					}
					seenWarnings[warning] = true
					if len(q.Warnings) < maxWarnings {
						q.Warnings = append(q.Warnings, previewText(warning))
					} else {
						q.OmittedWarnings++
					}
				}
			}
			cap := min(maxQuerySeries, limits.MaxSeries)
			resource := resourceQuery.MatchString(q.Name)
			if upQuery.MatchString(q.Name) {
				q.priority, cap = 0, limits.MaxSeries
			} else if resource {
				q.priority = 1
			} else if controlPlane.MatchString(q.Name) {
				q.priority = 2
			}
			candidates := make([]Series, 0, len(summary.Series))
			for index, ss := range summary.Series {
				q.Totals.Series++
				q.Totals.Samples += int64(ss.Samples)
				q.Totals.ValidSamples += int64(ss.ValidSamples)
				q.Totals.InvalidSamples += int64(ss.InvalidSamples)
				q.Totals.Expected += ss.Expected
				q.Totals.MissingApprox += ss.MissingApprox
				if ss.MissingApprox > 0 || ss.InvalidSamples > 0 {
					q.Totals.IncompleteSeries++
				}
				keys := make([]string, 0, len(ss.Labels))
				for key := range ss.Labels {
					keys = append(keys, key)
				}
				sort.Strings(keys)
				labels := make([]string, 0, len(keys))
				for _, key := range keys {
					labels = append(labels, key+"="+strconv.Quote(ss.Labels[key]))
				}
				candidates = append(candidates, Series{SeriesSummary: ss, LabelText: strings.Join(labels, ", "), sourceIndex: index, important: importantSeries(ss.Labels)})
			}
			if q.Totals.Expected > 0 {
				coverage := float64(q.Totals.ValidSamples) / float64(q.Totals.Expected)
				q.Totals.Coverage = &coverage
			}
			sort.SliceStable(candidates, func(i, j int) bool {
				a, b := candidates[i], candidates[j]
				if a.important != b.important {
					return a.important
				}
				if (a.Labels["le"] == "") != (b.Labels["le"] == "") {
					return a.Labels["le"] == ""
				}
				// Bucket magnitudes are not comparable latencies. Keep their stable
				// label order rather than suggesting that large buckets are important.
				if resource && a.Labels["le"] == "" && b.Labels["le"] == "" {
					if av, bv := peak(a), peak(b); av != bv {
						return av > bv
					}
				}
				return a.LabelText < b.LabelText
			})
			q.Series = append(q.Series, candidates[:min(cap, len(candidates))]...)
			source.Queries = append(source.Queries, q)
		}
		sources = append(sources, source)
	}
	var queries []*Query
	for i := range sources {
		for j := range sources[i].Queries {
			queries = append(queries, &sources[i].Queries[j])
		}
	}
	sort.SliceStable(queries, func(i, j int) bool {
		if queries[i].priority != queries[j].priority {
			return queries[i].priority < queries[j].priority
		}
		return queries[i].Name < queries[j].Name
	})
	remaining := limits.MaxSeries
	selected := make(map[*Query]int, len(queries))
	// Reserve scrape-up targets first, round-robin if multiple roots/queries
	// compete. Other queries then get one slot each per round, not a greedy fill.
	for _, up := range []bool{true, false} {
		for remaining > 0 {
			added := false
			for _, q := range queries {
				if (q.priority == 0) != up || selected[q] == len(q.Series) {
					continue
				}
				selected[q]++
				remaining--
				added = true
				if remaining == 0 {
					break
				}
			}
			if !added {
				break
			}
		}
	}
	for i := range sources {
		source := &sources[i]
		for j := range source.Queries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			q := &source.Queries[j]
			q.Series = append([]Series{}, q.Series[:selected[q]]...)
			q.OmittedSeries = q.Totals.Series - len(q.Series)
			if len(q.Series) == 0 || q.Status == metrics.StatusError {
				continue
			}
			capture := source.Manifest.Queries[q.captureIndex]
			if err := loadPoints(ctx, source.Directory, capture, source.Manifest.Step.Seconds(), q.Series, q.Totals.Series, limits.MaxPoints); err != nil {
				q.Status, q.Error = metrics.StatusError, previewText(err.Error())
				for k := range q.Series {
					q.Series[k].Points = nil
				}
			}
		}
	}
	return sources, ctx.Err()
}

func importantSeries(labels map[string]string) bool {
	for _, key := range []string{"name", "namespace", "pod", "container", "process", "job", "__name__"} {
		if controlPlane.MatchString(labels[key]) {
			return true
		}
	}
	return false
}

func peak(s Series) float64 {
	v := 0.0
	for _, p := range []*float64{s.Min, s.Max} {
		if p != nil {
			v = math.Max(v, math.Abs(*p))
		}
	}
	return v
}

func previewText(s string) string {
	const limit = 2048
	if len(s) > limit {
		return s[:limit] + " [truncated in preview; inspect full local evidence]"
	}
	return s
}

func loadPoints(ctx context.Context, dir string, capture metrics.QueryResult, step float64, series []Series, totalSeries, maxPoints int) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	info, err := root.Lstat(capture.File)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("response is no longer a regular file")
	}
	raw, err := root.ReadFile(capture.File)
	if err != nil {
		return err
	}
	// Recheck the bytes actually plotted against Analyze's manifest snapshot. A
	// writer may have replaced a file after analysis; never plot that replacement.
	hash := sha256.Sum256(raw)
	if hex.EncodeToString(hash[:]) != capture.SHA256 {
		return fmt.Errorf("response hash mismatch after analysis; chart withheld")
	}
	var response struct {
		Data struct {
			Result []json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return fmt.Errorf("cannot decode validated response")
	}
	if len(response.Data.Result) != totalSeries {
		return fmt.Errorf("series count changed after analysis")
	}
	for i := range series {
		if err := ctx.Err(); err != nil {
			return err
		}
		var row struct {
			Values [][2]json.RawMessage `json:"values"`
		}
		if err := json.Unmarshal(response.Data.Result[series[i].sourceIndex], &row); err != nil {
			return fmt.Errorf("cannot decode validated series")
		}
		points := make([]Point, 0, len(row.Values))
		segment := 0
		for _, pair := range row.Values {
			var timestamp float64
			var text string
			// Analyze already validated these exact bytes, including finite values.
			if json.Unmarshal(pair[0], &timestamp) != nil || json.Unmarshal(pair[1], &text) != nil {
				return fmt.Errorf("cannot decode validated sample")
			}
			value, err := strconv.ParseFloat(text, 64)
			if err != nil {
				return fmt.Errorf("cannot decode validated value")
			}
			if len(points) > 0 && timestamp-points[len(points)-1].Time > step+0.003 {
				segment++
			}
			if value == 0 {
				series[i].ZeroSamples++
			}
			points = append(points, Point{Time: timestamp, Value: value, Segment: segment})
		}
		series[i].Gaps = segment
		series[i].Points = downsample(points, maxPoints)
	}
	return nil
}

// Equal-count buckets retain both extrema in original timestamp order. This is
// visualization only: summaries and the immutable source retain all samples.
func downsample(points []Point, limit int) []Point {
	if len(points) <= limit {
		return points
	}
	result := make([]Point, 0, limit)
	buckets := limit / 2
	for b := 0; b < buckets; b++ {
		start, end := b*len(points)/buckets, (b+1)*len(points)/buckets
		lo, hi := start, start
		for i := start + 1; i < end; i++ {
			if points[i].Value < points[lo].Value {
				lo = i
			}
			if points[i].Value >= points[hi].Value {
				hi = i
			}
		}
		if lo > hi {
			lo, hi = hi, lo
		}
		result = append(result, points[lo])
		if hi != lo {
			result = append(result, points[hi])
		}
	}
	return result
}

// Render writes no untrusted markup. encoding/json's default HTML escaping is
// essential: a label containing </script> must remain inside the data element.
// Dynamic browser content is created with textContent, never innerHTML.
func Render(w io.Writer, page ResultPage) error {
	data, err := json.Marshal(page)
	if err != nil {
		return err
	}
	for _, part := range []string{htmlHead, string(data), htmlTail} {
		if _, err := io.WriteString(w, part); err != nil {
			return err
		}
	}
	return nil
}

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

package assessment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/metrics"
)

func (b *builder) metrics(dir string) {
	if b.ctx.Err() != nil {
		return
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		b.result.Status.Capture = "incomplete"
		b.finding("capture", "inconclusive", "Explicit metrics root is unavailable.", nil, "metrics")
		return
	}
	defer root.Close()
	before, err := readMetadata(b.ctx, root, "manifest.json")
	if err != nil {
		b.result.Status.Capture = "incomplete"
		b.finding("capture", "inconclusive", "Metrics manifest unavailable or exceeds metadata limit.", nil, "metrics")
		return
	}
	hash := sha256.Sum256(before)
	manifestRef := b.evidence(filepath.Join(abs, "manifest.json"), hex.EncodeToString(hash[:]))
	a, analysisErr := metrics.Analyze(abs)
	after, readErr := readMetadata(b.ctx, root, "manifest.json")
	if readErr != nil || sha256.Sum256(after) != hash {
		b.result.Status.Capture = "incomplete"
		b.finding("capture", "inconclusive", "Metrics manifest changed during analysis; summaries withheld.", []string{manifestRef}, "metrics")
		return
	}
	if analysisErr != nil {
		b.result.Status.Capture = "incomplete"
		// Analyze errors may contain server response text. Do not copy that text.
		b.finding("capture", "observed", "Metrics Analyze reported invalid evidence (including possible SHA256 mismatch, unsupported manifest, failed response, or invalid samples); affected queries are not reusable measurements.", []string{manifestRef}, "explicit metrics export")
	}
	for i, q := range a.Queries {
		c := Coverage{EvidenceRefs: []string{manifestRef}, Query: q.Name, Status: q.Status, Window: Window{Start: &a.Manifest.Start, End: &a.Manifest.End}}
		if q.Status == metrics.StatusError {
			if strings.Contains(q.Error, "response hash mismatch") {
				b.finding("capture", "observed", "Metrics response SHA256 mismatch; query measurements withheld: "+q.Name, []string{manifestRef}, "metrics byte integrity")
			}
			b.result.MetricsCoverage = append(b.result.MetricsCoverage, c)
			continue
		}
		capture := a.Manifest.Queries[i]
		ref := b.evidence(filepath.Join(abs, capture.File), capture.SHA256)
		ev := &b.result.Evidence[len(b.result.Evidence)-1]
		ev.Schema = "prometheus.query_range/matrix"
		ev.Window = &c.Window
		ev.Query = q.Expr
		ev.JSONPointer = "/data/result"
		c.EvidenceRefs = append(c.EvidenceRefs, ref)
		for _, s := range q.Series {
			c.Series++
			c.Samples += int64(s.Samples)
			c.ValidSamples += int64(s.ValidSamples)
			c.InvalidSamples += int64(s.InvalidSamples)
			c.Expected += s.Expected
			c.MissingApprox += s.MissingApprox
			if s.MissingApprox > 0 || s.InvalidSamples > 0 {
				c.IncompleteSeries++
			}
			if s.FirstTimestamp != nil {
				t := time.UnixMilli(int64(*s.FirstTimestamp * 1000)).UTC()
				if c.SampleWindow.Start == nil || t.Before(*c.SampleWindow.Start) {
					c.SampleWindow.Start = &t
				}
			}
			if s.LastTimestamp != nil {
				t := time.UnixMilli(int64(*s.LastTimestamp * 1000)).UTC()
				if c.SampleWindow.End == nil || t.After(*c.SampleWindow.End) {
					c.SampleWindow.End = &t
				}
			}
		}
		if c.Expected > 0 {
			f := float64(c.ValidSamples) / float64(c.Expected)
			c.Fraction = &f
		}
		b.result.MetricsCoverage = append(b.result.MetricsCoverage, c)
		if c.IncompleteSeries > 0 || q.Status != metrics.StatusSuccess {
			b.finding("capture", "observed", fmt.Sprintf("Query %s: %d/%d series have incomplete sample grids; births/deaths and scrape gaps are not distinguished.", q.Name, c.IncompleteSeries, c.Series), c.EvidenceRefs, "query evaluation window")
		}
	}
}

func (b *builder) capture(root *os.Root, docs map[string]map[string]json.RawMessage, refs map[string]string, summaryPath string) {
	var outcome struct {
		ShutdownFailed *bool `json:"shutdownOrExportFailed"`
	}
	data, _ := json.Marshal(docs["evidence/outcome.json"])
	decodeErr := json.Unmarshal(data, &outcome)
	if decodeErr != nil {
		b.finding("capture", "inconclusive", "Invalid typed shutdownOrExportFailed in outcome; capture outcome is unknown.", []string{refs["evidence/outcome.json"]}, "run shutdown/export")
	}
	if decodeErr == nil && outcome.ShutdownFailed != nil && *outcome.ShutdownFailed {
		b.result.Status.Capture = "incomplete"
		b.finding("capture", "observed", "Run reports shutdown or export failure, independently of workload outcome.", []string{refs["evidence/outcome.json"]}, "run shutdown/export")
	}
	manifestPath := "evidence/record/manifest.json"
	if doc := docs[manifestPath]; doc != nil {
		var m struct {
			Archive string `json:"archive"`
			SHA256  string `json:"sha256"`
			Bytes   int64  `json:"bytes"`
		}
		data, _ := json.Marshal(doc)
		valid := json.Unmarshal(data, &m) == nil && m.Archive == "archive.tar" && m.Bytes > 0 && m.Bytes <= 32<<30
		want, err := hex.DecodeString(m.SHA256)
		valid = valid && err == nil && len(want) == sha256.Size
		archivePath := "evidence/record/archive.tar"
		info, err := root.Lstat(archivePath)
		valid = valid && err == nil
		if valid {
			valid = info.Mode().IsRegular() && info.Size() == m.Bytes
		}
		archiveRefs := []string{refs[manifestPath]}
		if valid {
			f, err := root.Open(archivePath)
			if err != nil {
				valid = false
			} else {
				h := sha256.New()
				buffer := make([]byte, 128<<10)
				var count int64
				for b.ctx.Err() == nil {
					n, readErr := f.Read(buffer)
					count += int64(n)
					_, _ = h.Write(buffer[:n])
					if count > m.Bytes {
						valid = false
						break
					}
					if readErr == io.EOF {
						break
					}
					if readErr != nil {
						valid = false
						break
					}
				}
				f.Close()
				actual := hex.EncodeToString(h.Sum(nil))
				valid = valid && b.ctx.Err() == nil && count == m.Bytes && actual == m.SHA256
				if valid {
					archiveRefs = append(archiveRefs, b.evidence(filepath.Join(b.base, archivePath), actual))
				}
			}
		}
		if !valid {
			b.result.Status.Capture = "incomplete"
			b.finding("capture", "observed", "Recorder archive is missing or failed size/SHA256 verification; archive contents were not decoded.", archiveRefs, "recorder export")
		} else {
			b.finding("capture", "observed", "Recorder archive size and SHA256 verified. Byte integrity does not prove recording completeness; archive contents were not decoded.", archiveRefs, "recorder export bytes")
		}
	}
	if summaryPath == "" {
		b.finding("capture", "inconclusive", "Recorder stream summary not supplied. Archive existence, validation-mode success, and export success do not establish capture health.", nil, "recorder capture")
		return
	}
	abs, err := filepath.Abs(summaryPath)
	if err != nil {
		return
	}
	summaryRoot, err := os.OpenRoot(filepath.Dir(abs))
	if err != nil {
		b.finding("capture", "inconclusive", "Explicit recorder summary is unavailable.", nil, "recorder capture")
		return
	}
	defer summaryRoot.Close()
	type stream struct {
		Errors       *int64 `json:"errors"`
		Gaps         *int64 `json:"gaps"`
		Required     *int64 `json:"required"`
		Initialized  *int64 `json:"initialized"`
		LimitReached bool   `json:"limitReached"`
	}
	var summary struct {
		EnableWatch *bool  `json:"enableWatch"`
		EnableLogs  *bool  `json:"enableLogs"`
		Incomplete  *bool  `json:"incomplete"`
		Watch       stream `json:"watch"`
		Logs        stream `json:"logs"`
	}
	ref := b.document(summaryRoot, filepath.Base(abs), &summary)
	if ref == "" {
		return
	}
	b.finding("capture", "inconclusive", "Explicit summary is detached: linkage to the exported archive, first successful poll, readiness before seeding, and whole experiment window are unverified. A clean summary cannot make whole-run capture complete.", []string{ref}, "supplied recorder stream summary only")
	valid := summary.Incomplete != nil && summary.EnableWatch != nil && summary.EnableLogs != nil
	bad := summary.Incomplete != nil && *summary.Incomplete
	pointRefs := []string{}
	for _, s := range []struct {
		name    string
		enabled *bool
		stream  stream
	}{{"watch", summary.EnableWatch, summary.Watch}, {"logs", summary.EnableLogs, summary.Logs}} {
		if s.enabled == nil || !*s.enabled {
			continue
		}
		for _, counter := range []struct {
			name string
			n    *int64
		}{{"errors", s.stream.Errors}, {"gaps", s.stream.Gaps}} {
			if counter.n == nil || *counter.n < 0 || *counter.n > 1<<53 {
				valid = false
				continue
			}
			bad = bad || *counter.n > 0
			b.point("recorder."+s.name+"."+counter.name, float64(*counter.n), "count", "count", "supplied recorder "+s.name+" stream", []string{ref}, []string{"/" + s.name + "/" + counter.name}, "recorder stream counters", Window{}, []string{"Detached summary linkage and run window not verified; zero is not proof of lossless recording."})
			pointRefs = append(pointRefs, b.result.DataPoints[len(b.result.DataPoints)-1].ID)
		}
		bad = bad || s.stream.LimitReached
		if s.name == "watch" {
			if s.stream.Required == nil || s.stream.Initialized == nil {
				valid = false
			} else {
				bad = bad || *s.stream.Required > *s.stream.Initialized
			}
		}
	}
	if bad {
		b.result.Status.Capture = "incomplete"
		b.finding("capture", "observed", "Supplied recorder summary reports errors, gaps, missing initial watches, a byte limit, or incomplete recording.", []string{ref}, "supplied recorder streams")
		b.result.CarryForward = append(b.result.CarryForward, CarryForward{ID: "repair-capture", Action: "repair and verify recorder", Recommendation: "Investigate recorded stream errors/gaps and prove recorder readiness and full-window coverage before reusing capture-dependent conclusions.", EvidenceRefs: []string{ref}, DataPointRefs: pointRefs, Scope: "recorder harness", BlockedBy: []string{"capture incomplete", "summary/archive linkage unverified"}, Conditions: []string{"preserve original evidence; verify a new capture independently"}, Kind: "fix_harness"})
	}
	if !valid {
		b.finding("capture", "inconclusive", "Recorder summary lacks valid required counters or enabled-stream metadata; omitted values are not zeros.", []string{ref}, "recorder summary")
	}
}

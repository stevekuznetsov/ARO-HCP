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

package report

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/assessment"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/metrics"
	"github.com/Azure/ARO-HCP/tooling/hcp-perf/internal/progress"
)

var epoch = time.Unix(1700000000, 0).UTC()

const matrix = `{"status":"success","warnings":["server warning"],"data":{"resultType":"matrix","result":[{"metric":{"namespace":"hcp","pod":"b","container":"kas","process":"gc","__name__":"up"},"values":[[1700000000,"1"],[1700000001,"0"],[1700000004,"3"]]},{"metric":{"namespace":"hcp","pod":"a","container":"kas","process":"gc"},"values":[[1700000000,"2"],[1700000001,"4"]]}]}}`

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func fixture(t *testing.T, bodies map[string]string) (string, metrics.Manifest) {
	t.Helper()
	dir := t.TempDir()
	m := metrics.Manifest{Version: metrics.ManifestVersion, Start: epoch, End: epoch.Add(4 * time.Second), Step: time.Second}
	for name, body := range bodies {
		hash := sha256.Sum256([]byte(body))
		m.Queries = append(m.Queries, metrics.QueryResult{Query: metrics.Query{Name: name, Expr: "up"}, Status: metrics.StatusSuccess,
			HTTPStatus: 200, File: name + ".json", SHA256: hex.EncodeToString(hash[:]), Warnings: []string{"capture warning"}})
		write(t, filepath.Join(dir, name+".json"), []byte(body))
	}
	putManifest(t, dir, m)
	return dir, m
}

func putManifest(t *testing.T, dir string, m metrics.Manifest) {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "manifest.json"), b)
}

func TestLoadComposeRender(t *testing.T) {
	dir, m := fixture(t, map[string]string{"z-up": matrix, "a-gc": matrix})
	before, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	sources, err := LoadMetrics(context.Background(), []string{dir, dir})
	if err != nil {
		t.Fatal(err)
	}
	p := progress.Result{ObservedAt: epoch, Phase: "steady", Provisional: true, Deadlines: &progress.Durations{Ack: 1e9, Ready: 2e9, Cleanup: 3e9}}
	p.Baseline.Samples = []progress.Canary{{Command: progress.Command{Name: "canary-baseline-1", State: "completed"}, Ack: progress.Stage{Started: epoch, Completed: epoch.Add(time.Second), Duration: 1e9}}}
	p.Baseline.Passed = 1
	page := Compose(p, sources)
	if len(page.Sources) != 2 || !reflect.DeepEqual(page.Sources[0].Manifest, m) {
		t.Fatal("multiple roots or original manifest metadata lost")
	}
	q := page.Sources[0].Queries[0]
	if q.Name != "a-gc" || len(q.Series) != 2 || q.Series[0].Labels["__name__"] != "up" {
		t.Fatalf("query/label sorting or process labels lost: %+v", q)
	}
	s := q.Series[0]
	if s.Samples != 3 || s.ValidSamples != 3 || s.Expected != 5 || s.MissingApprox != 2 || s.Coverage != .6 || s.Gaps != 1 || s.ZeroSamples != 1 || len(s.Points) != 3 || s.Points[2].Segment != 1 {
		t.Fatalf("coverage or gaps incorrect: %+v", s)
	}
	if !strings.Contains(strings.Join(q.Warnings, " "), "capture warning") || !strings.Contains(strings.Join(q.Warnings, " "), "server warning") {
		t.Fatalf("capture or server warning lost: %+v", q.Warnings)
	}
	var html bytes.Buffer
	if err := Render(&html, page); err != nil {
		t.Fatal(err)
	}
	decoded := embeddedPage(t, html.String())
	if decoded.Progress.Deadlines.Ready != 2e9 || decoded.Progress.Baseline.Samples[0].Ack.Duration != 1e9 {
		t.Fatal("canary stages/budgets lost in embedded data")
	}
	for _, want := range []string{"Experiment evidence", "Evidence limits", "500 points", "raw query units", "Source integrity", "All query counts", "SHA-256", "series-chooser", "connect-src 'none'"} {
		if !strings.Contains(html.String(), want) {
			t.Errorf("missing report feature %q", want)
		}
	}
	for _, forbidden := range []string{"<script src", "<link ", "<iframe", "<img", "@import", "url(", "fetch(", "XMLHttpRequest", "innerHTML", "document.write"} {
		if strings.Contains(html.String(), forbidden) {
			t.Errorf("unsafe/external rendering: %q", forbidden)
		}
	}
	after, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("manifest changed")
	}
	for _, name := range []string{"z-up.json", "a-gc.json"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(raw) != matrix {
			t.Fatal("raw source changed")
		}
	}
}

func embeddedPage(t *testing.T, html string) ResultPage {
	t.Helper()
	_, rest, ok := strings.Cut(html, `<script id="report-data" type="application/json">`)
	if !ok {
		t.Fatal("no embedded application/json")
	}
	body, _, ok := strings.Cut(rest, "</script>")
	if !ok {
		t.Fatal("unclosed script data")
	}
	var page ResultPage
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatal(err)
	}
	return page
}

func TestPartialSourcesAndEscaping(t *testing.T) {
	attack := `</script><script>alert("label")</script><img src=x onerror=alert(1)>&` + "\u2028\u2029"
	encoded, _ := json.Marshal(attack)
	body := strings.Replace(matrix, `"gc"`, string(encoded), 1)
	dir, _ := fixture(t, map[string]string{"good": body, "bad": matrix, "empty": `{"status":"success","data":{"resultType":"matrix","result":[]}}`})
	write(t, filepath.Join(dir, "bad.json"), []byte("CORRUPTED-SECRET-RAW-RESPONSE"))
	write(t, filepath.Join(dir, "credentials.json"), []byte("PRIVATE-CREDENTIALS"))
	write(t, filepath.Join(dir, "archive.tar"), []byte("PRIVATE-ARCHIVE"))
	write(t, filepath.Join(dir, "logs.ndjson"), []byte("PRIVATE-LOGS"))
	sources, err := LoadMetrics(context.Background(), []string{filepath.Join(dir, "missing"), dir})
	if err != nil {
		t.Fatalf("one bad source must not abort: %v", err)
	}
	page := Compose(progress.Result{}, sources)
	page.Assessment = &assessment.Result{Experiment: assessment.Experiment{Title: attack, Question: attack}, Findings: []assessment.Finding{{Statement: attack}}, Evidence: []assessment.Evidence{{ID: "e1", Path: attack}}}
	if page.Sources[0].Error == "" || !strings.Contains(page.Sources[1].Error, "response hash mismatch") {
		t.Fatal("source integrity errors hidden")
	}
	if len(page.Sources[1].Queries[0].Series) != 0 || len(page.Sources[1].Queries[1].Series) != 0 || len(page.Sources[1].Queries[2].Series) != 2 {
		t.Fatal("bad/empty queries plotted or healthy query suppressed")
	}
	var html bytes.Buffer
	if err := Render(&html, page); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{attack, "CORRUPTED-SECRET", "PRIVATE-CREDENTIALS", "PRIVATE-ARCHIVE", "PRIVATE-LOGS"} {
		if strings.Contains(html.String(), forbidden) {
			t.Fatalf("untrusted markup or unrelated file content embedded: %s", forbidden)
		}
	}
	if !strings.Contains(html.String(), `\u003c/script\u003e`) || strings.Count(html.String(), "</script>") != 2 {
		t.Fatal("script terminator not escaped")
	}
	decoded := embeddedPage(t, html.String())
	if decoded.Assessment == nil || decoded.Assessment.Experiment.Title != attack || decoded.Assessment.Evidence[0].Path != attack {
		t.Fatal("assessment escaping did not round-trip")
	}
	if decoded.Sources[1].Queries[2].Series[0].Labels["process"] != attack {
		t.Fatal("escaped label did not round-trip")
	}
}

func TestUnsafeSources(t *testing.T) {
	for _, mode := range []string{"filename", "symlink", "invalid-name", "invalid-manifest", "nonfinite"} {
		t.Run(mode, func(t *testing.T) {
			dir, m := fixture(t, map[string]string{"up": matrix})
			switch mode {
			case "filename":
				m.Queries[0].File = "credentials.json"
				write(t, filepath.Join(dir, "credentials.json"), []byte("SECRET"))
				putManifest(t, dir, m)
			case "symlink":
				if err := os.Remove(filepath.Join(dir, "up.json")); err != nil {
					t.Fatal(err)
				}
				private := filepath.Join(t.TempDir(), "private")
				write(t, private, []byte("SECRET"))
				if err := os.Symlink(private, filepath.Join(dir, "up.json")); err != nil {
					t.Fatal(err)
				}
			case "invalid-name":
				m.Queries[0].Name = "../private"
				putManifest(t, dir, m)
			case "invalid-manifest":
				write(t, filepath.Join(dir, "manifest.json"), []byte(`{"version":`))
			case "nonfinite":
				dir, _ = fixture(t, map[string]string{"up": strings.Replace(matrix, `"1"`, `"NaN"`, 1)})
			}
			sources, err := LoadMetrics(context.Background(), []string{dir})
			if err != nil || sources[0].Error == "" {
				t.Fatalf("source error not preserved: %+v %v", sources, err)
			}
			for _, q := range sources[0].Queries {
				for _, s := range q.Series {
					if len(s.Points) != 0 {
						t.Fatal("invalid query plotted")
					}
				}
			}
			var html bytes.Buffer
			if err := Render(&html, Compose(progress.Result{}, sources)); err != nil || strings.Contains(html.String(), "SECRET") {
				t.Fatalf("unsafe source leaked or rendering failed: %v", err)
			}
		})
	}
}

func TestDownsampleExtremaAndGaps(t *testing.T) {
	for _, n := range []int{0, 1, 499, 500, 501, 1001, 100000} {
		points := make([]Point, n)
		for i := range points {
			points[i] = Point{Time: float64(i), Value: math.Sin(float64(i)), Segment: i / 13}
		}
		original := append([]Point(nil), points...)
		got := downsample(points, MaxPoints)
		if len(got) > MaxPoints || (n <= MaxPoints && len(got) != n) {
			t.Fatalf("point cap or small series changed: n=%d got=%d", n, len(got))
		}
		for i, pt := range got {
			if pt != original[int(pt.Time)] || (i > 0 && got[i-1].Time >= pt.Time) {
				t.Fatal("timestamp order or original gap segment lost")
			}
		}
		if n > MaxPoints {
			retained := map[float64]bool{}
			for _, pt := range got {
				retained[pt.Time] = true
			}
			for b := 0; b < MaxPoints/2; b++ {
				start, end := b*n/(MaxPoints/2), (b+1)*n/(MaxPoints/2)
				lo, hi := start, start
				for i := start; i < end; i++ {
					if points[i].Value < points[lo].Value {
						lo = i
					}
					if points[i].Value >= points[hi].Value {
						hi = i
					}
				}
				if !retained[points[lo].Time] || !retained[points[hi].Time] {
					t.Fatal("bucket extrema lost")
				}
			}
		}
		for i := range points {
			if points[i] != original[i] {
				t.Fatal("input mutated")
			}
		}
	}
}

func TestChangedResponseWithheld(t *testing.T) {
	dir, m := fixture(t, map[string]string{"up": matrix})
	write(t, filepath.Join(dir, "up.json"), []byte(strings.Replace(matrix, `"gc"`, `"replacement"`, 1)))
	if err := loadPoints(context.Background(), dir, m.Queries[0], 1, nil, 2, DefaultMaxPoints); err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("changed bytes accepted after analysis: %v", err)
	}
}

func TestLargeCaptureReductionOnly(t *testing.T) {
	const count = 10001
	values := make([][2]any, count)
	for i := range values {
		values[i] = [2]any{epoch.Unix() + int64(i), strconv.Itoa(i % 37)}
	}
	values[1234][1] = "-999"
	values[9876][1] = "999"
	body, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": []any{map[string]any{"metric": map[string]string{"pod": "kas", "container": "api", "process": "gc"}, "values": values}}}})
	if err != nil {
		t.Fatal(err)
	}
	dir, m := fixture(t, map[string]string{"process-gc": string(body)})
	m.End = epoch.Add((count - 1) * time.Second)
	putManifest(t, dir, m)
	sources, err := LoadMetrics(context.Background(), []string{dir})
	if err != nil || sources[0].Error != "" {
		t.Fatalf("large capture rejected: %v %+v", err, sources)
	}
	s := sources[0].Queries[0].Series[0]
	if len(s.Points) > DefaultMaxPoints || s.Samples != count || s.ValidSamples != count || s.Expected != count || s.Coverage != 1 || *s.Min != -999 || *s.Max != 999 {
		t.Fatalf("full-source metadata reduced with visualization: %+v", s.SeriesSummary)
	}
	var low, high bool
	for _, pt := range s.Points {
		low = low || pt.Value == -999
		high = high || pt.Value == 999
	}
	if !low || !high {
		t.Fatal("large capture extrema dropped")
	}
	after, err := os.ReadFile(filepath.Join(dir, "process-gc.json"))
	if err != nil || !bytes.Equal(after, body) {
		t.Fatal("large raw capture modified")
	}
}

func TestBoundedHighCardinalityPreview(t *testing.T) {
	bodies := map[string]string{}
	for query := 0; query < 12; query++ {
		name, count := fmt.Sprintf("node-memory-%02d", query), 25
		if query == 0 {
			name, count = "aaa-api-histogram", 1000
		}
		if query == 1 {
			name, count = "scrape-up", 43
		}
		rows := make([]any, 0, count)
		for i := 0; i < count; i++ {
			labels := map[string]string{"pod": fmt.Sprintf("pod-%04d", i), "container": "worker", "namespace": "hcp"}
			if query == 0 {
				labels["le"] = strconv.Itoa(i)
			}
			if i < 3 {
				labels["container"] = []string{"kas", "etcd", "cpo"}[i]
			}
			values := make([][2]any, 0, 35)
			for j := 0; j < 35; j++ {
				if j == 15 {
					continue
				}
				v := i + j
				if j == 20 {
					v = -1000 - i
				}
				if j == 30 {
					v = 1000 + i
				}
				values = append(values, [2]any{epoch.Unix() + int64(j), strconv.Itoa(v)})
			}
			rows = append(rows, map[string]any{"metric": labels, "values": values})
		}
		body, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": rows}})
		if err != nil {
			t.Fatal(err)
		}
		bodies[name] = string(body)
	}
	dir, m := fixture(t, bodies)
	m.End = epoch.Add(34 * time.Second)
	for i := range m.Queries {
		if m.Queries[i].Name == "aaa-api-histogram" {
			for j := 0; j < 1000; j++ {
				m.Queries[i].Warnings = append(m.Queries[i].Warnings, fmt.Sprintf("capture warning %d", j))
			}
		}
	}
	putManifest(t, dir, m)
	manifestBefore, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	limits := Limits{MaxSeries: 200, MaxPoints: 6}
	sources, err := LoadMetricsWithLimits(context.Background(), []string{dir}, limits)
	if err != nil || sources[0].Error != "" {
		t.Fatalf("load: %v %s", err, sources[0].Error)
	}
	page := Compose(progress.Result{}, sources)
	const total = 1000 + 43 + 10*25
	if page.TotalSeries != total || page.PreviewSeries != 200 || page.OmittedSeries != total-200 || page.PlottedPoints > 1200 {
		t.Fatalf("global preview bounds/counts: total=%d preview=%d omitted=%d points=%d", page.TotalSeries, page.PreviewSeries, page.OmittedSeries, page.PlottedPoints)
	}
	for _, q := range page.Sources[0].Queries {
		if q.Totals.Samples != int64(q.Totals.Series*34) || q.Totals.ValidSamples != q.Totals.Samples || q.Totals.Expected != int64(q.Totals.Series*35) || q.Totals.MissingApprox != int64(q.Totals.Series) || q.Totals.IncompleteSeries != q.Totals.Series || math.Abs(*q.Totals.Coverage-34.0/35) > 1e-12 {
			t.Fatalf("all-series coverage was reduced: %s %+v", q.Name, q.Totals)
		}
		if q.Name == "scrape-up" {
			if len(q.Series) != 43 || q.OmittedSeries != 0 {
				t.Fatal("scrape targets not reserved")
			}
		} else if len(q.Series) < 14 || len(q.Series) > 15 {
			t.Fatalf("query round-robin not balanced: %s %d", q.Name, len(q.Series))
		}
		if q.OmittedSeries != q.Totals.Series-len(q.Series) || q.OmittedWarnings == 0 || len(q.Warnings) > maxWarnings {
			t.Fatalf("omissions not explicit: %+v", q)
		}
		important := map[string]bool{}
		for _, s := range q.Series {
			important[s.Labels["container"]] = true
			if len(s.Points) > limits.MaxPoints || s.Gaps != 1 || s.Samples != 34 {
				t.Fatal("point/metadata bounds")
			}
			var low, high bool
			for _, pt := range s.Points {
				low = low || pt.Value == *s.Min
				high = high || pt.Value == *s.Max
				if pt.Time < float64(epoch.Unix()+16) && pt.Segment != 0 || pt.Time >= float64(epoch.Unix()+16) && pt.Segment != 1 {
					t.Fatal("gap bridged after reduction")
				}
			}
			if !low || !high {
				t.Fatal("extrema omitted")
			}
		}
		if !important["kas"] || !important["etcd"] || !important["cpo"] {
			t.Fatal("control-plane labels not promoted")
		}
	}
	var html bytes.Buffer
	if err := Render(&html, page); err != nil {
		t.Fatal(err)
	}
	if html.Len() > 400000 {
		t.Fatalf("bounded preview unexpectedly large: %d bytes", html.Len())
	}
	if strings.Contains(html.String(), `"labelText":`) || strings.Contains(html.String(), `"segment":`) {
		t.Fatal("duplicated labels or uncompressed point objects embedded")
	}
	decoded := embeddedPage(t, html.String())
	if decoded.TotalSeries != total || decoded.Sources[0].ManifestWarningsOmitted == 0 {
		t.Fatal("totals/warning omissions lost in HTML")
	}
	manifestAfter, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil || !bytes.Equal(manifestAfter, manifestBefore) {
		t.Fatal("source manifest changed during preview rendering")
	}
	// Reversing manifest order cannot let the histogram query consume all slots.
	for i, j := 0, len(m.Queries)-1; i < j; i, j = i+1, j-1 {
		m.Queries[i], m.Queries[j] = m.Queries[j], m.Queries[i]
	}
	putManifest(t, dir, m)
	again, err := LoadMetricsWithLimits(context.Background(), []string{dir}, limits)
	if err != nil {
		t.Fatal(err)
	}
	second := Compose(progress.Result{}, again)
	for i, q := range page.Sources[0].Queries {
		other := second.Sources[0].Queries[i]
		if len(q.Series) != len(other.Series) {
			t.Fatal("manifest ordering changed allocation")
		}
		for j := range q.Series {
			if q.Series[j].LabelText != other.Series[j].LabelText {
				t.Fatal("nondeterministic selection")
			}
		}
	}
	write(t, filepath.Join(dir, "manifest.json"), manifestBefore)
	for name, body := range bodies {
		raw, err := os.ReadFile(filepath.Join(dir, name+".json"))
		if err != nil || string(raw) != body {
			t.Fatal("raw data modified")
		}
	}
	t.Logf("%d analyzed series -> %d preview series, %d points, %d-byte HTML", total, page.PreviewSeries, page.PlottedPoints, html.Len())
}

func TestPreviewRankingAndSmallBudgets(t *testing.T) {
	var rows []any
	for i := 0; i < 1000; i++ {
		rows = append(rows, map[string]any{"metric": map[string]string{"pod": fmt.Sprintf("pod-%04d", i)}, "values": [][2]any{{epoch.Unix(), strconv.Itoa(-i)}}})
	}
	body, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": rows}})
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := fixture(t, map[string]string{"node-cpu": string(body), "node-memory": string(body), "histogram": string(body)})
	for _, limit := range []int{1, 3, 200, 1000} {
		sources, err := LoadMetricsWithLimits(context.Background(), []string{dir}, Limits{limit, 3})
		if err != nil {
			t.Fatal(err)
		}
		page := Compose(progress.Result{}, sources)
		if page.TotalSeries != 3000 || page.PreviewSeries > limit || page.PreviewSeries > 60 || page.OmittedSeries != 3000-page.PreviewSeries {
			t.Fatal("query/global caps ignored")
		}
		for _, q := range page.Sources[0].Queries {
			if q.Name == "node-cpu" {
				found := false
				for _, s := range q.Series {
					found = found || s.Labels["pod"] == "pod-0999"
				}
				if !found {
					t.Fatal("maximum absolute value not selected for CPU")
				}
			}
		}
	}
	for _, limits := range []Limits{{0, 300}, {-1, 300}, {1001, 300}, {1, 1}, {1, 501}} {
		if _, err := LoadMetricsWithLimits(context.Background(), []string{dir}, limits); err == nil {
			t.Fatalf("invalid limits accepted: %+v", limits)
		}
	}
	points := []Point{{0, 1, 0}, {1, -100, 0}, {4, 100, 1}, {5, 2, 1}}
	for _, limit := range []int{2, 3, 4, 300} {
		got := downsample(points, limit)
		if len(got) > limit {
			t.Fatal("odd/tiny point limit exceeded")
		}
		if limit < 4 && (len(got) != 2 || got[0] != points[1] || got[1] != points[2]) {
			t.Fatal("extrema/gaps lost for tiny budget")
		}
	}
}

func TestCancelAndWriterErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := LoadMetrics(ctx, []string{t.TempDir()}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
	if err := Render(errorWriter{}, ResultPage{}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("writer error ignored: %v", err)
	}
	if err := Render(io.Discard, ResultPage{Sources: []MetricSource{{Queries: []Query{{Series: []Series{{Points: []Point{{Value: math.NaN()}}}}}}}}}); err == nil {
		t.Fatal("non-JSON numeric value accepted")
	}
}

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestEmbeddedJavaScriptSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; Go/data safety tests still run")
	}
	script := strings.TrimSuffix(strings.TrimPrefix(htmlTail, "</script><script>"), "</script></body></html>")
	cmd := exec.Command(node, "--check")
	cmd.Stdin = strings.NewReader(script)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("embedded JavaScript syntax: %v\n%s", err, output)
	}
}

// A small DOM stand-in catches JavaScript/data contract and interaction errors
// offline. This is not a layout, accessibility, or real-browser visual test.
func TestEmbeddedJavaScriptInteractions(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	dir, _ := fixture(t, map[string]string{"cpu": matrix, "empty": `{"status":"success","data":{"resultType":"matrix","result":[]}}`})
	sources, err := LoadMetrics(context.Background(), []string{dir})
	if err != nil {
		t.Fatal(err)
	}
	p := progress.Result{ObservedAt: epoch.Add(time.Minute), Phase: "steady", Provisional: true, Deadlines: &progress.Durations{Ack: 1e9, Ready: 2e9, Cleanup: 3e9}}
	p.Baseline.Samples = []progress.Canary{{Command: progress.Command{Name: "canary-baseline-1", State: "completed"}, Ack: progress.Stage{Started: epoch, Completed: epoch.Add(time.Second), Duration: 1e9}}}
	p.SteadyCanaries.Samples = []progress.Canary{{Command: progress.Command{Name: "canary-steady-1", State: "inProgress"},
		Ack:     progress.Stage{Started: epoch, Completed: epoch.Add(time.Millisecond), Duration: 1e6},
		Ready:   progress.Stage{Started: epoch, Completed: epoch.Add(5 * time.Second), Duration: 5e9},
		Cleanup: progress.Stage{Started: epoch.Add(5 * time.Second)}}}
	p.Commands = []progress.Command{{Name: "turnover", State: "inProgress", StartedAt: &epoch},
		{Name: "canary-steady-1", State: "inProgress", StartedAt: &epoch},
		{Name: "canary-baseline-1", State: "completed", StartedAt: &epoch},
		{Name: "canary-warmup", State: "completed", StartedAt: &epoch},
		{Name: "seed-pod-gate", State: "completed", StartedAt: &epoch}}
	p.WindowStart, p.Steady.StartedAt = &epoch, &epoch
	page := Compose(p, sources)
	for i := range page.Sources[0].Queries[0].Series {
		labels := page.Sources[0].Queries[0].Series[i].Labels
		labels["pod"], labels["id"], labels["image"] = "same-pod", fmt.Sprintf("raw-id-%d", i), "private-image-digest"
		labels["cpu"], labels["mode"] = strconv.Itoa(i), "idle"
	}
	assessedPage := page
	assessedPage.Assessment = &assessment.Result{
		SchemaVersion: assessment.SchemaVersion, AnalysisVersion: assessment.AnalysisVersion, GeneratedAt: epoch,
		Experiment: assessment.Experiment{Title: "A bounded experiment", Question: "Does the canary complete under pressure?", MeasurementMode: "unspecified"},
		Status:     assessment.Status{Workload: "pass", Capture: "incomplete", Qualification: "not_assessed"},
		Findings: []assessment.Finding{
			{Category: "workload", Status: "observed", Statement: "Recorded cycle completed", Evidence: []string{"e1"}, Scope: "test workload"},
			{Category: "capture", Status: "observed", Statement: "Recorder stream has gaps", Evidence: []string{"e1"}},
			{Category: "measurement", Status: "inconclusive", Statement: "Method unspecified"},
			{Category: "resources", Status: "inconclusive", Statement: "No sizing conclusion"},
		},
		Evidence:     []assessment.Evidence{{ID: "e1", Path: "commands/canary.json", SHA256: strings.Repeat("a", 64)}},
		DataPoints:   []assessment.DataPoint{{ID: "d1", Name: "canary.ready", Value: 5, Unit: "seconds", Statistic: "median", MeasurementMethod: "unspecified", Population: "completed steady cycles", Applicability: "this run only", Provenance: assessment.Provenance{EvidenceRefs: []string{"e1"}, JSONPointers: []string{"/ready/durationNanoseconds"}}, Limitations: []string{"Not pure server latency"}}},
		CarryForward: []assessment.CarryForward{{ID: "c1", Recommendation: "Retain scoped observations", Kind: "retain", Action: "retain", EvidenceRefs: []string{"e1"}, DataPointRefs: []string{"d1"}, Scope: "same measurement", BlockedBy: []string{"method unspecified"}, Conditions: []string{"verify equivalence"}}},
	}
	for _, tc := range []struct {
		name   string
		page   ResultPage
		setup  string
		checks string
	}{
		{name: "empty", page: Compose(progress.Result{}, nil)},
		{name: "metrics-and-canaries", page: page},
		{name: "node-labels", page: page, setup: `for(const ss of fixtureData.sources[0].queries[0].series)ss.labels={node:'worker',cpu:ss.labels.cpu,mode:'idle',id:ss.labels.id,image:ss.labels.image};`},
		{name: "target-labels", page: page, setup: `for(const ss of fixtureData.sources[0].queries[0].series)ss.labels={job:'node-exporter',instance:'worker:9100',cpu:ss.labels.cpu,id:ss.labels.id,image:ss.labels.image};`},
		{name: "measurement-modes", page: page, setup: `fixtureData.progress.measurementMode='poll';fixtureData.progress.steadyCanaries.samples[0].measurementMode='watch';`},
		{name: "historical-heading", page: assessedPage, setup: `fixtureData.assessment.experiment={title:'unknown',question:'unknown',mode:'validation',cpuTreatment:'quiet',guestNodes:3,pods:15,applications:3,steadySeconds:300};`, checks: `
assert.equal($('experiment-title').textContent,'Quiet harness validation: 3 nodes, 15 application Pods');
assert.equal($('purpose').textContent,'Did the Deployment lifecycle checks complete, and is the collected evidence usable for evaluating the harness?');
assert.ok($('assessment-metadata').textContent.includes('Recorded objective not supplied'));
assert.equal(data.assessment.experiment.title,'unknown','descriptive heading does not modify experiment metadata');`},
		{name: "partial-heading", page: assessedPage, setup: `fixtureData.assessment.experiment={title:'',question:'',mode:'unknown',cpuTreatment:'unknown',guestNodes:null,pods:null,applications:0};`, checks: `
assert.equal($('experiment-title').textContent,'Experiment evidence: 0 applications');
assert.equal($('purpose').textContent,'Did the workload complete, and is its evidence usable?');
assert.ok(!$('experiment-title').textContent.includes('Pods'),'application count is not converted into unrecorded Pod count');`},
		{name: "unknown-heading", page: assessedPage, setup: `fixtureData.assessment.experiment={title:'unknown',question:'unknown'};`, checks: `
assert.equal($('experiment-title').textContent,'Experiment evidence');
assert.equal($('purpose').textContent,'Did the workload complete, and is its evidence usable?');`},
		{name: "many-evidence-sources", page: assessedPage, setup: `fixtureData.assessment.evidence=Array.from({length:100},(_,i)=>({id:'e'+i,path:'commands/canary-'+i+'.json',sha256:'a'.repeat(64)}));fixtureData.assessment.findings[0].evidence=fixtureData.assessment.evidence.map(e=>e.id);`, checks: `
const finding=$('worked-list').children[0],groups=finding.children.filter(n=>n.tag==='details');
assert.equal(groups.length,1,'one collapsed group regardless of evidence count');
assert.equal(groups[0].children[0].textContent,'Evidence (100 sources)');
assert.ok(!groups[0].open,'evidence group starts collapsed');
assert.equal(descend(groups[0],'details').length,0,'sources are subordinate rows, not more disclosure controls');
assert.equal(descend(groups[0],'strong').length,100,'all source references remain inspectable');
assert.ok(descend(groups[0],'p').some(n=>n.textContent==='commands/canary-99.json'));`},
		{name: "capture-submission", page: page, setup: `fixtureData.progress.commands[0].name='record-start';`, checks: `
const markers=descend($('timeline'),'line').filter(n=>n.attrs['data-phase']);
assert.equal(markers.length,1);
assert.equal(markers[0].attrs['data-phase'],'Capture submission');
assert.ok(descend(markers[0],'title')[0].textContent.includes('not recorder readiness or process start'));
assert.ok(!descend($('timeline'),'line').some(n=>n.attrs['data-phase']==='capture start'),'submission cannot imply capture began');`},
		{name: "assessment", page: assessedPage, checks: `
assert.equal($('findings-summary').textContent,'All findings (4)');
assert.equal($('recommendations-summary').textContent,'All recommendations (1)');
assert.equal($('carry-forward-lead').textContent,'Retain scoped observations');
assert.equal($('experiment-title').textContent,'A bounded experiment');
assert.equal($('purpose').textContent,'Does the canary complete under pressure?');
assert.deepEqual(descend($('assessment-status'),'strong').map(n=>n.textContent),['Passed','Unknown','Incomplete']);
assert.equal($('worked-list').children.length,1);
assert.ok(descend($('worked-list'),'p').some(n=>n.textContent==='commands/canary.json'));
assert.ok(descend($('worked-list'),'p').some(n=>n.textContent==='SHA-256: '+'a'.repeat(64)));
assert.equal($('limits-list').children.length,3);
assert.equal($('carry-forward-list').children.length,1);
assert.ok(descend($('carry-forward-list'),'p').some(n=>n.textContent.includes('Data point [d1]: canary.ready = 5 seconds (median; unspecified; completed steady cycles)')));
assert.ok(descend($('carry-forward-list'),'p').some(n=>n.textContent==='Blocked by: method unspecified'));
assert.ok(descend($('carry-forward-list'),'small').some(n=>n.textContent===' [retain]'));
assert.deepEqual(descend($('data-point-table'),'td').map(n=>n.textContent),['canary.ready','5','seconds','median','unspecified','completed steady cycles','this run only']);
assert.ok(descend($('data-point-details'),'p').some(n=>n.textContent==='Limit: Not pure server latency'));`},
		{name: "executive-summary", page: assessedPage, setup: `
fixtureData.progress.steadyCanaries.passed=10;fixtureData.progress.steadyCanaries.expected=10;
fixtureData.progress.turnover={completed:1,scheduled:1};
fixtureData.assessment.dataPoints.push({id:'errors',name:'recorder.logs.errors',value:642,unit:'count',statistic:'count'},{id:'gaps',name:'recorder.logs.gaps',value:216,unit:'count',statistic:'count'});
fixtureData.assessment.carryForward.push({kind:'fix_harness',recommendation:'Review recording before reuse. '.repeat(30)});`, checks: `
assert.equal($('worked-lead').textContent,'10/10 steady canaries and 1/1 rollouts completed. The recorded workload passed.');
assert.equal($('limits-lead').textContent,'Recording incomplete: 642 log errors, 216 log gaps. Review evidence limits before reuse.');
assert.ok($('carry-forward-lead').textContent.startsWith('Address the recorded harness issues'));
assert.ok(descend($('carry-forward-list'),'span').some(n=>n.textContent.length>180),'full recommendation is retained, not truncated');`},
		{name: "failed-workload-complete-capture", page: assessedPage, setup: `fixtureData.assessment.status={workload:'fail',capture:'complete'};`, checks: `
assert.deepEqual(descend($('assessment-status'),'strong').map(n=>n.textContent),['Failed','Unknown','Complete']);
assert.equal($('worked-list').children.length,1);
assert.equal(descend($('worked-list'),'span')[0].textContent,'Recorder stream has gaps','classification uses axes, never statement parsing');
assert.equal(descend($('limits-list'),'span')[0].textContent,'Recorded cycle completed');`},
		{name: "untrusted-assessment", page: page, setup: `fixtureData.assessment={experiment:{title:'<img src=x onerror=alert(1)>',question:'</script><script>alert(1)</script>'},status:{workload:'pass',capture:'future'},evidence:[{id:'bad',path:'javascript:alert(1)',sha256:'<img src=x>'}],findings:[{category:'workload',status:'<script>',statement:'Unvalidated claim',evidence:['bad','missing']},{category:'__proto__',status:'observed',statement:'Unknown category'},{category:'workload',status:'inconclusive',statement:'Not established'},null]};`, checks: `
assert.equal($('experiment-title').textContent,'<img src=x onerror=alert(1)>');
assert.equal($('purpose').textContent,'</script><script>alert(1)</script>');
assert.equal($('limits-list').children.length,3);
assert.ok($('worked-list').children[0].textContent.includes('No positive'));
assert.ok(descend($('limits-list'),'small').some(n=>n.textContent===' [workload / inconclusive]'));
assert.ok(descend($('limits-list'),'p').some(n=>n.textContent==='Unresolved evidence reference'));
assert.ok(descend($('limits-list'),'p').some(n=>n.textContent==='javascript:alert(1)'));
assert.equal(descend($('limits-list'),'a').length,0);
assert.equal(descend($('limits-list'),'script').length,0);
assert.equal(descend($('assessment-status'),'strong')[1].textContent,'Unknown');`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var html bytes.Buffer
			if err := Render(&html, tc.page); err != nil {
				t.Fatal(err)
			}
			_, rest, _ := strings.Cut(html.String(), `<script id="report-data" type="application/json">`)
			body, _, _ := strings.Cut(rest, "</script>")
			encoded, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			script := strings.TrimSuffix(strings.TrimPrefix(htmlTail, "</script><script>"), "</script></body></html>")
			cmd := exec.Command(node)
			setup := "\nconst fixtureData=JSON.parse(" + string(encoded) + ");\n" + tc.setup + "\ndocument.getElementById('report-data').textContent=JSON.stringify(fixtureData);\n"
			cmd.Stdin = strings.NewReader(mockDOM + setup + script + interactionChecks + tc.checks)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("embedded report interactions: %v\n%s", err, output)
			}
		})
	}
}

const mockDOM = `
const assert=require('node:assert/strict');
class Element {
  constructor(tag){this.tag=tag;this.children=[];this.style={};this.attrs={};this.textContent='';this.selected=false;this.checked=false;this.disabled=false;this._value='';}
  appendChild(child){this.children.push(child);child.parent=this;return child;}
  setAttribute(key,value){this.attrs[key]=value;}
  replaceChildren(){this.children=[];}
  remove(){this.parent.children=this.parent.children.filter(n=>n!==this);}
  get options(){return this.children.filter(n=>n.tag==='option');}
  get selectedOptions(){return this.options.filter(n=>n.selected);}
  set value(value){this._value=String(value);}
  get value(){return this._value||(this.tag==='select'?this.options[0]?.value:'')||'';}
}
const elements=new Map();
const document={
  getElementById(id){if(!elements.has(id))elements.set(id,new Element(id==='query-choice'?'select':'div'));return elements.get(id);},
  createElement(tag){return new Element(tag);},
  createElementNS(ns,tag){assert.equal(ns,'http://www.w3.org/2000/svg');return new Element(tag);}
};
function descend(root,tag){return root.children.flatMap(n=>[...(n.tag===tag?[n]:[]),...descend(n,tag)]);}
`

const interactionChecks = `
for(const id of ['worked-lead','limits-lead','carry-forward-lead'])assert.ok($(id).textContent.length>0&&$(id).textContent.length<=180,'bounded executive lead: '+id);
assert.equal(document.getElementById('overview').children.length,4);
assert.ok(descend(document.getElementById('metric-table'),'table').length);
assert.ok(descend(document.getElementById('query-table'),'table').length);
assert.ok(document.getElementById('preview-status').textContent.includes('omitted'));
if(!data.assessment){
  assert.equal($('experiment-title').textContent,'Experiment evidence');
  for(const id of ['worked-list','limits-list','carry-forward-list'])assert.ok($(id).children[0].textContent.includes('Not analyzed'));
}
if(p.measurementMode)assert.ok($('measurement-note').textContent.includes('Recorded measurement mode(s): poll, watch.'));
else assert.ok($('measurement-note').textContent.includes('not recorded in this artifact'));
assert.equal($('show-baseline').checked,false,'baseline cycles are opt-in');
const filter=document.getElementById('query-filter'),add=document.getElementById('add-panel');
filter.value='[';filter.oninput();assert.equal(add.disabled,true);
filter.value='';filter.oninput();
if(queries.length){
  assert.equal(add.disabled,false);
  const panels=document.getElementById('panels'),count=panels.children.length;
  add.onclick();assert.equal(panels.children.length,count+1);
  const panel=panels.children.at(-1),chooser=descend(panel,'select')[0];
  assert.ok(chooser.options.length<=data.limits.maxSeries,'bounded preview chooser');
  assert.equal(chooser.options.length,2);
  for(const o of chooser.options){
    assert.ok(!o.textContent.includes('id=')&&!o.textContent.includes('image='),'raw identity and image hidden by default');
    assert.ok(o.textContent.includes('[series '),'colliding labels are disambiguated');
    assert.ok(o.title.includes('id=')&&o.title.includes('image='),'full labels retained in tooltip');
  }
  const inputs=descend(panel,'input'),defaults=inputs.filter(n=>n.checked).map(n=>n.value);
  if(inputs.some(n=>n.value==='namespace'))assert.deepEqual(defaults,['container','namespace','pod']);
  else if(inputs.some(n=>n.value==='node'))assert.deepEqual(defaults,['node']);
  else assert.deepEqual(defaults,['instance','job']);
  assert.ok(descend(panel,'td').some(n=>n.textContent.includes('private-image-digest')),'full labels retained in expandable table');
  assert.ok(descend(panel,'p').some(n=>n.textContent.includes('Distinct raw series')),'collision warning visible');
  const beforePaths=descend(panel,'path').map(n=>({...n.attrs})),beforeSeries=JSON.stringify(data.sources);
  const idToggle=inputs.find(n=>n.value==='id');idToggle.checked=true;idToggle.onchange();
  assert.deepEqual(descend(panel,'path').map(n=>({...n.attrs})),beforePaths,'display labels cannot alter traces or identity');
  assert.equal(JSON.stringify(data.sources),beforeSeries,'raw label sets and samples cannot change');
  assert.ok(chooser.options.every(o=>o.textContent.includes('id=')));
  idToggle.checked=false;idToggle.onchange();
  chooser.options.forEach(o=>o.selected=false);chooser.onchange();assert.equal(descend(panel,'svg').length,0);
  chooser.options.forEach(o=>o.selected=true);chooser.onchange();assert.equal(descend(panel,'svg').length,1);
  assert.ok(descend(panel,'path').length>=2,'separate container paths');
  const path=descend(panel,'path').find(n=>n.attrs.d.split(' M').length>2);assert.ok(path,'gap breaks preserved');
  descend(panel,'button')[0].onclick();assert.equal(panels.children.length,count);
  filter.value='no-such-query';filter.oninput();assert.equal(add.disabled,true);
  assert.ok(descend(document.getElementById('timeline'),'rect').some(n=>n.attrs['stroke-dasharray']==='4 3'),'pending stage bar');
  const timeline=$('timeline'),cycles=descend(timeline,'g').filter(n=>n.attrs['data-cycle']);
  assert.equal(cycles.length,1,'only steady cycle shown, commands never create duplicate cycle rows');
  const bars=descend(cycles[0],'rect');assert.equal(bars.length,3,'one cycle has three stage bars, not duplicate command bars');
  assert.deepEqual(bars.map(n=>n.attrs['data-stage']),['ack','ready','cleanup']);
  assert.equal(new Set(bars.map(n=>n.attrs.y)).size,3,'ACK and Ready use separate lanes');
  assert.equal(bars[0].attrs.x,bars[1].attrs.x,'ACK remains aligned with Ready start');
  assert.ok(Number(bars[0].attrs.width)>=4,'short ACK remains visible');
  assert.equal(descend(timeline,'line').filter(n=>n.attrs['data-phase']).length,1,'only recognized phase commands create markers');
  assert.equal(descend($('command-table'),'tbody')[0].children.length,5,'all commands retained in details');
  assert.equal(descend($('canary-table'),'tbody')[0].children.length,1,'compact table has one row per cycle');
  const baseline=$('show-baseline'),steadyX=bars[0].attrs.x;baseline.checked=true;baseline.onchange();
  assert.equal(descend(timeline,'g').length,2,'baseline is inspectable');
  assert.equal(descend($('canary-table'),'tbody')[0].children.length,2);
  assert.equal(descend(timeline,'g').find(n=>n.attrs['data-cycle'].startsWith('steady')).children.find(n=>n.tag==='rect').attrs.x,steadyX,'baseline toggle preserves shared UTC range');
  baseline.checked=false;baseline.onchange();assert.equal(descend(timeline,'g').length,1);
}else{assert.equal(add.disabled,true);}
`

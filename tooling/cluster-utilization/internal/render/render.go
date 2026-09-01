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

// Package render turns a processed report into a single self-contained HTML
// page (Tailwind + ECharts are loaded from CDNs; all data is inlined).
package render

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"time"

	"github.com/go-logr/logr"

	"github.com/Azure/ARO-HCP/tooling/cluster-utilization/internal/model"
)

//go:embed report.html.tmpl
var reportTemplate string

// Options controls a render run.
type Options struct {
	Input  string // report JSON (output of process)
	Output string // HTML file to write
}

// Run reads the report JSON and writes the HTML page.
func Run(log logr.Logger, opts Options) error {
	b, err := os.ReadFile(opts.Input)
	if err != nil {
		return fmt.Errorf("reading report %s: %w", opts.Input, err)
	}
	var report model.Report
	if err := json.Unmarshal(b, &report); err != nil {
		return fmt.Errorf("parsing report: %w", err)
	}

	// json.Marshal escapes <, > and & as \u003c/\u003e/\u0026, so the payload is
	// safe to inline directly inside a <script> block.
	payload, err := json.Marshal(report)
	if err != nil {
		return err
	}

	tmpl, err := template.New("report").Funcs(template.FuncMap{}).Parse(reportTemplate)
	if err != nil {
		return fmt.Errorf("parsing template: %w", err)
	}

	data := struct {
		Data        template.JS
		GeneratedAt string
		Window      string
		Percentile  float64
	}{
		Data:        template.JS(payload),
		GeneratedAt: report.GeneratedAt.Format(time.RFC3339),
		Window:      report.Window,
		Percentile:  report.Percentile,
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return fmt.Errorf("executing template: %w", err)
	}
	if err := os.WriteFile(opts.Output, buf.Bytes(), 0o644); err != nil {
		return err
	}
	log.Info("wrote report HTML", "file", opts.Output, "bytes", buf.Len())
	return nil
}

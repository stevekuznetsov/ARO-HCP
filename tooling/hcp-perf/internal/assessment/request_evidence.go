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
	"regexp"
	"strings"
	"time"
)

const RequestEvidenceVersion = "hcp-perf.request-evidence/v1"

var auditIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
var scalePathPattern = regexp.MustCompile(`^/apis/apps/v1/namespaces/[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?/deployments/canary/scale$`)

// Explicitly supplied summaries authorize hashing only local sources below their
// directory. artifactReferences are not read: imported paths cannot authorize
// unrelated filesystem access or replace the run's independently read evidence.
func (b *builder) requestEvidence(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("request evidence path unavailable")
	}
	root, err := os.OpenRoot(filepath.Dir(abs))
	if err != nil {
		return fmt.Errorf("request evidence directory unavailable")
	}
	defer root.Close()
	data, err := readMetadata(b.ctx, root, filepath.Base(abs))
	if err != nil {
		return fmt.Errorf("request evidence must be bounded regular JSON metadata")
	}
	var doc struct {
		SchemaVersion string `json:"schemaVersion"`
		Observations  []struct {
			Command      string    `json:"command"`
			AuditID      string    `json:"auditID"`
			Verb         string    `json:"verb"`
			Path         string    `json:"path"`
			ObjectUID    string    `json:"objectUID"`
			ResponseCode int       `json:"responseCode"`
			Stage        string    `json:"stage"`
			ReceivedAt   time.Time `json:"receivedAt"`
			CompletedAt  time.Time `json:"completedAt"`
			SourcePath   string    `json:"sourcePath"`
			SourceSHA256 string    `json:"sourceSHA256"`
			SourceLine   *int64    `json:"sourceLine"`
			Correlation  string    `json:"correlation"`
		} `json:"observations"`
	}
	if json.Unmarshal(data, &doc) != nil || doc.SchemaVersion != RequestEvidenceVersion || len(doc.Observations) == 0 {
		return fmt.Errorf("invalid or unsupported request evidence schema or observations")
	}
	hash := sha256.Sum256(data)
	ref := b.evidence(abs, hex.EncodeToString(hash[:]))
	b.result.Evidence[len(b.result.Evidence)-1].Schema = RequestEvidenceVersion
	seenCommands, seenAudits := map[string]bool{}, map[string]bool{}
	sources := map[string]Evidence{}
	for i, o := range doc.Observations {
		var failure *Failure
		for j := range b.result.Failures {
			if b.result.Failures[j].Command == o.Command {
				failure = &b.result.Failures[j]
				break
			}
		}
		// The reviewed correlation identifies the serial scale-up ACK attempt.
		// A cleanup request with the same resource path is not interchangeable.
		if failure == nil || failure.Stage != "ack" || failure.DeploymentUID == "" || o.ObjectUID != failure.DeploymentUID || seenCommands[o.Command] || seenAudits[o.AuditID] {
			return fmt.Errorf("request evidence observation %d does not uniquely match a failed ACK command and Deployment UID", i+1)
		}
		if !auditIDPattern.MatchString(o.AuditID) || o.Verb != "update" || !scalePathPattern.MatchString(o.Path) || o.Stage != "ResponseComplete" || o.ResponseCode < 100 || o.ResponseCode > 599 || o.Correlation != "path-uid-time-serial" ||
			o.ReceivedAt.IsZero() || o.CompletedAt.IsZero() || o.CompletedAt.Before(o.ReceivedAt) || o.CompletedAt.After(b.result.GeneratedAt) || (o.SourceLine != nil && *o.SourceLine <= 0) {
			return fmt.Errorf("invalid request evidence observation %d: require canonical scale update, ResponseComplete, HTTP code, ordered timestamps and reviewer correlation", i+1)
		}
		seenCommands[o.Command], seenAudits[o.AuditID] = true, true
		want, err := hex.DecodeString(o.SourceSHA256)
		if err != nil || len(want) != sha256.Size || !filepath.IsLocal(o.SourcePath) || filepath.ToSlash(filepath.Clean(o.SourcePath)) != o.SourcePath || strings.ContainsAny(o.SourcePath, "\\\x00\r\n") {
			return fmt.Errorf("invalid request evidence source path or SHA256 at observation %d", i+1)
		}
		source, hashed := sources[o.SourcePath]
		if !hashed {
			// Reject parent aliases and leaf symlinks even when they resolve within
			// the root. Stream opaque bytes; do not parse or embed raw log content.
			for p := o.SourcePath; p != "."; p = filepath.Dir(p) {
				info, err := root.Lstat(p)
				if err != nil || (p == o.SourcePath && !info.Mode().IsRegular()) || (p != o.SourcePath && !info.IsDir()) {
					return fmt.Errorf("request evidence source is unavailable or not a regular unaliased file")
				}
			}
			file, err := root.Open(o.SourcePath)
			if err != nil {
				return fmt.Errorf("request evidence source unavailable")
			}
			const maxSourceBytes = 32 << 30
			info, err := file.Stat()
			if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxSourceBytes {
				file.Close()
				return fmt.Errorf("request evidence source must contain 1 byte to 32 GiB")
			}
			h := sha256.New()
			buffer := make([]byte, 128<<10)
			var count int64
			for {
				if err = b.ctx.Err(); err != nil {
					break
				}
				var n int
				n, err = file.Read(buffer)
				count += int64(n)
				_, _ = h.Write(buffer[:n])
				if count > info.Size() || err != nil {
					break
				}
			}
			closeErr := file.Close()
			if b.ctx.Err() != nil {
				return b.ctx.Err()
			}
			if err != io.EOF || closeErr != nil || count != info.Size() {
				return fmt.Errorf("request evidence source changed or could not be hashed")
			}
			actual := hex.EncodeToString(h.Sum(nil))
			if actual != strings.ToLower(o.SourceSHA256) {
				return fmt.Errorf("request evidence source SHA256 mismatch")
			}
			sourceRef := b.evidence(filepath.Join(root.Name(), o.SourcePath), actual)
			source = Evidence{ID: sourceRef, SHA256: actual}
			sources[o.SourcePath] = source
		}
		if source.SHA256 != strings.ToLower(o.SourceSHA256) {
			return fmt.Errorf("conflicting request evidence source SHA256")
		}
		refs := []string{ref, source.ID}
		duration := o.CompletedAt.Sub(o.ReceivedAt).Seconds()
		failure.ServerObservation = &ServerObservation{ResponseCode: o.ResponseCode, DurationSeconds: duration, ReceivedAt: o.ReceivedAt, CompletedAt: o.CompletedAt, AuditID: o.AuditID, Correlation: o.Correlation, Provenance: "reviewer-correlated source-hash-verified", SourceLine: o.SourceLine, EvidenceRefs: refs}
		failure.EvidenceRefs = unique(append(failure.EvidenceRefs, refs...))
		if o.ResponseCode >= 200 && o.ResponseCode < 300 {
			failure.ApplicationOutcome = "server_success_observed"
		}
		statement := fmt.Sprintf("Matching server audit reports HTTP %d in %s; client ACK completion remains unobserved", o.ResponseCode, failureDuration(duration))
		if failure.Status == "client_timeout" && failure.DeadlineSeconds != nil {
			statement = fmt.Sprintf("Matching server audit reports HTTP %d in %s; client ACK not observed before %s", o.ResponseCode, failureDuration(duration), failureDuration(*failure.DeadlineSeconds))
		}
		statement += ". Reviewer-correlated source-hash-verified evidence: path/UID/time/serial correlation, not shared client request-ID correlation. Summary fields and sourceLine are reviewer-declared, not independently extracted from the source. Server duration uses only audit timestamps; no cross-clock latency subtraction or client receipt inferred. Workload gate remains failed; cause and service availability impact remain unknown."
		// Enrich the existing descriptive failure rather than leaving a stale
		// application-outcome-unknown claim next to a correlated success report.
		for j := range b.result.Findings {
			finding := &b.result.Findings[j]
			if finding.Scope == "recorded workload gate" && strings.HasPrefix(finding.Statement, o.Command+":") {
				finding.Statement = strings.TrimSuffix(finding.Statement, " Application outcome and cause unknown; no server rejection or service availability failure established.") + " " + statement
				finding.Evidence = failure.EvidenceRefs
			}
		}
		for j := range b.result.CarryForward {
			carry := &b.result.CarryForward[j]
			if carry.ID == "diagnose-ack-"+o.Command {
				carry.EvidenceRefs = failure.EvidenceRefs
				carry.BlockedBy = []string{"cause unknown; audit correlation is reviewer-declared, not shared client request-ID correlation"}
			}
		}
	}
	return b.ctx.Err()
}

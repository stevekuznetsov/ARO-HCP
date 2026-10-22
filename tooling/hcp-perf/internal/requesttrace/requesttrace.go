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

// Package requesttrace records bounded, private HTTP evidence for workload clients.
// It does not observe client-side rate-limiter waits, body reads, or WATCH events.
package requesttrace

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

const MaxBytes = 10 << 20

// ErrIncomplete identifies invalid harness evidence, not an HCP failure.
// Transport outcomes are never replaced by recording errors; callers must Close.
var ErrIncomplete = errors.New("request telemetry incomplete (harness evidence invalid, not an HCP failure)")

// Observation measures a single RoundTrip, including retries internal to client-go.
// Duration uses the monotonic clock and ends before logging or reading the body.
// Namespace is the caller-supplied client scope, not a verified server identity.
type Observation struct {
	Version             string        `json:"version"`
	Namespace           string        `json:"namespace"`
	Method              string        `json:"method"`
	Path                string        `json:"path"`
	Watch               bool          `json:"watch,omitempty"` // Only the watch boolean is extracted; query values are never retained.
	Started             time.Time     `json:"started"`
	Duration            time.Duration `json:"durationNanoseconds"`
	Kind                string        `json:"kind"` // headerReceived or transportError; never body completion.
	StatusCode          int           `json:"statusCode,omitempty"`
	ErrClass            string        `json:"errClass,omitempty"` // timeout, canceled, transport.
	APFFlowSchemaUID    string        `json:"apfFlowSchemaUID,omitempty"`
	APFPriorityLevelUID string        `json:"apfPriorityLevelUID,omitempty"`
	AuditID             string        `json:"auditID,omitempty"`
	RetryAfter          string        `json:"retryAfter,omitempty"` // Validated raw delta-seconds or HTTP-date, at most 64 bytes.
}

// Log appends and syncs one NDJSON record per observed request under a mutex.
// There is no buffered loss window or background goroutine. Sync cost is outside
// Duration but still contributes to workload wall time. Stop clients before Close.
type Log struct {
	mu     sync.Mutex
	file   *os.File
	size   int
	err    error
	closed bool
}

// Open exclusively creates a private file; its parent must already exist.
// An empty path disables tracing (nil Log methods are safe).
func Open(path string) (*Log, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0600)
	if err != nil {
		return nil, errors.Join(ErrIncomplete, errors.New("cannot create request log"))
	}
	return &Log{file: f}, nil
}

// Close is idempotent and reports any recording, sync, or close failure. Call it
// even after cancellation, and join its error with the workload error.
func (l *Log) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.closed = true
		if err := errors.Join(l.file.Sync(), l.file.Close()); err != nil {
			l.err = errors.Join(l.err, ErrIncomplete, errors.New("cannot sync or close request log"))
		}
	}
	return l.err
}

// WrapTransport returns a rest.Config.WrapTransport callback scoped to namespace.
// Use config.Wrap(log.WrapTransport(namespace)) to preserve existing wrappers.
// No request headers are added, including Audit-ID: only the server's ID is saved.
func (l *Log) WrapTransport(namespace string) func(http.RoundTripper) http.RoundTripper {
	return func(next http.RoundTripper) http.RoundTripper {
		if l == nil {
			return next
		}
		return &transport{next: next, log: l, namespace: namespace}
	}
}

type transport struct {
	next      http.RoundTripper
	log       *Log
	namespace string
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !allowed(req, t.namespace) {
		return t.next.RoundTrip(req)
	}
	o := Observation{Version: "v1", Namespace: t.namespace, Method: req.Method, Path: req.URL.Path}
	if req.Method == http.MethodGet {
		o.Watch, _ = strconv.ParseBool(req.URL.Query().Get("watch"))
	}
	o.Started = time.Now()
	resp, err := t.next.RoundTrip(req)
	o.Duration = time.Since(o.Started)
	o.Kind = "transportError"
	if resp != nil {
		o.Kind = "headerReceived"
		o.StatusCode = resp.StatusCode
		o.APFFlowSchemaUID = validUID(resp.Header.Get("X-Kubernetes-PF-FlowSchema-UID"))
		o.APFPriorityLevelUID = validUID(resp.Header.Get("X-Kubernetes-PF-PriorityLevel-UID"))
		o.AuditID = validUID(resp.Header.Get("Audit-ID"))
		o.RetryAfter = validRetryAfter(resp.Header.Get("Retry-After"))
	}
	if err != nil {
		var timeout net.Error
		switch {
		case errors.Is(err, context.Canceled):
			o.ErrClass = "canceled"
		case errors.Is(err, context.DeadlineExceeded), errors.As(err, &timeout) && timeout.Timeout():
			o.ErrClass = "timeout"
		default:
			o.ErrClass = "transport"
		}
	}
	t.log.append(o)
	return resp, err
}

func (l *Log) append(o Observation) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return
	}
	if l.closed {
		l.err = errors.Join(ErrIncomplete, errors.New("request arrived after log closed"))
		return
	}
	data, err := json.Marshal(o)
	if err == nil {
		data = append(data, '\n')
		if l.size+len(data) > MaxBytes {
			l.err = errors.Join(ErrIncomplete, errors.New("request log reached 10 MiB limit"))
			return
		}
		var n int
		n, err = l.file.Write(data)
		l.size += n
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
		if err == nil {
			err = l.file.Sync()
		}
	}
	if err != nil {
		l.err = errors.Join(ErrIncomplete, errors.New("cannot append or sync request log"))
	}
}

// Exact resource shapes exclude discovery, credentials, logs, exec/proxy, and
// arbitrary subresources. Never persist RawQuery, host, userinfo, or fragments.
func allowed(req *http.Request, namespace string) bool {
	switch req.Method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return false
	}
	if req.URL == nil || len(validation.IsDNS1123Label(namespace)) != 0 {
		return false
	}
	p := req.URL.Path
	if p == "/metrics" {
		return req.Method == http.MethodGet
	}
	resource := ""
	switch {
	case strings.HasPrefix(p, "/api/v1/namespaces/"+namespace+"/"):
		resource = strings.TrimPrefix(p, "/api/v1/namespaces/"+namespace+"/")
		if strings.Split(resource, "/")[0] != "pods" {
			return false
		}
	case strings.HasPrefix(p, "/apis/apps/v1/namespaces/"+namespace+"/"):
		resource = strings.TrimPrefix(p, "/apis/apps/v1/namespaces/"+namespace+"/")
		if r := strings.Split(resource, "/")[0]; r != "deployments" && r != "replicasets" {
			return false
		}
	default:
		return false
	}
	parts := strings.Split(resource, "/")
	if len(parts) > 3 || len(parts) >= 2 && len(validation.IsDNS1123Subdomain(parts[1])) != 0 {
		return false
	}
	return len(parts) < 3 || parts[0] != "pods" && parts[2] == "scale"
}

var uidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func validUID(raw string) string {
	if len(raw) == 36 && uidPattern.MatchString(raw) {
		return raw
	}
	return ""
}

func validRetryAfter(raw string) string {
	if len(raw) == 0 || len(raw) > 64 {
		return ""
	}
	if _, err := strconv.ParseUint(raw, 10, 32); err == nil {
		return raw
	}
	if _, err := http.ParseTime(raw); err == nil {
		return raw
	}
	return ""
}

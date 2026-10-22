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

// Package provision talks directly to a trusted personal RP, without ARM tokens,
// Azure infrastructure creation, or the test framework. Callers supply the total
// operation deadline. Every HTTP exchange, including reading its body, is bounded
// to 30 seconds. A supplied transport is trusted and must honor request contexts.
package provision

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const provider = "Microsoft.RedHatOpenShift"
const maxBody = 16 << 20

var (
	uuidPattern     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	clusterPattern  = regexp.MustCompile(`^[a-zA-Z]([-a-zA-Z0-9]{0,52}[a-zA-Z0-9])?$`)
	nodePoolPattern = regexp.MustCompile(`^[a-zA-Z]([-a-zA-Z0-9]{0,13}[a-zA-Z0-9])?$`)
	groupPattern    = regexp.MustCompile(`^[\p{L}\p{N}_.()-]{1,90}$`)
	segmentPattern  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]{0,127}$`)
	codePattern     = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_.-]{0,127}$`)
)

type Config struct {
	Version        string `json:"version"`
	Endpoint       string `json:"endpoint"`
	SubscriptionID string `json:"subscriptionID"`
	TenantID       string `json:"tenantID"`
	ObjectID       string `json:"objectID"`
	Principal      string `json:"principal"`
	APIVersion     string `json:"apiVersion"`
}

type Resource struct {
	ResourceGroup string `json:"resourceGroup"`
	Cluster       string `json:"cluster"`
	NodePool      string `json:"nodePool,omitempty"`
}

// Result deliberately contains no response bodies, payloads, or credentials.
// An empty FinalState means completion has not been established. OperationURL
// survives cancellation; no method rolls back an accepted request.
type Result struct {
	Version        string `json:"version"`
	ResourceID     string `json:"resourceID"`
	RequestURL     string `json:"requestURL"`
	Method         string `json:"method"`
	OperationURL   string `json:"operationURL,omitempty"`
	ResultURL      string `json:"resultURL,omitempty"`
	LastState      string `json:"lastState,omitempty"`
	FinalState     string `json:"finalState,omitempty"`
	HTTPStatus     int    `json:"httpStatus,omitempty"`
	ErrorCode      string `json:"errorCode,omitempty"`
	OutputDir      string `json:"outputDir,omitempty"`
	CredentialFile string `json:"credentialFile,omitempty"`
}

type Client struct {
	config   Config
	endpoint *url.URL
	http     *http.Client
}

// New accepts HTTPS origins or HTTP on localhost/literal loopback only. Endpoint
// is an explicit trust decision: identity headers are sent to that origin.
// The supplied client is copied, redirects are rejected, and cookies disabled.
func New(config Config, supplied *http.Client) (*Client, error) {
	if config.Version != "v1" {
		return nil, errors.New("config version must be v1")
	}
	if config.APIVersion != "2024-06-10-preview" && config.APIVersion != "2025-12-23-preview" {
		return nil, errors.New("unsupported RP API version")
	}
	for _, id := range []string{config.SubscriptionID, config.TenantID, config.ObjectID} {
		if !uuidPattern.MatchString(id) {
			return nil, errors.New("subscriptionID, tenantID and objectID must be UUIDs")
		}
	}
	if strings.TrimSpace(config.Principal) == "" || len(config.Principal) > 256 {
		return nil, errors.New("principal must contain 1-256 printable ASCII characters")
	}
	for _, ch := range config.Principal {
		if ch < 32 || ch > 126 {
			return nil, errors.New("principal must contain printable ASCII characters")
		}
	}
	u, err := url.Parse(config.Endpoint)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(config.Endpoint, "#") || (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return nil, errors.New("endpoint must be an absolute origin without userinfo, path, query or fragment")
	}
	loopback := strings.EqualFold(u.Hostname(), "localhost") || net.ParseIP(u.Hostname()).IsLoopback()
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return nil, errors.New("endpoint requires HTTPS except on localhost or literal loopback")
	}
	u.Path = ""
	client := http.Client{}
	if supplied != nil {
		client = *supplied
	}
	client.Jar = nil
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if client.Timeout == 0 || client.Timeout > 30*time.Second {
		client.Timeout = 30 * time.Second
	}
	return &Client{config: config, endpoint: u, http: &client}, nil
}

func (c *Client) resourceID(ref Resource) (string, error) {
	if !groupPattern.MatchString(ref.ResourceGroup) || strings.HasSuffix(ref.ResourceGroup, ".") {
		return "", errors.New("invalid ARM resource group name")
	}
	// These limits match internal/validation/validators.go, not generic ARM names.
	if !clusterPattern.MatchString(ref.Cluster) {
		return "", errors.New("cluster name must be an RFC 1035 label of 1-54 characters starting with a letter")
	}
	id := "/subscriptions/" + c.config.SubscriptionID + "/resourceGroups/" + ref.ResourceGroup + "/providers/" + provider + "/hcpOpenShiftClusters/" + ref.Cluster
	if ref.NodePool != "" {
		if !nodePoolPattern.MatchString(ref.NodePool) {
			return "", errors.New("node pool name must be an RFC 1035 label of 1-15 characters starting with a letter")
		}
		id += "/nodePools/" + ref.NodePool
	}
	return id, nil
}

func (c *Client) resourceURL(id, version string) string {
	u := *c.endpoint
	u.Path = id
	u.RawQuery = url.Values{"api-version": {version}}.Encode()
	return u.String()
}

// Only the RP's two location-scoped operation routes are followable. In
// particular, Location is never a generic GET capability or a credential action.
func (c *Client) operationURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Opaque != "" || u.Fragment != "" || strings.Contains(raw, "#") || u.RawPath != "" || strings.Contains(u.EscapedPath(), "%") {
		return "", errors.New("unsafe operation URL")
	}
	if !u.IsAbs() && (u.Host != "" || !strings.HasPrefix(u.Path, "/")) {
		return "", errors.New("operation URL must be absolute or root relative")
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return "", errors.New("operation URL cannot contain traversal segments")
		}
	}
	u = c.endpoint.ResolveReference(u)
	if u.Scheme != c.endpoint.Scheme || !strings.EqualFold(u.Host, c.endpoint.Host) {
		return "", errors.New("operation URL must use the configured origin")
	}
	p := strings.Split(u.Path, "/")
	if len(p) != 9 || p[0] != "" || !strings.EqualFold(p[1], "subscriptions") || !strings.EqualFold(p[2], c.config.SubscriptionID) || !strings.EqualFold(p[3], "providers") || !strings.EqualFold(p[4], provider) || !strings.EqualFold(p[5], "locations") || !segmentPattern.MatchString(p[6]) || (!strings.EqualFold(p[7], "hcpOperationStatuses") && !strings.EqualFold(p[7], "hcpOperationResults")) || !segmentPattern.MatchString(p[8]) {
		return "", errors.New("operation URL must target this subscription's RP operation status or result route")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(q) > 1 || (len(q) == 1 && (len(q["api-version"]) != 1 || q.Get("api-version") != c.config.APIVersion)) {
		return "", errors.New("operation URL has an unexpected query")
	}
	u.RawQuery = url.Values{"api-version": {c.config.APIVersion}}.Encode()
	return u.String(), nil
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errors.New("generate request UUID")
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

func (c *Client) request(ctx context.Context, method, target string, payload []byte, a *artifacts) (response, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r := response{}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(payload))
	if err != nil {
		return r, errors.New("construct RP request")
	}
	requestID, err := newUUID()
	if err != nil {
		return r, err
	}
	correlationID, err := newUUID()
	if err != nil {
		return r, err
	}
	req.Header.Set("X-Ms-Client-Request-Id", requestID)
	req.Header.Set("X-Ms-Correlation-Request-Id", correlationID)
	req.Header.Set("X-Ms-Home-Tenant-Id", c.config.TenantID)
	req.Header.Set("X-Ms-Client-Object-Id", c.config.ObjectID)
	req.Header.Set("X-Ms-Client-Principal-Name", c.config.Principal)
	req.Header.Set("Referer", target)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "hcp-perf/v1")
	if method == http.MethodPut {
		systemData, _ := json.Marshal(map[string]string{"createdBy": c.config.Principal, "createdByType": "Application", "createdAt": time.Now().UTC().Format(time.RFC3339)})
		req.Header.Set("X-Ms-Arm-Resource-System-Data", string(systemData))
		req.Header.Set("X-Ms-Identity-Url", "https://dummyhost.identity.azure.net")
		req.Header.Set("Content-Type", "application/json")
	}
	if method == http.MethodPatch {
		systemData, _ := json.Marshal(map[string]string{"lastModifiedBy": c.config.Principal, "lastModifiedByType": "Application", "lastModifiedAt": time.Now().UTC().Format(time.RFC3339)})
		req.Header.Set("X-Ms-Arm-Resource-System-Data", string(systemData))
		req.Header.Set("Content-Type", "application/json")
	}
	if err := a.event(event{Method: method, URL: target, RequestID: requestID, CorrelationID: correlationID}); err != nil {
		return r, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// Transport errors can include URLs, headers, and arbitrary secret text.
		if ctx.Err() != nil {
			return r, ctx.Err()
		}
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			return r, context.DeadlineExceeded
		}
		return r, errors.New("RP HTTP transport failed")
	}
	defer resp.Body.Close()
	r.status, r.header = resp.StatusCode, resp.Header
	r.body, err = io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		if ctx.Err() != nil {
			return r, ctx.Err()
		}
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			return r, context.DeadlineExceeded
		}
		return r, errors.New("read RP response failed")
	}
	if len(r.body) > maxBody {
		return r, errors.New("RP response exceeds 16 MiB")
	}
	return r, nil
}

func httpError(r response) error {
	return fmt.Errorf("RP HTTP %d (code %s); response details are private artifacts when available", r.status, errorCode(r))
}

func errorCode(r response) string {
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(r.body, &body)
	code := body.Error.Code
	if code == "" {
		code = r.header.Get("X-Ms-Error-Code")
	}
	if !codePattern.MatchString(code) {
		return "Unknown"
	}
	return code
}

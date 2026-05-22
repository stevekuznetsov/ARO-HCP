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

// Package agent implements LLM-driven test failure analysis using the
// GitHub Copilot SDK. It wraps the Copilot CLI process and provides a
// structured session interface for analysis workflows.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/go-logr/logr"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

const (
	// CopilotAuthModeLoggedIn uses the developer's GitHub CLI session.
	CopilotAuthModeLoggedIn = "logged-in"
	// CopilotAuthModeToken reads a GitHub token from a file.
	CopilotAuthModeToken = "token"
	// CopilotAuthModeBYOK uses an Azure Entra token against a model endpoint.
	CopilotAuthModeBYOK = "byok"
)

// AgentConfig configures the agent's auth and model settings.
// This is the superset of configuration options — callers populate
// only the fields relevant to their auth mode.
type AgentConfig struct {
	// AuthMode is one of "logged-in", "token", or "byok".
	AuthMode string

	// GitHubTokenFile is the path to a file containing a GitHub token (token mode).
	GitHubTokenFile string

	// ModelEndpoint is the Azure AI Foundry endpoint URL (byok mode).
	ModelEndpoint string

	// ModelDeployment is the model deployment name (byok mode).
	ModelDeployment string

	// AzureCredential is used to acquire Entra tokens for BYOK sessions.
	AzureCredential azcore.TokenCredential

	// Model overrides the default model for Copilot sessions.
	Model string

	// MaxRounds is the maximum number of tool-call rounds per session.
	MaxRounds int
}

// CopilotClient wraps a copilot.Client that manages the Copilot CLI process.
// One instance is created per process lifetime.
type CopilotClient struct {
	inner *copilot.Client
	cfg   *AgentConfig
}

// NewCopilotClient creates a CopilotClient configured for the given auth mode.
// The underlying CLI process is started lazily on first session creation
// (AutoStart defaults to true).
func NewCopilotClient(cfg *AgentConfig) (*CopilotClient, error) {
	clientOpts := &copilot.ClientOptions{}

	switch cfg.AuthMode {
	case CopilotAuthModeLoggedIn, "":
		// UseLoggedInUser defaults to true; no extra config needed.
	case CopilotAuthModeToken:
		token, err := os.ReadFile(cfg.GitHubTokenFile)
		if err != nil {
			return nil, fmt.Errorf("reading GitHub token from %s: %w", cfg.GitHubTokenFile, err)
		}
		clientOpts.GitHubToken = string(token)
	case CopilotAuthModeBYOK:
		// BYOK auth is configured per-session via ProviderConfig, not at the
		// client level. Disable logged-in auth so the CLI doesn't try to
		// authenticate with GitHub.
		clientOpts.UseLoggedInUser = copilot.Bool(false)
	default:
		return nil, fmt.Errorf("unsupported copilot auth mode: %q", cfg.AuthMode)
	}

	return &CopilotClient{
		inner: copilot.NewClient(clientOpts),
		cfg:   cfg,
	}, nil
}

// Stop shuts down the Copilot CLI process and releases all resources.
func (c *CopilotClient) Stop() error {
	return c.inner.Stop()
}

// SessionConfig configures a new analysis session.
type SessionConfig struct {
	// WorkingDirectory is the workspace root for the Copilot session.
	// Tool operations (read_file, grep, bash, glob) are relative to this directory.
	WorkingDirectory string

	// SystemMessage configures system prompt customization.
	SystemMessage *copilot.SystemMessageConfig

	// Tools are custom tools (e.g. kusto_query) registered on this session.
	Tools []copilot.Tool

	// Model overrides the default model for this session.
	Model string
}

// Session wraps a copilot.Session for a single analysis run.
type Session struct {
	inner  *copilot.Session
	client *CopilotClient
	logger logr.Logger
}

// CreateSession creates a new Copilot session for an analysis run.
func (c *CopilotClient) CreateSession(ctx context.Context, logger logr.Logger, cfg SessionConfig) (*Session, error) {
	sessionCfg := &copilot.SessionConfig{
		Model:               cfg.Model,
		SystemMessage:       cfg.SystemMessage,
		Tools:               cfg.Tools,
		WorkingDirectory:    cfg.WorkingDirectory,
		OnPermissionRequest: copilot.PermissionHandler.ApproveAll,
		// Disable config discovery — we don't want to pick up .mcp.json or
		// AGENTS.md from the analysis workspace.
		EnableConfigDiscovery: false,
	}

	// Apply the configured model if the caller didn't override.
	if sessionCfg.Model == "" && c.cfg.Model != "" {
		sessionCfg.Model = c.cfg.Model
	}

	// Configure BYOK provider if applicable.
	if c.cfg.AuthMode == CopilotAuthModeBYOK {
		token, err := c.cfg.AzureCredential.GetToken(ctx, policy.TokenRequestOptions{
			Scopes: []string{"https://cognitiveservices.azure.com/.default"},
		})
		if err != nil {
			return nil, fmt.Errorf("acquiring Entra token for BYOK copilot session: %w", err)
		}
		sessionCfg.Provider = &copilot.ProviderConfig{
			Type:        "azure",
			BaseURL:     c.cfg.ModelEndpoint,
			BearerToken: token.Token,
		}
		if sessionCfg.Model == "" {
			sessionCfg.Model = c.cfg.ModelDeployment
		}
	}

	session, err := c.inner.CreateSession(ctx, sessionCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create copilot session: %w", err)
	}

	logger = logger.WithValues("sessionID", session.SessionID)
	logger.Info("Created Copilot session.")

	return &Session{
		inner:  session,
		client: c,
		logger: logger,
	}, nil
}

// SessionID returns the unique identifier for this session.
func (s *Session) SessionID() string {
	return s.inner.SessionID
}

// SendAndWait sends a prompt to the session and blocks until the agent is idle.
// If ctx is cancelled, the in-flight work is aborted.
// Returns the final assistant message content.
func (s *Session) SendAndWait(ctx context.Context, prompt string) (string, error) {
	s.logger.Info("Sending message to Copilot session.", "promptLength", len(prompt))

	// The SDK applies a 60s default timeout when the context has no deadline.
	// Analysis turns routinely take 10+ minutes, so set a generous deadline
	// to prevent the SDK from timing out prematurely. The caller's context
	// cancellation (via the abort path below) remains the real control.
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Minute)
		defer cancel()
	}

	// Run SendAndWait in a goroutine so we can select on ctx.Done() for abort.
	type result struct {
		event *copilot.SessionEvent
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		ev, err := s.inner.SendAndWait(ctx, copilot.MessageOptions{
			Prompt: prompt,
		})
		ch <- result{event: ev, err: err}
	}()

	select {
	case <-ctx.Done():
		s.logger.Info("Context cancelled, aborting Copilot session.")
		if err := s.inner.Abort(context.Background()); err != nil {
			s.logger.Error(err, "Failed to abort Copilot session.")
		}
		// Wait for SendAndWait to return after abort.
		r := <-ch
		if r.err != nil {
			return "", fmt.Errorf("copilot session aborted: %w", r.err)
		}
		return "", ctx.Err()
	case r := <-ch:
		if r.err != nil {
			return "", fmt.Errorf("copilot session failed: %w", r.err)
		}
		if r.event == nil {
			return "", fmt.Errorf("copilot session returned no response")
		}
		data, ok := r.event.Data.(*copilot.AssistantMessageData)
		if !ok {
			return "", fmt.Errorf("copilot session returned unexpected event type %T", r.event.Data)
		}
		s.logger.Info("Copilot session responded.", "responseLength", len(data.Content))
		return data.Content, nil
	}
}

// Disconnect releases in-memory session resources. Session state is preserved
// on disk and can be resumed later.
func (s *Session) Disconnect() error {
	return s.inner.Disconnect()
}

// Delete permanently removes all session data from disk.
func (s *Session) Delete(ctx context.Context) error {
	return s.client.inner.DeleteSession(ctx, s.inner.SessionID)
}

// SaveConversation writes the session's conversation history to a JSON file
// at the given path. This is best-effort: errors are logged but not returned.
func (s *Session) SaveConversation(ctx context.Context, path string) {
	events, err := s.inner.GetMessages(ctx)
	if err != nil {
		s.logger.Error(err, "Failed to get session messages for conversation dump.")
		return
	}

	data, err := json.MarshalIndent(events, "", "  ")
	if err != nil {
		s.logger.Error(err, "Failed to marshal conversation events.")
		return
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		s.logger.Error(err, "Failed to create directory for conversation dump.", "path", path)
		return
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		s.logger.Error(err, "Failed to write conversation to disk.", "path", path)
		return
	}

	s.logger.Info("Wrote conversation to disk.", "path", path, "size", len(data))
}

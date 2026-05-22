# Upstream Extraction: `agent.Analyze()` Orchestration Function

This document specifies the changes to extract the shared analysis orchestration
loop from both the `hcpctl snapshot analyze` CLI command and the sdp-pipelines
`testCaseAnalysisController` into a single `Analyze()` function in the
`github.com/Azure/ARO-HCP/tooling/hcpctl/pkg/agent` package.

## Motivation

Both the CLI (`cmd/snapshot/analyze_cmd.go`) and the downstream controller
(`test_case_analysis_controller.go`) independently implement identical
orchestration logic:

1. Send initial prompt via `BuildInitialPrompt`
2. Parse/validate draft in a correction loop (`validateDraftLoop`)
3. Hydrate proof items with real query results and code excerpts
4. Run review rounds (render markdown, send review prompt, re-validate, re-hydrate)

The two implementations are line-for-line identical (modulo a log message here
and there). Extracting this into the `agent` package eliminates the duplication
and ensures both callers stay in sync as the analysis protocol evolves.

## New file: `tooling/hcpctl/pkg/agent/analyze.go`

```go
package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/go-logr/logr"
)

// AnalyzeOptions configures a single analysis run.
type AnalyzeOptions struct {
	// Manifest is the raw manifest.json content.
	Manifest []byte

	// TestName is the name of the failed test (used for rendering).
	TestName string

	// TestError is the content of test/error.log, or empty.
	TestError string

	// TestOutput is the content of test/output.log, or empty.
	TestOutput string

	// SiblingTests is the content of sibling_tests.json, or empty.
	SiblingTests string

	// DataDir is the root of the structured data directory.
	DataDir string

	// WorktreePaths maps repository names to local filesystem paths.
	WorktreePaths map[string]string

	// KustoCluster is the Kusto cluster URI for hydration share links.
	KustoCluster string

	// KustoDatabase is the Kusto database name.
	KustoDatabase string

	// MaxValidationRounds is the maximum number of parse/validate correction
	// rounds per validate-draft cycle. Zero defaults to 10.
	MaxValidationRounds int

	// ReviewRounds is the number of review passes. Zero defaults to 3.
	ReviewRounds int
}

// AnalyzeResult contains the output of a successful analysis.
type AnalyzeResult struct {
	// HydratedChain is the fully validated and hydrated causal chain.
	HydratedChain *HydratedChain

	// DraftChain is the last validated draft before the final hydration.
	DraftChain *DraftChain
}

// Analyze runs the full agentic analysis loop: initial prompt, validate-draft,
// hydrate, and review rounds. It requires an already-created Session and
// KustoClient. The caller is responsible for session lifecycle (create, save
// conversation, delete/disconnect) and Kusto client lifecycle (create, close).
//
// The function sends the initial prompt, validates and corrects the agent's
// output, hydrates proof items with real query results and code excerpts,
// then runs review rounds where the agent sees its rendered output and can
// refine it.
func Analyze(ctx context.Context, logger logr.Logger, session *Session, kustoClient KustoClient, opts AnalyzeOptions) (*AnalyzeResult, error) {
	maxValidationRounds := opts.MaxValidationRounds
	if maxValidationRounds <= 0 {
		maxValidationRounds = 10
	}
	reviewRounds := opts.ReviewRounds
	if reviewRounds < 0 {
		reviewRounds = 3
	}

	// Phase 1: Send initial prompt.
	logger.Info("Sending initial analysis prompt.")
	prompt := BuildInitialPrompt(string(opts.Manifest), opts.TestError, opts.TestOutput, opts.SiblingTests, opts.DataDir, opts.WorktreePaths)
	output, err := session.SendAndWait(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("agent analysis failed: %w", err)
	}

	// Build validation context.
	validRepos := make(map[string]bool, len(opts.WorktreePaths))
	for repo := range opts.WorktreePaths {
		validRepos[repo] = true
	}
	vc := &ValidationContext{
		ValidRepos:    validRepos,
		WorktreePaths: opts.WorktreePaths,
		DataDir:       opts.DataDir,
		TestError:     opts.TestError,
		TestOutput:    opts.TestOutput,
	}

	// Phase 2: Validate draft loop.
	logger.Info("Validating agent output.")
	draftChain, output, err := ValidateDraftLoop(ctx, logger, session, kustoClient, vc, output, maxValidationRounds)
	if err != nil {
		return nil, err
	}

	// Log the validated draft for exemplar collection.
	if draftJSON, err := json.Marshal(draftChain); err != nil {
		logger.Error(err, "Failed to marshal draft chain to JSON for logging.")
	} else {
		logger.Info("Validated draft chain.", "draft", string(draftJSON))
	}

	// Phase 3: Hydration.
	logger.Info("Hydrating analysis.")
	hydrator := NewHydrator(kustoClient, opts.KustoCluster, opts.KustoDatabase, opts.WorktreePaths, opts.TestError, opts.TestOutput, opts.DataDir)
	hydratedChain, err := hydrator.Hydrate(ctx, draftChain)
	if err != nil {
		return nil, fmt.Errorf("hydration failed: %w", err)
	}
	if err := Validate(hydratedChain); err != nil {
		return nil, fmt.Errorf("hydrated chain validation failed: %w", err)
	}

	// Phase 4: Review rounds.
	for review := 0; review < reviewRounds; review++ {
		logger.Info("Review pass.", "round", review+1)

		rendered := RenderMarkdown(hydratedChain, opts.TestName)
		reviewPrompt := BuildReviewPrompt(rendered)

		output, err = session.SendAndWait(ctx, reviewPrompt)
		if err != nil {
			return nil, fmt.Errorf("agent review failed at round %d: %w", review+1, err)
		}

		draftChain, _, err = ValidateDraftLoop(ctx, logger, session, kustoClient, vc, output, maxValidationRounds)
		if err != nil {
			return nil, err
		}

		hydratedChain, err = hydrator.Hydrate(ctx, draftChain)
		if err != nil {
			return nil, fmt.Errorf("hydration failed after review round %d: %w", review+1, err)
		}
		if err := Validate(hydratedChain); err != nil {
			return nil, fmt.Errorf("hydrated chain validation failed after review round %d: %w", review+1, err)
		}
	}

	return &AnalyzeResult{
		HydratedChain: hydratedChain,
		DraftChain:    draftChain,
	}, nil
}

// ValidateDraftLoop parses and validates the agent's output, sending correction
// feedback for up to maxRounds iterations. It returns the validated draft chain
// and the raw output string (which may have been updated by agent corrections).
func ValidateDraftLoop(
	ctx context.Context,
	logger logr.Logger,
	session *Session,
	kustoClient KustoClient,
	vc *ValidationContext,
	output string,
	maxRounds int,
) (*DraftChain, string, error) {
	var draftChain *DraftChain
	var err error
	for attempt := 0; ; attempt++ {
		draftChain, err = ParseDraftChain(output)
		if err != nil {
			if attempt >= maxRounds {
				return nil, output, fmt.Errorf("failed to parse agent output as draft chain after %d correction rounds: %w", attempt, err)
			}
			logger.Info("Failed to parse agent output as JSON; sending correction to agent.", "attempt", attempt+1, "error", err)
			output, err = session.SendAndWait(ctx, fmt.Sprintf(
				"Your output could not be parsed as valid JSON: %v\n\nPlease re-emit the complete JSON output.", err,
			))
			if err != nil {
				return nil, output, fmt.Errorf("agent correction failed at attempt %d: %w", attempt+1, err)
			}
			continue
		}

		feedback := ValidateDraft(ctx, kustoClient, draftChain, vc)
		if feedback == "" {
			break // all validation passed
		}

		if attempt >= maxRounds {
			logger.Info("Validation still has failures after max correction rounds; proceeding with best-effort.", "attempts", attempt)
			break
		}

		logger.Info("Validation found errors; sending corrections to agent.", "attempt", attempt+1)
		output, err = session.SendAndWait(ctx, feedback)
		if err != nil {
			return nil, output, fmt.Errorf("agent correction failed at attempt %d: %w", attempt+1, err)
		}
	}
	return draftChain, output, nil
}

// BuildReviewPrompt constructs the prompt sent to the agent during review
// rounds, asking it to review and re-emit the analysis.
func BuildReviewPrompt(rendered string) string {
	return fmt.Sprintf(
		"Below is your analysis rendered as a complete document with query results.\n\n"+
			"Review it for:\n"+
			"1. **Narrative coherence** — does each answer directly and completely address its question? "+
			"Does each subsequent question follow naturally from the previous answer? "+
			"Do any answers 'jump' more than one layer down the stack, omitting crucial context?\n"+
			"2. **Evidence quality** — do the query results actually support the answers? "+
			"Are there unexpected results (empty tables, too many rows, irrelevant columns, repetitive output)?\n"+
			"3. **Depth** — have you stopped the chain too early? Could you ask another \"why?\" to get deeper?\n"+
			"4. **Accuracy** — now that you can see the actual query results, do any of your answers need revision?\n\n"+
			"**Important:** The output you produce is the final document shown to readers. "+
			"Do not mention the review process, do not add notes about what you changed or why, "+
			"and do not reference these instructions. The document should read as if it were "+
			"written correctly the first time.\n\n"+
			"Re-emit the complete corrected JSON output (even if no changes are needed). "+
			"---\n\n%s", rendered,
	)
}
```

## Changes to `tooling/hcpctl/cmd/snapshot/analyze_cmd.go`

### Delete

- The local `validateDraftLoop` function (lines 372-419) — now exported as
  `agent.ValidateDraftLoop`.

### Replace orchestration in `run()` (lines 252-338)

Replace from `// Phase 1: Initial analysis.` through `// Phase 5: Write output.`
with:

```go
	result, err := agent.Analyze(ctx, logger, session, cachedKustoClient, agent.AnalyzeOptions{
		Manifest:            manifestData,
		TestName:            manifest.TestName,
		TestError:           testError,
		TestOutput:          testOutput,
		SiblingTests:        siblingTests,
		DataDir:             o.dataDir,
		WorktreePaths:       o.worktreePaths,
		KustoCluster:        manifest.KustoCluster,
		KustoDatabase:       manifest.KustoDatabase,
		MaxValidationRounds: o.maxRounds,
		ReviewRounds:        o.reviewRounds,
	})
	if err != nil {
		analysisErr = err
		return analysisErr
	}
	hydratedChain := result.HydratedChain
```

Everything before (session setup, Kusto client creation, workspace setup) and
after (writing analysis.json, analysis.md) remains unchanged.

## Downstream adaptation (sdp-pipelines `test_case_analysis_controller.go`)

### Delete

- The local `validateDraftLoop` function — now `agent.ValidateDraftLoop`.

### Replace orchestration in `runAnalysis()` (phases 2-4)

Replace from `// Phase 2: Agentic analysis.` through the end of the review loop
with:

```go
	result, err := agent.Analyze(ctx, logger, session, cachedKustoClient, agent.AnalyzeOptions{
		Manifest:      manifest,
		TestName:      testName,
		TestError:     testError,
		TestOutput:    testOutput,
		SiblingTests:  siblingTests,
		DataDir:       gatherResult.DataDir,
		WorktreePaths: worktreePaths,
		KustoCluster:  gatherResult.Manifest.KustoCluster,
		KustoDatabase: gatherResult.Manifest.KustoDatabase,
	})
	if err != nil {
		return err
	}
	hydratedChain := result.HydratedChain
```

Everything before (Phase 1: data gathering, worktree setup, Kusto client
creation, session creation with defer) and after (Phase 5: persistence) remains
unchanged. The `copilot` import is no longer needed in the controller since the
session config is the only thing that referenced it, and the `kustoTool` and
`systemMessage` setup moves above the `Analyze` call (they're still needed for
session creation).

Wait — the session creation still happens in the controller (it owns session
lifecycle), so `copilot.Tool` is still referenced. The controller still needs:

```go
	kustoTool := agent.NewKustoTool(cachedKustoClient)
	systemMessage, err := agent.BuildSystemMessageConfig()
	// ...
	session, err := c.copilotClient.CreateSession(ctx, logger, agent.SessionConfig{
		WorkingDirectory: workspaceDir,
		SystemMessage:    systemMessage,
		Tools:            []copilot.Tool{kustoTool},
	})
```

So the `copilot` import stays. The only things removed from the controller are:
- The `validateDraftLoop` function
- The ~100 lines of orchestration (initial prompt, validation loop, hydration,
  review loop) replaced by the single `agent.Analyze()` call
- The `maxValidationRounds` and `maxReviewRounds` constants (defaults are in
  `Analyze`)

## API surface added to `agent` package

| Symbol | Description |
|---|---|
| `AnalyzeOptions` | Config struct for a single analysis run |
| `AnalyzeResult` | Return struct containing hydrated and draft chains |
| `Analyze(ctx, logger, session, kustoClient, opts)` | Full orchestration loop |
| `ValidateDraftLoop(ctx, logger, session, kustoClient, vc, output, maxRounds)` | Exported validate-draft correction loop |
| `BuildReviewPrompt(rendered)` | Constructs the review prompt string |

## Design decisions

**`Analyze` takes `*Session` and `KustoClient`, not raw credentials.**
The caller owns lifecycle. The CLI creates a temp workspace and cleans it up;
the controller creates a persistent workspace keyed by analysis ID. The CLI
creates a one-shot Kusto client; the controller wraps it with caching. These
differences stay in the callers.

**`ValidateDraftLoop` is exported separately.**
Even though `Analyze` calls it internally, exporting it lets callers who need
custom orchestration (e.g., a future "interactive mode" or a test harness) use
the building block directly.

**`BuildReviewPrompt` is exported separately.**
Same rationale — the prompt text is a policy decision that callers might want to
inspect or override in the future.

**Default values for `MaxValidationRounds` (10) and `ReviewRounds` (3).**
Both callers currently use these values. The CLI exposes them as flags; the
controller hardcodes them. Making them configurable via the options struct with
sensible defaults covers both cases.

## Verification

After implementing this change upstream, both of the following should still work:

```bash
# CLI — exercises Analyze via the analyze subcommand
hcpctl snapshot analyze <data-dir> \
  --aro-hcp ~/code/ARO-HCP \
  --hypershift ~/code/hypershift \
  --maestro ~/code/maestro \
  --clusters-service ~/code/clusters-service

# Unit tests
go test ./tooling/hcpctl/pkg/agent/... -count=1
```

After bumping the dependency in sdp-pipelines, the controller tests should pass:

```bash
go test ./release-dashboard/backend/pkg/controllers/... -count=1 -timeout 60s
```

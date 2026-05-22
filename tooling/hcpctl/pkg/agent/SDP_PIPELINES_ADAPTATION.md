# sdp-pipelines Upstream Adaptation Spec

This document specifies how to adapt sdp-pipelines to import the `gather` and
`agent` packages from upstream ARO-HCP instead of maintaining local copies.

## Principle

Every upstream API symbol that sdp-pipelines will import is exercised directly
by the `hcpctl snapshot gather` and `hcpctl snapshot analyze` CLI commands. Running
those two commands end-to-end proves the upstream API works before sdp-pipelines
ever depends on it.

## Upstream API Surface

### `github.com/Azure/ARO-HCP/tooling/hcpctl/pkg/snapshot`

| Symbol | Exercised by CLI | Used by downstream |
|---|---|---|
| `GatherForTest(ctx, credential, opts)` | `gather` cmd | `gather.snapshotGatherer.Gather()` replacement |
| `GatherForTestOptions{ProwJobURL, TestName, OutputDir, QueryTimeout, Concurrency}` | `gather` cmd | `gather.Options` replacement |
| `GatherForTestResult{ManifestPath, DataDir, Manifest, VerificationReport, StartTime, EndTime, CleanupStartTime, TestSummaries}` | `gather` cmd | `gather.Result` replacement |
| `RecoverTimeWindow(dataDir, testName)` | not directly (cache-reuse path only) | controller cache-reuse path |
| `Manifest` | `analyze` cmd | controller Phase 2 |
| `TestSummary` | `gather` cmd (via GatherForTestResult) | replaced by upstream type |
| `ParseProwURL(url)` | `gather` cmd | no longer needed (GatherForTest calls it) |
| `FetchProwJobData(ctx, prowInfo)` | `gather` cmd | no longer needed |
| `ProwJobInfo` | `gather` cmd | no longer needed |
| `SanitizeTestName(name)` | `gather` cmd | controller output-dir construction |
| `WriteTestLogs(dir, test)` | via `GatherForTest` | no longer needed |
| `ConvertTestResults(results)` | via `GatherForTest` | no longer needed |
| `WriteSiblingTests(dir, summaries)` | via `GatherForTest` | no longer needed |
| `WriteManifest(dir, manifest)` | via `GatherForTest` | no longer needed |

### `github.com/Azure/ARO-HCP/tooling/hcpctl/pkg/agent`

| Symbol | Exercised by CLI | Used by downstream |
|---|---|---|
| `AgentConfig{AuthMode, GitHubTokenFile, ModelEndpoint, ModelDeployment, AzureCredential, Model, MaxRounds}` | `analyze` cmd | replaces `analysisoptions.AnalysisOptions` in `NewCopilotClient` |
| `CopilotAuthModeLoggedIn` / `CopilotAuthModeToken` / `CopilotAuthModeBYOK` | `analyze` cmd (`--auth-mode`) | controller wiring |
| `NewCopilotClient(cfg)` | `analyze` cmd | replaces `agent.NewCopilotClient(opts)` |
| `CopilotClient.Stop()` | `analyze` cmd | controller teardown |
| `CopilotClient.CreateSession(ctx, logger, cfg)` | `analyze` cmd | controller Phase 2 |
| `SessionConfig{WorkingDirectory, SystemMessage, Tools, Model}` | `analyze` cmd | controller Phase 2 |
| `Session.SendAndWait(ctx, prompt)` | `analyze` cmd | controller agentic loop |
| `Session.SaveConversation(ctx, path)` | `analyze` cmd | controller cleanup |
| `Session.Delete(ctx)` | `analyze` cmd | controller cleanup (success) |
| `Session.Disconnect()` | `analyze` cmd | controller cleanup (error) |
| `NewADXKustoClient(credential, clusterURI, database)` | `analyze` cmd | replaces `agent.NewADXKustoClient(credential, clusterURI, database, forceHTTP1)` |
| `ADXKustoClient.Close()` | `analyze` cmd | controller teardown |
| `NewCachingKustoClient(delegate)` | `analyze` cmd | controller Phase 2 |
| `KustoClient` (interface) | `analyze` cmd | controller validateDraftLoop |
| `NewKustoTool(client)` | `analyze` cmd | controller session setup |
| `BuildSystemMessageConfig()` | `analyze` cmd | controller Phase 2 |
| `BuildInitialPrompt(manifest, testError, testOutput, siblingTests, dataDir, worktreePaths)` | `analyze` cmd | controller Phase 2 |
| `ParseDraftChain(output)` | `analyze` cmd | controller validateDraftLoop |
| `DraftChain` | `analyze` cmd | controller validateDraftLoop |
| `ValidateDraft(ctx, client, draft, vc)` | `analyze` cmd | controller validateDraftLoop |
| `ValidationContext{ValidRepos, WorktreePaths, DataDir, TestError, TestOutput}` | `analyze` cmd | controller validateDraftLoop |
| `NewHydrator(client, endpoint, database, worktrees, testError, testOutput, dataDir)` | `analyze` cmd | controller Phase 3 |
| `Hydrator.Hydrate(ctx, draft)` | `analyze` cmd | controller Phase 3 |
| `Validate(hydratedChain)` | `analyze` cmd | controller Phase 3+4 |
| `HydratedChain` | `analyze` cmd | controller Phase 3+4+5 |
| `RenderMarkdown(chain, testName)` | `analyze` cmd | controller Phase 4 |
| `BuildSummaryPrompt(analyses)` | not exercised (job-level only) | job analysis controller |

## Changes to sdp-pipelines

### 1. Delete the `gather` package entirely

**Delete:** `backend/pkg/gather/` (both `gather.go` and `sibling_tests.go`)

**Rationale:** All functionality is now in `snapshot.GatherForTest`. The
`gather.Gatherer` interface, `gather.Options`, `gather.Result`, and
`gather.TestSummary` types are replaced by their upstream equivalents.

### 2. Delete the `agent` package entirely

**Delete:** `backend/pkg/agent/` (all files: `agent.go`, `hydration.go`,
`prompt.go`, `render.go`, `schema.go`, `tools_kusto.go`, `validation.go`,
and `prompts/` directory)

**Rationale:** All functionality is now in
`github.com/Azure/ARO-HCP/tooling/hcpctl/pkg/agent`.

### 3. Delete the `analysisoptions` package entirely

**Delete:** `backend/pkg/analysisoptions/`

**Rationale:** `agent.AgentConfig` replaces `analysisoptions.AnalysisOptions`.
The validation logic and flag binding move into the places that use them (see
§6 below).

### 4. Update `test_case_analysis_controller.go`

#### 4a. Replace imports

```go
// Before:
import (
    "go.goms.io/aro/sdp-pipelines/release-dashboard/backend/pkg/agent"
    "go.goms.io/aro/sdp-pipelines/release-dashboard/backend/pkg/gather"
)

// After:
import (
    "github.com/Azure/ARO-HCP/tooling/hcpctl/pkg/agent"
    "github.com/Azure/ARO-HCP/tooling/hcpctl/pkg/snapshot"
)
```

#### 4b. Replace `gather.Gatherer` field with credential

The controller currently holds a `gather.Gatherer` interface. Replace it with
`azcore.TokenCredential` and call `snapshot.GatherForTest` directly:

```go
// Before (controller struct field):
gatherer gather.Gatherer

// After:
// (credential is already a field on the controller)
// Remove gatherer field entirely.
```

```go
// Before (Phase 1 gather call):
gatherResult, err = c.gatherer.Gather(ctx, gather.Options{
    ProwJobURL:    prowURL,
    TestName:      testName,
    OutputDir:     dataDir,
    WorktreePaths: worktreePaths,
})

// After:
gatherResult, err := snapshot.GatherForTest(ctx, c.credential, snapshot.GatherForTestOptions{
    ProwJobURL: prowURL,
    TestName:   testName,
    OutputDir:  dataDir,
})
```

Note: `WorktreePaths` was on `gather.Options` but was never used by
`gather.snapshotGatherer.Gather()` — it was vestigial. Remove it.

#### 4c. Replace `gather.Result` with `snapshot.GatherForTestResult`

```go
// Before:
var gatherResult *gather.Result

// After:
var gatherResult *snapshot.GatherForTestResult
```

The field names are identical: `ManifestPath`, `DataDir`, `Manifest`,
`VerificationReport`, `StartTime`, `EndTime`, `CleanupStartTime`,
`TestSummaries`.

#### 4d. Replace `gather.RecoverTimeWindow`

```go
// Before:
gatherResult.StartTime, gatherResult.EndTime, gatherResult.CleanupStartTime =
    gather.RecoverTimeWindow(dataDir, testName, logger)

// After:
gatherResult.StartTime, gatherResult.EndTime, gatherResult.CleanupStartTime =
    snapshot.RecoverTimeWindow(dataDir, testName)
```

Note: the upstream `RecoverTimeWindow` does not take a logger parameter. The
downstream version logged parse errors; the upstream silently returns zero times.

#### 4e. Drop `forceHTTP1` from `NewADXKustoClient`

```go
// Before:
kustoClient, err := agent.NewADXKustoClient(
    c.credential,
    gatherResult.Manifest.KustoCluster,
    gatherResult.Manifest.KustoDatabase,
    c.forceHTTP1,
)

// After:
kustoClient, err := agent.NewADXKustoClient(
    c.credential,
    gatherResult.Manifest.KustoCluster,
    gatherResult.Manifest.KustoDatabase,
)
```

Remove the `forceHTTP1` field from the controller struct and from
`NewTestCaseAnalysis` parameters.

#### 4f. Replace `agent.CopilotClient` construction

The controller currently receives a pre-built `*agent.CopilotClient`. The
upstream `NewCopilotClient` now takes `*agent.AgentConfig` instead of
`*analysisoptions.AnalysisOptions`. Update the call site in `run/options.go`,
`run_once/options.go`, and `sync_once/options.go`:

```go
// Before:
copilotClient, err := agent.NewCopilotClient(opts.AnalysisOptions)

// After:
copilotClient, err := agent.NewCopilotClient(&agent.AgentConfig{
    AuthMode:        opts.CopilotAuthMode,
    GitHubTokenFile: opts.CopilotGitHubTokenFile,
    ModelEndpoint:   opts.ModelEndpoint,
    ModelDeployment: opts.ModelDeployment,
    AzureCredential: opts.AzureCredential,
    Model:           opts.CopilotModel,
    MaxRounds:       opts.MaxAgentRounds,
})
```

### 5. Update `NewTestCaseAnalysis` signature

```go
// Before:
func NewTestCaseAnalysis(
    logger logr.Logger,
    dbConn *pgxpool.Pool,
    gatherer gather.Gatherer,
    copilotClient *agent.CopilotClient,
    credential azcore.TokenCredential,
    worktrees *worktree.Manager,
    dataDir string,
    workspaceDir string,
    maxRounds int,
    forceHTTP1 bool,
) *testCaseAnalysisController

// After:
func NewTestCaseAnalysis(
    logger logr.Logger,
    dbConn *pgxpool.Pool,
    copilotClient *agent.CopilotClient,
    credential azcore.TokenCredential,
    worktrees *worktree.Manager,
    dataDir string,
    workspaceDir string,
    maxRounds int,
) *testCaseAnalysisController
```

Remove `gatherer` parameter (controller uses `snapshot.GatherForTest` directly).
Remove `forceHTTP1` parameter (dropped entirely).

### 6. Update `run/options.go`, `run_once/options.go`, `sync_once/options.go`

At each call site:
- Remove `gather.NewGatherer(cred, opts.ForceHTTP1)` — no longer needed.
- Update `NewTestCaseAnalysis` call to match new signature (§5).
- Update `agent.NewCopilotClient` call to pass `agent.AgentConfig` (§4f).
- Inline the validation that was in `analysisoptions` (required fields, auth
  mode switch) into the options validation pipeline of each entry point, or
  create a shared helper.

### 7. Replace `analysisoptions` flag binding

The flags that were in `analysisoptions.BindAnalysisOptions` need to be
preserved somewhere. Options:

**Option A (recommended):** Create a minimal `AnalysisFlags` struct in the
`controllers` or `run` package that holds the raw flag values and produces an
`agent.AgentConfig`. This replaces the three-stage
`Raw → Validated → Completed` pipeline with a simpler adapter.

**Option B:** Inline the flags into each entry point's options struct.

Either way, the flags are:
- `--controllers.analysis.data-dir`
- `--controllers.analysis.worktree-dir`
- `--controllers.analysis.workspace-dir`
- `--controllers.analysis.max-rounds`
- `--controllers.analysis.copilot-auth-mode`
- `--controllers.analysis.copilot-github-token-file`
- `--controllers.analysis.copilot-model`
- `--controllers.analysis.model-endpoint` (BYOK only)
- `--controllers.analysis.model-deployment` (BYOK only)
- ~~`--controllers.analysis.force-http1`~~ (removed)

### 8. Update `go.mod`

Bump the `github.com/Azure/ARO-HCP` dependency to the commit containing the
upstream agent and snapshot packages. The `github.com/github/copilot-sdk/go`
transitive dependency is already pulled in by ARO-HCP's `go.mod`.

## Verification

Running the following commands exercises every upstream API symbol that
sdp-pipelines will import (except `BuildSummaryPrompt` and `RecoverTimeWindow`,
which are job-level and cache-reuse paths respectively):

```bash
# 1. Gather — exercises snapshot.GatherForTest and all its dependencies
hcpctl snapshot gather \
  --url <prow-job-url> \
  --test <test-substring>

# 2. Analyze — exercises all agent.* symbols
hcpctl snapshot analyze <data-dir> \
  --aro-hcp ~/code/ARO-HCP \
  --hypershift ~/code/hypershift \
  --maestro ~/code/maestro \
  --clusters-service ~/code/clusters-service
```

If both commands succeed, the API contract is proven and the downstream can
safely import.

## Symbols NOT exercised by CLI

| Symbol | Reason | Risk |
|---|---|---|
| `RecoverTimeWindow` | Only needed when reusing cached data from a prior run | Low — trivial function, unit-testable |
| `BuildSummaryPrompt` | Job-level summary, not per-test analysis | Low — string builder, no side effects |
| `AgentConfig.MaxRounds` | Stored on config but never read by agent internals; loop control is at caller level | Low — vestigial field |

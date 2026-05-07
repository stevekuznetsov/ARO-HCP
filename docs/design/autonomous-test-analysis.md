# Design: Autonomous E2E Test Failure Analysis System

## Overview

An autonomous agentic system that analyzes failed deployment verification e2e tests in the ARO-HCP project. When a Prow job fails, the system automatically gathers diagnostic data, runs an LLM-powered root-cause analysis, and publishes structured results viewable in the release dashboard — so that by the time a human looks at the failure, a detailed analysis is already waiting.

## Goals

- **Thorough and correct** over fast — rigor is the priority
- **Reproducible** — every claim backed by verifiable Kusto query proof with share URIs
- **Consistent** — all analyses follow an identical structured format
- **Conservative but curious** — no speculation beyond evidence, but creative exploration when pre-gathered data is insufficient
- **Integrated** — analysis is viewable in the existing release dashboard with consistent look and feel

## Architecture

### System Diagram

```
┌─────────────────────────────────────────────────────────────────────┐
│                    Release Dashboard (sdp-pipelines)                │
│                                                                     │
│  Backend (StatefulSet, single replica, PV for git clones)           │
│  ┌─────────────────────┐  ┌──────────────────────────────────────┐  │
│  │ Existing Controllers│  │ New Controllers                      │  │
│  │                     │  │                                      │  │
│  │ sippy_controller ───┼──┼→ job-analysis-controller             │  │
│  │   (writes to        │  │    (bootstrap, summary)              │  │
│  │    prow_results)     │  │                                      │  │
│  │                     │  │  test-case-analysis-controller       │  │
│  │ ev2_status_ctrl     │  │    (per-test data gather + LLM)     │  │
│  │                     │  │                                      │  │
│  │ ...                 │  │  analysis-artifact-cleanup-ctrl      │  │
│  │                     │  │    (worktree/data dir lifecycle)     │  │
│  └─────────────────────┘  └──────────────────────────────────────┘  │
│                                                                     │
│  Frontend (Deployment, serves HTML)                                 │
│  ┌─────────────────────────────────────────────────────────────────┐ │
│  │ Existing routes (unauthenticated)                              │ │
│  │   /releases/... , /dashboard/... , ...                         │ │
│  │                                                                │ │
│  │ New routes (OIDC-protected, Entra + Red Hat SSO)               │ │
│  │   /analysis/{job-name}/{run-id}                                │ │
│  │   /analysis/{job-name}/{run-id}/{test-name}                    │ │
│  │   /auth/login, /auth/callback                                  │ │
│  └─────────────────────────────────────────────────────────────────┘ │
│                                                                     │
│  Postgres (Azure Database for PostgreSQL)                           │
│  ┌─────────────────────────────────────────────────────────────────┐ │
│  │ Existing tables: prow_results, ev2_steps, ...                  │ │
│  │ New tables: analysis_jobs, analysis_tests, chain_links,        │ │
│  │   proof_items, kusto_proofs, log_proofs, code_proofs,          │ │
│  │   analysis_facts, job_summaries                                │ │
│  └─────────────────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────────────┘
         │                              │
         │ Azure AI Foundry             │ Azure Data Explorer (Kusto)
         │ (chat completions,           │ (log queries, share URIs)
         │  workload identity)          │
         │                              │
         │         ┌────────────────────┘
         │         │
         │         │   GCS (public, unauthenticated)
         │         │   (Prow job artifacts)
         │         │
         ▼         ▼
    ┌─────────────────────┐
    │  Git Bare Clones    │
    │  (PV on backend)    │
    │                     │
    │  ARO-HCP            │
    │  clusters-service   │
    │  maestro            │
    │  HyperShift         │
    │                     │
    │  Worktrees keyed by │
    │  (repo, commit SHA) │
    └─────────────────────┘
```

## Components

### 1. Deterministic Data Gatherer

**Location:** ARO-HCP repo, `tooling/triage` (rewrite/extension of existing code)

**Responsibility:** Given a Prow job URL and test name, produce a structured data directory containing all diagnostic data needed for analysis.

**Exported as:** Go packages importable by the analysis engine. Also builds as a CLI for CI validation.

#### Data Gathering Pipeline (per test)

1. Download Prow artifacts from GCS (test logs, `config.yaml`, metadata)
2. Extract resource group and time window from test metadata
3. Query Kusto for all frontend requests in the resource group during the time window
4. Parse ARM request paths → group requests by resource type and name
5. For each mutating request (PUT, POST, PATCH, DELETE), run the full `trace-request` query chain (~25 queries with dependency tracking), discovering downstream identifiers (Maestro bundle IDs, HyperShift object names, etc.)
6. For each resource, query state timelines from backend, Clusters Service, Maestro, and HyperShift using discovered identifiers
7. Run broader contextual queries (container logs, control plane events)
8. Write structured data directory with `manifest.json`

#### Output Data Structure

```
{test-name}/
  manifest.json              # index of all gathered data
  test/
    error.log
    output.log
    metadata.json
  resources/
    {resource-type}/{resource-name}/
      requests.json           # summary: correlation ID, method, path, status, timestamp
      requests/
        {correlation-id}/
          frontend.md         # frontend logs for this request
          trace/              # full trace output (mutating requests only)
            SUMMARY.md
            ...per-query outputs...
      state/
        backend.md            # state evolution over test time window
        clusters-service.md
        maestro.md            # uses discovered bundle IDs
        hypershift.md         # uses discovered object names
  context/                    # broader queries not tied to a specific resource
    frontend_requests.md
    container_logs.md
    control_plane_events.md
```

#### manifest.json Schema

```json
{
  "test_name": "TestNodePoolCreation",
  "time_window": {"start": "2026-05-05T10:00:00Z", "end": "2026-05-05T10:30:00Z"},
  "resource_group": "rg-e2e-12345",
  "resources": [
    {
      "type": "NodePool",
      "name": "test-nodepool-1",
      "requests": [
        {
          "correlation_id": "abc-123",
          "method": "PUT",
          "path": "/subscriptions/.../nodePools/test-nodepool-1",
          "status": 201,
          "timestamp": "2026-05-05T10:01:00Z",
          "has_trace": true
        },
        {
          "correlation_id": "def-456",
          "method": "GET",
          "status": 200,
          "timestamp": "2026-05-05T10:02:00Z",
          "has_trace": false
        }
      ],
      "state_files": ["backend.md", "clusters-service.md", "maestro.md", "hypershift.md"]
    }
  ]
}
```

#### CI Validation

The data gatherer also runs in CI on PRs to validate invariants:
- A deterministic Go post-processor checks that certain queries always return data (e.g., "every mutating request must have frontend logs")
- Catches logging format regressions or query breakage before they affect production analysis

### 2. Analysis Engine Controllers

**Location:** sdp-pipelines repo, within the release dashboard backend

**Three new controllers added to the existing backend process:**

#### job-analysis-controller

**Key:** `(job-name, run-id)`

**Responsibilities:**
- **Discovery:** Queries `prow_results` for failed jobs that have no corresponding `analysis_jobs` row. Enqueues new work.
- **Bootstrap:** Looks up commit SHAs for each component repo from existing dashboard Postgres tables. Ensures bare clones are fetched and worktrees exist at the correct commits. Determines which tests failed (from Prow artifacts).
- **Fan-out:** Creates `analysis_tests` rows for each failed test with `status = pending`, which the test-case-analysis-controller picks up.
- **Summary:** After all per-test analyses reach `status = complete` (or `failed`), runs a lightweight agentic LLM step that reads the per-test chain data and synthesizes a job-level summary (are failures related? common root cause?). Writes the summary to Postgres.

#### test-case-analysis-controller

**Key:** `(job-name, run-id, test-name)`

**Responsibilities:**
- Picks up `analysis_tests` rows with `status = pending`
- Sets `status = running`
- **Phase 1 — Data gathering:** Calls the deterministic data gatherer (imported Go library) to produce the structured data directory for this test
- **Phase 2 — Agentic analysis:** Runs the LLM agent loop (details below). Agent produces a draft chain JSON (claims + KQL strings, no share URIs or result tables)
- **Phase 3 — Hydration:** Deterministic post-processing re-runs each KQL query from the draft chain, generates share URIs via deep-link encoding, captures result tables. Validates schema completeness (every chain link has proof, every Kusto proof has KQL + share URI + table)
- **Phase 4 — Persistence:** Writes the fully hydrated analysis to the normalized Postgres tables
- Sets `status = complete` (or `status = failed` with error message)

**Concurrency:** Bounded worker pool (configurable, e.g., default 4 workers). Multiple tests from the same or different jobs can run in parallel.

#### analysis-artifact-cleanup-controller

**Responsibilities:**
- Manages filesystem lifecycle on the PV
- Cleans up git worktrees when no in-progress analysis references a given `(repo, commit SHA)` — ref-counted
- Cleans up data directories after analysis is complete and results are persisted to Postgres
- Prevents unbounded PV growth

### 3. Agentic LLM Loop

**LLM Client:** `github.com/openai/openai-go` with `github.com/Azure/azure-sdk-for-go/sdk/ai/azopenai` for Azure-native auth via workload identity

**Model:** Configurable at deployment time, served by Azure AI Foundry. Swappable without code changes.

#### Tools

| Tool | Implementation | Purpose |
|------|---------------|---------|
| `bash` | `exec.Command` wrapper | Scoped shell access to data directory + repo worktrees. Agent uses `ls`, `grep`, `cat`, `find`, etc. Read-only enforcement. Timeout per invocation. Output truncation to protect context window. |
| `kusto_query` | Go method imported from `tooling/triage/pkg/kql` | Executes ad-hoc KQL against Azure Data Explorer. Takes KQL string + template parameters. Returns markdown table for agent reasoning. Uses workload identity for auth. |

#### Agent Loop

```go
for rounds := 0; rounds < maxRounds; rounds++ {
    response := llm.ChatCompletion(messages)
    if response.has_tool_calls {
        for each tool_call {
            result := dispatch(tool_call)
            messages.append(tool_result)
        }
    } else {
        // Agent is done — parse draft chain JSON from final message
        break
    }
}
```

- Generous tool-call cap (~50 rounds), otherwise runs to completion
- Each test gets its own independent LLM conversation

#### System Prompt

**Always in context:**

1. **Identity and mission:** "You are analyzing a failed e2e test. Your goal is to produce a rigorous, reproducible root-cause analysis. Every claim must be proven with evidence."

2. **Output schema:** The draft chain JSON format. Agent must produce valid JSON conforming to this schema as its final output.

3. **KQL quality rules:**
   - Queries must be self-contained stories — a reader should understand the intent from the KQL alone
   - Use `| summarize`, `| where`, `| project` to produce focused, unambiguous output
   - To demonstrate absence, use `| summarize count = count()` — never rely on empty result sets
   - Queries will be rendered verbatim alongside their results — write them as if presenting to a colleague

4. **Epistemological rules:**
   - Never assert a cause without proof from logs, Kusto, or source code
   - When pre-gathered data is insufficient, formulate ad-hoc Kusto queries to investigate further
   - When you hit a dead end, state what you looked for and what you didn't find, with proof
   - Do not speculate beyond what the evidence supports. The chain stops where the proof stops.
   - Explore multiple hypotheses before committing to one — check alternatives

5. **Tool descriptions:** Parameters, scoping rules, usage guidance

6. **Exemplar analyses:** Placeholder — to be populated with 2-3 real completed analyses that demonstrate the expected quality, depth, KQL style, and reasoning patterns

**Loaded on-demand via file-read tool:**
- Architecture overview (`references/architecture.md`)
- Service components reference (`references/service-components.md`)
- Tracing methodology with KQL examples (`references/tracing_state.md`)
- `manifest.json` + test logs (read at the start of each analysis)

### 4. Analysis Output Schema

#### Draft Chain JSON (agent output, pre-hydration)

```json
{
  "summary": "NodePool creation timed out because the HyperShift operator was crashlooping due to a nil pointer dereference",
  "notes": "Optional free-form markdown for anything outside the chain",
  "facts": [
    {
      "label": "Cluster ID",
      "value": "abc-12345",
      "kql": "HCPClusters | where ResourceGroup == 'rg-e2e-12345' | project ClusterId"
    },
    {
      "label": "Maestro Bundle ID",
      "value": "bundle-xyz",
      "kql": "MaestroResources | where ClusterId == 'abc-12345' | project BundleId"
    }
  ],
  "chain": [
    {
      "claim": "The test timed out waiting for the NodePool to become Ready",
      "notes": "Optional per-link commentary",
      "proof": [
        {
          "type": "log",
          "source": "test/error.log",
          "excerpt": "timed out waiting for NodePool 'test-np' to reach Ready state"
        }
      ]
    },
    {
      "claim": "The PUT request returned 201 but the async operation never completed",
      "proof": [
        {
          "type": "kusto",
          "kql": "FrontendRequests | where CorrelationId == 'abc-123' | project CorrelationId, Status, Duration"
        },
        {
          "type": "kusto",
          "kql": "BackendOperations | where CorrelationId == 'abc-123' | summarize count = count() by State",
          "note": "Shows the operation was stuck in InProgress state"
        }
      ]
    },
    {
      "claim": "The HyperShift operator was crashlooping with a nil pointer dereference",
      "proof": [
        {
          "type": "kusto",
          "kql": "ContainerLog | where PodName startswith 'hypershift-operator' | where Log contains 'panic' | project Timestamp, Log, RestartCount | order by Timestamp asc"
        },
        {
          "type": "code",
          "repo": "HyperShift",
          "file": "control-plane-operator/controllers/nodepool/nodepool.go",
          "lines": [142, 158],
          "excerpt": "func (r *reconciler) reconcile(...) {\n\t// BUG: np.Status.Conditions can be nil\n\tfor _, c := range np.Status.Conditions {"
        }
      ]
    }
  ],
  "suggestions": [
    "The HyperShift operator should nil-check np.Status.Conditions before iterating",
    "The test should surface operator pod status in its error output for faster triage"
  ]
}
```

#### Hydrated Chain JSON (post-processing output)

Same structure, but every `kusto` proof item gains two additional fields populated deterministically:

```json
{
  "type": "kusto",
  "kql": "ContainerLog | where PodName startswith 'hypershift-operator' | ...",
  "share_uri": "https://dataexplorer.azure.com/clusters/...",
  "table": [
    {"Timestamp": "2026-05-05T10:05:12Z", "Log": "panic: runtime error: invalid memory address", "RestartCount": "12"}
  ],
  "note": "Optional per-proof commentary"
}
```

Facts also gain `share_uri` and `table` fields.

### 5. Postgres Schema

**Ownership:** All analysis tables are owned by the analysis engine controllers. The dashboard frontend reads them.

**Tables (normalized, not JSONB):**

```sql
-- Job-level analysis request
CREATE TABLE analysis_jobs (
    job_name        TEXT NOT NULL,
    run_id          TEXT NOT NULL,
    prow_url        TEXT NOT NULL,
    status          TEXT NOT NULL CHECK (status IN ('pending', 'running', 'complete', 'failed')),
    error           TEXT,
    summary_text    TEXT,            -- job-level summary (populated after all tests done)
    summary_notes   TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (job_name, run_id)
);

-- Commit SHAs per repo for a job
CREATE TABLE analysis_job_commits (
    job_name        TEXT NOT NULL,
    run_id          TEXT NOT NULL,
    repo            TEXT NOT NULL,   -- e.g. 'ARO-HCP', 'clusters-service', 'maestro', 'hypershift'
    commit_sha      TEXT NOT NULL,
    PRIMARY KEY (job_name, run_id, repo),
    FOREIGN KEY (job_name, run_id) REFERENCES analysis_jobs (job_name, run_id)
);

-- Per-test analysis
CREATE TABLE analysis_tests (
    job_name        TEXT NOT NULL,
    run_id          TEXT NOT NULL,
    test_name       TEXT NOT NULL,
    status          TEXT NOT NULL CHECK (status IN ('pending', 'running', 'complete', 'failed')),
    error           TEXT,
    summary         TEXT,            -- one-line summary of root cause
    notes           TEXT,            -- free-form notes from agent
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (job_name, run_id, test_name),
    FOREIGN KEY (job_name, run_id) REFERENCES analysis_jobs (job_name, run_id)
);

-- Discovered facts (context data, not causal claims)
CREATE TABLE analysis_facts (
    id              SERIAL PRIMARY KEY,
    job_name        TEXT NOT NULL,
    run_id          TEXT NOT NULL,
    test_name       TEXT NOT NULL,
    ordinal         INT NOT NULL,
    label           TEXT NOT NULL,   -- e.g. 'Cluster ID'
    value           TEXT NOT NULL,   -- e.g. 'abc-12345'
    kql             TEXT NOT NULL,
    share_uri       TEXT NOT NULL,
    FOREIGN KEY (job_name, run_id, test_name) REFERENCES analysis_tests (job_name, run_id, test_name)
);

-- Result table rows for fact queries
CREATE TABLE analysis_fact_rows (
    fact_id         INT NOT NULL REFERENCES analysis_facts (id),
    row_ordinal     INT NOT NULL,
    data            JSONB NOT NULL,  -- single row as key-value pairs
    PRIMARY KEY (fact_id, row_ordinal)
);

-- Why-chain links (ordered causal claims)
CREATE TABLE analysis_chain_links (
    id              SERIAL PRIMARY KEY,
    job_name        TEXT NOT NULL,
    run_id          TEXT NOT NULL,
    test_name       TEXT NOT NULL,
    ordinal         INT NOT NULL,    -- position in the chain
    claim           TEXT NOT NULL,
    notes           TEXT,            -- optional per-link commentary
    FOREIGN KEY (job_name, run_id, test_name) REFERENCES analysis_tests (job_name, run_id, test_name)
);

-- Proof items (one or more per chain link)
CREATE TABLE analysis_proof_items (
    id              SERIAL PRIMARY KEY,
    chain_link_id   INT NOT NULL REFERENCES analysis_chain_links (id),
    ordinal         INT NOT NULL,
    proof_type      TEXT NOT NULL CHECK (proof_type IN ('kusto', 'log', 'code')),
    note            TEXT             -- optional per-proof commentary
);

-- Kusto proof details
CREATE TABLE analysis_kusto_proofs (
    proof_item_id   INT NOT NULL REFERENCES analysis_proof_items (id) PRIMARY KEY,
    kql             TEXT NOT NULL,
    share_uri       TEXT NOT NULL
);

-- Result table rows for kusto proofs
CREATE TABLE analysis_kusto_proof_rows (
    proof_item_id   INT NOT NULL,
    row_ordinal     INT NOT NULL,
    data            JSONB NOT NULL,  -- single row as key-value pairs
    PRIMARY KEY (proof_item_id, row_ordinal),
    FOREIGN KEY (proof_item_id) REFERENCES analysis_kusto_proofs (proof_item_id)
);

-- Log proof details
CREATE TABLE analysis_log_proofs (
    proof_item_id   INT NOT NULL REFERENCES analysis_proof_items (id) PRIMARY KEY,
    source_path     TEXT NOT NULL,   -- e.g. 'test/error.log'
    excerpt         TEXT NOT NULL
);

-- Code proof details
CREATE TABLE analysis_code_proofs (
    proof_item_id   INT NOT NULL REFERENCES analysis_proof_items (id) PRIMARY KEY,
    repo            TEXT NOT NULL,
    file_path       TEXT NOT NULL,
    line_start      INT NOT NULL,
    line_end        INT NOT NULL,
    excerpt         TEXT NOT NULL
);

-- Suggestions
CREATE TABLE analysis_suggestions (
    id              SERIAL PRIMARY KEY,
    job_name        TEXT NOT NULL,
    run_id          TEXT NOT NULL,
    test_name       TEXT NOT NULL,
    ordinal         INT NOT NULL,
    suggestion      TEXT NOT NULL,
    FOREIGN KEY (job_name, run_id, test_name) REFERENCES analysis_tests (job_name, run_id, test_name)
);
```

**Note:** `analysis_fact_rows` and `analysis_kusto_proof_rows` use JSONB for individual row data because Kusto query result columns are dynamic — different queries return different columns. This is appropriate JSONB usage (single-row key-value pairs with variable schema, not a giant document blob).

### 6. Git Worktree Management

**Storage:** PV attached to the release dashboard backend StatefulSet

**Repos:** ARO-HCP, clusters-service, maestro, HyperShift

**Auth:** Azure workload identity federated to GitHub, or GitHub App installation token from Key Vault

**Lifecycle:**

1. **Bare clones** created on first use, stored at `{pv}/repos/{repo}.git`. Updated with `git fetch` before each analysis if stale.
2. **Worktrees** keyed by `(repo, commit SHA)`, stored at `{pv}/worktrees/{repo}/{commit-sha}/`. Created on demand. Shared read-only across all concurrent workers analyzing the same commits.
3. **Reference counting:** The cleanup controller tracks which active analyses reference each worktree. When the ref count drops to zero, the worktree is eligible for cleanup.
4. **Data directories** stored at `{pv}/data/{job-name}/{run-id}/{test-name}/`. Cleaned up after analysis results are persisted.

### 7. Release Dashboard Frontend Changes

**New route:** `GET /analysis/{job-name}/{run-id}` — job-level analysis page showing:
- Job summary
- List of per-test analyses with status indicators
- Links to individual test analyses

**New route:** `GET /analysis/{job-name}/{run-id}/{test-name}` — per-test analysis page showing:
- Fixed layout rendered from Postgres data via templ:
  - Summary banner
  - Facts table (with Kusto share links)
  - Numbered why-chain: each link shows claim, proof blocks (styled by type)
    - Kusto proofs: clickable share link + formatted result table + collapsible raw KQL
    - Log proofs: source path + excerpt in a code block
    - Code proofs: repo/file/lines + excerpt with syntax highlighting
    - Per-proof notes rendered inline
  - Suggestions section
  - Free-form notes (if any)

**Existing pages:** Build/rollout pages that display `ProwJobResult` rows gain an "Analysis" link when `analysis_jobs` has a matching row with `status = complete`.

### 8. Authentication

**Scope:** `/analysis/*` and `/auth/*` routes only. All other dashboard routes remain unauthenticated.

**Implementation:** Go OIDC middleware (e.g., `github.com/coreos/go-oidc/v3`)

**Dual IdP federation:**
- **Microsoft Entra ID:** issuer `https://login.microsoftonline.com/{tenant-id}/v2.0`
- **Red Hat SSO:** issuer URL for Red Hat's Keycloak instance

**Flow:**
1. Request hits `/analysis/*`
2. Middleware checks for valid session cookie
3. If absent → redirect to `/auth/login` (presents "Sign in with Microsoft" / "Sign in with Red Hat")
4. User selects IdP → OIDC authorization code flow → callback at `/auth/callback`
5. Token validated against the appropriate issuer → session cookie set
6. Redirect back to original URL

### 9. Graceful Shutdown

- On SIGTERM, stop accepting new analyses, drain in-progress ones
- `terminationGracePeriodSeconds` set to ~30 minutes
- Analyses stuck in `status = running` for >2 hours are reset to `pending` and retried from scratch (handled by the job-analysis-controller's reconcile loop)

### 10. Data Flow Summary

```
1.  Prow job fails
2.  Sippy ingests result
3.  sippy_controller writes to prow_results (existing)
4.  job-analysis-controller discovers failed job without analysis
5.  Inserts analysis_jobs row (status=pending) with commit SHAs
6.  Determines failed tests, inserts analysis_tests rows (status=pending)
7.  Sets analysis_jobs status=running
8.  test-case-analysis-controller picks up pending tests (bounded worker pool)
9.  Worker:
      a. Ensure git worktrees at correct commits
      b. Run deterministic data gathering (Go library import)
      c. Run agentic LLM analysis loop
         - Read manifest.json → read test logs → navigate to relevant resources
         - Read source code in repo worktrees to understand behavior
         - Run ad-hoc Kusto queries when pre-gathered data insufficient
         - Produce draft chain JSON (claims + KQL strings)
      d. Deterministic hydration (re-run KQL → populate share URIs + tables)
      e. Schema validation
      f. Write hydrated analysis to normalized Postgres tables
      g. Set analysis_tests status=complete
10. job-analysis-controller detects all tests done
11. Runs lightweight summary agentic step (reads per-test chains)
12. Writes summary to analysis_jobs
13. Sets analysis_jobs status=complete
14. analysis-artifact-cleanup-controller cleans up worktrees + data dirs
15. Dashboard frontend renders analysis pages from Postgres via templ
16. Existing job result pages show "Analysis" links
```

## Code Organization

| Concern | Location | Repo |
|---------|----------|------|
| Deterministic data gathering (Go library + CLI) | `tooling/triage/` | ARO-HCP |
| Data gathering CI validation (invariant checks) | `tooling/triage/` | ARO-HCP |
| KQL query templates | `tooling/triage/` | ARO-HCP |
| Analysis engine controllers | `release-dashboard/backend/pkg/controllers/` | sdp-pipelines |
| Agent loop + tool dispatch | `release-dashboard/backend/pkg/controllers/` (or sub-package) | sdp-pipelines |
| System prompt + reference docs | `release-dashboard/backend/` (embedded) | sdp-pipelines |
| Analysis JSON schema + hydration | `release-dashboard/` (shared package) | sdp-pipelines |
| HTML rendering (templ pages) | `release-dashboard/frontend/ui/pages/` | sdp-pipelines |
| Frontend handlers | `release-dashboard/frontend/handlers/` | sdp-pipelines |
| Auth middleware | `release-dashboard/frontend/` | sdp-pipelines |
| Postgres schema migrations | `release-dashboard/internal/storage/migrations/` | sdp-pipelines |
| sqlc queries | `release-dashboard/internal/storage/` | sdp-pipelines |

## Dependencies

| Dependency | Purpose | Auth |
|------------|---------|------|
| Azure AI Foundry | LLM chat completions with tool calling | Workload identity |
| Azure Data Explorer (Kusto) | Log queries + share URI generation | Workload identity |
| Azure Database for PostgreSQL | Analysis storage + coordination | Workload identity |
| GCS (`test-platform-results` bucket) | Prow job artifacts | Unauthenticated (public) |
| GitHub | Repo cloning | Federated from workload identity or App token from Key Vault |
| Sippy API | Test failure discovery | Unauthenticated (existing) |
| Entra ID + Red Hat SSO | OIDC authentication for analysis pages | OIDC client credentials |

## Open Items / Future Work

- **Exemplar analyses:** Create 2-3 hand-crafted analyses to use as few-shot examples in the system prompt
- **Historical analysis comparison:** Allow the agent to read analyses of previous runs of the same test to identify recurring vs. novel failures
- **Detailed Postgres schema review:** The schema above is a starting point — column types, indexes, and constraints should be refined during implementation
- **LSP integration:** If text-based code search proves insufficient for the agent's source code comprehension, add gopls integration for go-to-definition and find-references

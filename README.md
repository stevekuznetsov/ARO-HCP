# Identity Validation Screenshot Evidence

All values and series labels are SYNTHETIC DATA, not production telemetry or evidence of live PromQL execution. Every screenshot includes an explicit synthetic-data banner. Invented values illustrate the charts, not a mathematically simulated validation workload or an exhaustive label inventory.

## Screenshots

- `before-desktop.png`: existing Backend Metrics tab at 1440px, with ten charts and no Identity Validation tab.
- `after-desktop.png`: new Identity Validation tab adjacent to Backend Metrics at 1440px, with six charts.
- `after-mobile.png`: the same new tab at 390px, recording existing renderer limitations rather than hiding them.

Only these three PNGs and this README are published. HTML previews, capture code, and browser measurements stay local under `/tmp/opencode/identity-validation-evidence`.

## Method

Used the existing `buildChartData`, `renderPanelHTML`, and `renderObservabilityPage` functions. Before configuration came from `git show HEAD:test/cmd/aro-hcp-tests/gather-observability/queries.yaml`; after configuration came from the edited embedded YAML. Backend charts are unchanged and use identical synthetic input in both previews. Other tabs retain their configuration titles but have placeholder content outside the screenshot scope.

Each series has 61 one-minute samples from 12:00 to 13:00 UTC on 2026-09-27. ECharts was inlined from the repository's existing local asset for offline capture. No live service, Azure workspace, or credentials were used. The temporary rendering helper was created and removed with apply_patch; no Go test is retained in the worktree.

## Verification

- `go test ./cmd/aro-hcp-tests/gather-observability -count=1` from `test/`: passed (18.848s).
- Temporary preview generation: `go test ./cmd/aro-hcp-tests/gather-observability -run '^TestIdentityEvidencePreview$' -count=1 -v`: passed.
- `git diff --check -- test/cmd/aro-hcp-tests/gather-observability/queries.yaml`: passed.
- `node /tmp/opencode/identity-validation-evidence/capture.mjs`: passed chart-count, SVG rendering, sample-count, unit, zero initial threshold, tab adjacency, and synthetic-banner checks. Zero browser exceptions or external HTTP requests.
- Desktop: no page or panel horizontal overflow. Mobile: outer page fits, but the existing metrics renderer clips/overflows chart content and crowds time ticks/legends. The mobile screenshot records that limitation; renderer changes are intentionally out of scope.

PromQL was reviewed for rate-before-dedup, dedup-before-sum, retained cluster labels, and positive mean denominators. These synthetic renderer checks do not evaluate PromQL or establish live metric availability.

## Publication

Published via the GitHub API to the new orphan branch `skuznets/identity-validation-telemetry-evidence` in `stevekuznetsov/ARO-HCP`, refusing to overwrite an existing branch. The evidence commit has no parents and contains only this README and the three PNGs, with no repository source, source history, or secrets. No source-worktree commit or push was performed.

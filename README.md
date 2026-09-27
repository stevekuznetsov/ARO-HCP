# Backend Workqueue Screenshot Evidence

All values are SYNTHETIC DATA, not production telemetry or evidence of live PromQL execution. Each HTML preview and screenshot has an explicit synthetic-data banner.

## Files

- `before-desktop.png`: full Backend panel at 1440px width, 4 charts.
- `after-desktop.png`: full Backend panel at 1440px width, 10 charts.
- `after-mobile.png`: full Backend panel at 390px width, 10 charts.

Only these three PNGs and this README are published. Standalone HTML previews, browser measurements (`validation.json`), and the local capture script remain in `/tmp/opencode/backend-workqueue-evidence`; they are not part of the evidence branch.

## Method And Validation

Used the existing `buildChartData`, `renderPanelHTML`, and `renderObservabilityPage` renderer functions. Before uses `git show HEAD:test/cmd/aro-hcp-tests/gather-observability/queries.yaml`; after uses the worktree's embedded queries.yaml. This preserves the original retry description before, updates it after, and omits exactly the six new charts before. Shared charts use identical synthetic series, with 61 one-minute samples from 12:00 to 13:00 UTC on 2026-09-27. The independent invented series illustrate chart appearance, not a mathematically simulated queue.

The temporary `zz_workqueue_evidence_preview_test.go` was created and deleted using apply_patch. Only `go test ./cmd/aro-hcp-tests/gather-observability -run '^TestWorkqueueEvidencePreview$' -count=1 -v` was run, and passed. No full package verification was repeated. No tracked file or other agent file was edited.

Browser checks passed for chart counts, rendered SVG charts, sample counts, updated/original retry descriptions, six new unit labels, synthetic labels, and no page/panel horizontal overflow. Zero browser exceptions and zero external HTTP requests. Browser timezone is UTC.

Mobile limitation: chart containers resize to 358px within the 390px viewport, but long SVG chart titles and time subtitles are clipped. Full titles remain available in the wrapping PromQL footer. This existing renderer behavior is intentionally not patched or hidden.

## Publication

Published separately in `stevekuznetsov/ARO-HCP` on the orphan branch `skuznets/backend-workqueue-diagnostics-evidence`. The evidence commit contains only this README and the three PNGs, without source files or source history.

Use commit-pinned raw screenshot URLs in PR Markdown and retain the synthetic-data qualification. The publication script prints those immutable URLs. A prepared comment can be posted with `gh pr comment PR_NUMBER --repo Azure/ARO-HCP --body-file SCREENSHOT_COMMENT.md`.

---
trigger: model_decision
description: Receipt-Driven Development (RDD) frozen-snapshot workflow using odd_review_* MCP tools, 4R lenses, refuter batch, and consent gates.
---

# Receipt-Driven Development (RDD)

RDD ensures significant diffs are reviewed against an **immutable SHA-256 frozen snapshot** before delivery. RDD is enabled by default in `.agents/odd/config.json` and toggled via `odd-agy rdd <enable|disable|status>` or `odd_review_mode`.

## 1. `odd_review_*` MCP Lifecycle

1. **`odd_review_mode`**: Inspect or toggle project RDD mode (`status` | `enable` | `disable`).
2. **`odd_review_assess`**: Inspect live diff or committed `baseRef` range, optionally pass `escalate: {item: 1..6, reason}` per High-Risk rubric, and compute risk tier + `reviewDue`.
3. **`odd_review_start` & `odd_review_consent`**: Freeze the exact candidate diff and touched file contents under `.git/odd-agy/candidates/<lineageId>/` with a SHA-256 manifest, selecting applicable 4R lenses (`risk`, `resilience`, `readability`, `reliability`) with a clear `lensesReason`.
   - **NEVER** call `odd_review_consent` on your own, assume approval, or invoke `odd_review_consent` silently without explaining first.
   - After `odd_review_start` returns `consent_required`, **ALWAYS** explain clearly to the human in their reply language before asking: why review is due (risk tier + trigger reason), which lenses will run, and the effect of granting vs declining. Then ask via `ask_question` with choices `granted` and `declined`, and call `odd_review_consent` only with their literal answer.
4. **Parallel Frozen-Snapshot Review (`odd_review_scope` / `odd_review_run_lenses` / `odd_external_run`)**:
   - **For `agy-native` review lenses**: ALWAYS launch ALL pending lenses together in ONE single parallel `invoke_subagent` call (enforced by the `PreToolUse` hook; never launch pending native lenses one by one). All 4R lens agents and `review-refuter` MUST inspect the candidate exclusively through `odd_review_scope` — never the live worktree.
   - **For `external-mcp` review lenses**: Call the grouped MCP tool **`odd_review_run_lenses`** (`{"lineageId": "..."}`) once to execute all pending external lenses concurrently (each runs in a frozen-snapshot-only sandbox via `read_review_scope` and submits directly via `submit_review_findings`; `odd_external_run` with `lineageId` remains available for individual external calls). If `odd_review_run_lenses` returns `nativeLensesPending`, launch all listed native lenses together in ONE single parallel `invoke_subagent` call.
5. **`odd_review_submit` (Manual Fallback Only)**:
   - Only fall back to manual `odd_review_submit` if a reviewer did not already report `submittedToLineage: true`.
   - When calling `odd_review_submit`, always pass exact schema fields: `id`, `severity` (`"BLOCKER"` / `"SEVERE"`, `"WARNING"`, `"SUGGESTION"`), `causality` (`"candidate"`, `"pre_existing"`, `"unknown"`), `location` (`file:line`), and `claim` (or `refuterVerdicts` for `review-refuter`).
   - Candidate-caused severe blockers open at most **1 bounded correction** budget.
   - Pre-existing issues not introduced by the candidate (`causality: "pre_existing"`) are recorded as non-blocking advisories.
6. **`odd_review_status` & `odd_review_ack`**: Verify transaction state via `odd_review_status` and close by burning the approved lineage receipt with `odd_review_ack` once all severe candidate findings are resolved or refuted.

## 2. Separation from Delivery

`odd_review_ack` closes the review receipt only. It **never** authorizes `git push`, PR creation, or publishing — those always remain explicit human decisions.

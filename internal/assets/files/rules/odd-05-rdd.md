---
trigger: model_decision
description: Receipt-Driven Development (RDD) frozen-snapshot workflow using odd_review_* MCP tools, 4R lenses, refuter batch, and consent gates.
---

# Receipt-Driven Development (RDD)

RDD ensures significant diffs are reviewed against an **immutable SHA-256 frozen snapshot** before delivery. RDD is enabled by default in `.agents/odd/config.json` and toggled via `odd-agy rdd <enable|disable|status>` or `odd_review_mode`.

## 1. `odd_review_*` MCP Lifecycle

1. **`odd_review_mode`**: Inspect or toggle project RDD mode (`status` | `enable` | `disable`).
2. **`odd_review_assess`**: Inspect live diff or committed `baseRef` range, optionally pass `escalate: {item: 1..6, reason}` per High-Risk rubric, and compute risk tier + `reviewDue`.
3. **`odd_review_start`**: Freeze the exact candidate diff and touched file contents under `.git/odd-agy/candidates/<lineageId>/` with a SHA-256 manifest. Select applicable 4R lenses (`risk`, `resilience`, `readability`, `reliability`) with a clear `lensesReason`, and obtain explicit human consent (`granted` | `declined`) before admitting findings.
4. **`odd_review_scope`**: Read-only paginated view of the frozen candidate verified against SHA-256 digests. All review lenses and the refuter MUST inspect the candidate exclusively through `odd_review_scope` — never the live worktree.
5. **`odd_review_submit`**: Submit structured verdicts from the selected 4R lens agents (`review-risk`, `review-resilience`, `review-readability`, `review-reliability`) and the `review-refuter` batch.
   - Candidate-caused severe blockers open at most **1 bounded correction** budget.
   - Pre-existing issues not introduced by the candidate are recorded as non-blocking advisories.
6. **`odd_review_status` & `odd_review_ack`**: Check transaction state and burn the approved lineage receipt once all severe candidate findings are resolved or refuted.

## 2. Separation from Delivery

`odd_review_ack` closes the review receipt only. It **never** authorizes `git push`, PR creation, or publishing — those always remain explicit human decisions.

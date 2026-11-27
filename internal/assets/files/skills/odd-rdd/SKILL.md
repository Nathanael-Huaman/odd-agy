---
name: odd-rdd
description: Toggle or inspect Receipt-Driven Development (RDD) mode, assess current changes, or inspect frozen review status.
---

# `/odd-rdd` — RDD Control & Review Lifecycle

Manage Receipt-Driven Development for the current project:

1. **Inspect or toggle mode**:
   - Use `call_mcp_tool` -> `odd_review_mode` (`action: "status" | "enable" | "disable"`) or run `odd-agy rdd status|enable|disable`.
2. **Assess diff risk**:
   - Call `odd_review_assess` (with optional `baseRef` and `escalate` rubric item `1..6`) to determine risk tier and whether review is due.
3. **Run frozen-snapshot review**:
   - Call `odd_review_start` with selected 4R lenses (`risk`, `resilience`, `readability`, `reliability`) and `lensesReason`.
   - When `consent_required` is returned, **ALWAYS** explain clearly to the human in their reply language before invoking `odd_review_consent`: why review is due (risk tier + trigger reason), which lenses will run, and the effect of granting vs declining — NEVER invoke `odd_review_consent` silently. Obtain explicit consent via `ask_question` (`granted` | `declined`) and pass their literal answer to `odd_review_consent`.
   - Launch all requested `review-*` lenses in **parallel** (via `odd_external_run` or `invoke_subagent` per `.agents/odd/profiles.json`, plus `review-refuter` on severe findings).
   - For `external-mcp` roles, pass `lineageId` on `odd_external_run` so each reviewer reads ONLY the frozen candidate via `read_review_scope` and submits directly via `submit_review_findings`.
   - Only fall back to manual `odd_review_submit` if a reviewer did not already report `submittedToLineage: true`, passing exact schema fields (`id`, `severity`, `causality`, `location`, `claim`).
   - Verify `odd_review_status` and burn the approved receipt with `odd_review_ack` once satisfied. Note: `odd_review_ack` never authorizes `git push` or PR creation.

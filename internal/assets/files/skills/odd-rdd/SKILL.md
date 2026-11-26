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
   - Call `odd_review_start` with selected 4R lenses (`risk`, `resilience`, `readability`, `reliability`) and `lensesReason`, obtaining explicit human consent via `ask_question`.
   - Dispatch `review-*` agents (and `review-refuter` on severe findings) via `invoke_subagent` or `odd_external_run` (per `.agents/odd/profiles.json`).
   - Burn the approved receipt with `odd_review_ack` once satisfied. Note: `odd_review_ack` never authorizes `git push` or PR creation.

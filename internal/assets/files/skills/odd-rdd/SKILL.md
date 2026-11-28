---
name: odd-rdd
description: Toggle or inspect Receipt-Driven Development (RDD) mode, assess current changes, or inspect frozen review status.
---

# `/odd-rdd` — RDD Control & Review Lifecycle

Manage Receipt-Driven Development for the current project:

- **Desktop App / `/` cwd**: When running in Antigravity Desktop App or if an `odd-agy/*` MCP tool reports running in `"/"`, pass `workspaceRoot` (the active project's absolute directory path) in every `odd_review_*`, `todo`, and `odd_external_*` call.

1. **Inspect or toggle mode**:
   - Use `call_mcp_tool` -> `odd_review_mode` (`action: "status" | "enable" | "disable"`) or run `odd-agy rdd status|enable|disable`.
2. **Assess diff risk**:
   - Call `odd_review_assess` (with optional `baseRef` and `escalate` rubric item `1..6`) to determine risk tier and whether review is due.
3. **Run frozen-snapshot review**:
   - Call `odd_review_start` with selected 4R lenses (`risk`, `resilience`, `readability`, `reliability`) and `lensesReason`.
   - When `consent_required` is returned, **ALWAYS** explain clearly to the human in their reply language before invoking `odd_review_consent`: why review is due (risk tier + trigger reason), which lenses will run, and the effect of granting vs declining — NEVER invoke `odd_review_consent` silently. Obtain explicit consent via `ask_question` (`granted` | `declined`) and pass their literal answer to `odd_review_consent`.
   - **For `agy-native` review lenses**: ALWAYS launch ALL pending `review-*` lenses together in ONE single parallel `invoke_subagent` call (enforced by the `PreToolUse` hook; never one by one), plus `review-refuter` on severe findings.
   - **For `external-mcp` review lenses**: Call the grouped MCP tool **`odd_review_run_lenses`** (`{"lineageId": "..."}`) to execute all pending external lenses concurrently (each reads ONLY the frozen candidate via `read_review_scope` and submits directly via `submit_review_findings`; `odd_external_run` with `lineageId` also supports single external calls). `odd_review_run_lenses` and `odd_external_run` wait in bounded chunks (~100s default; `waitSeconds` up to `120`) to prevent Antigravity's 3m0s MCP timeout:
     - When `odd_review_run_lenses` returns `status: "running"` (with `runningLenses`), re-call `odd_review_run_lenses` with the same `lineageId` (and `workspaceRoot`) to attach to the active batch without launching duplicate reviews; **never** fall back to native while `status` is `"running"`.
     - When `odd_external_run` returns `status: "running"` with a `taskId`, poll `odd_external_status` (`{taskId, waitSeconds: 100}`, plus `workspaceRoot` when applicable) until a terminal status; **never** fall back to native while `status` is `"running"`.
     - Only fall back to native `invoke_subagent` (with `fallback_to_native` in the prompt, launching all pending native lenses in ONE single parallel call) when: `odd_review_run_lenses` completes with `fallbackToNative: true` / `nativeLensesPending`, `odd_external_run` / `odd_external_status` returns `status: "fallback_to_native"`, or an external MCP call aborts with a raw host/transport timeout (e.g., `timed out after 3m0s`) without JSON — in that case, call `odd_review_status` first and fall back only if the role is still pending.
   - Only fall back to manual `odd_review_submit` if a reviewer did not already report `submittedToLineage: true`, passing exact schema fields (`id`, `severity`, `causality`, `location`, `claim`).
   - Verify `odd_review_status` and burn the approved receipt with `odd_review_ack` once satisfied. Note: `odd_review_ack` never authorizes `git push` or PR creation.

---
trigger: always_on
description: Orchestrator delegation contract, Antigravity subagent mechanisms, hybrid external MCP routing, and workspace isolation.
---

# ODD Orchestrator & Delegation Contract

## 1. Delegation Mode (`full-delegation` by default)

Project mode lives in `.agents/odd/config.json` (`delegationMode`: `full-delegation` | `organic`) and is switchable via `odd-agy mode <full-delegation|organic>` or `odd-agy ui`.

- **`full-delegation` (default)**: The parent orchestrator (`odd-orchestrator`) coordinates, tracks, verifies, and communicates, and **never edits source, tests, configs, or docs directly**. Its `write_to_file` access is hook-gated solely to `odd/tasks/*.md`. Route every code, test, asset, or doc mutation to `gentle-ai-worker` (or `jd-fix-agent` for Judgment Day fixes).
- **`organic`**: Small, single-unit edits may be delegated or executed directly, while multi-unit, parallel, or tracked `Spec:` tasks still delegate with explicit edit boundaries.

## 2. Delegation Mechanisms

- **Ask**: Use `ask_question` with a concise closed set of choices and a clear recommendation.
- **Explore**:
  - For small or read-only exploration requests that fit in 3 bounded reads (e.g., repos with `<= 3` source/test files), read them directly without spawning `gentle-ai-explore`, and skip reading `.agents/odd/profiles.json` when no delegation or external-model call is needed.
  - Otherwise, invoke `gentle-ai-explore` (`flash`, read-only) via `invoke_subagent` to produce a `<=2k` token map with `path:line` evidence before planning.
  - When you do spawn `gentle-ai-explore`, wait for its result — never duplicate the same reads in parallel and never cancel (`manage_subagents kill`) a running explorer just to answer first.
- **Implement (Writer)**: Invoke `gentle-ai-worker` (`pro`, `inheritCustomizations: false`, `commandExecutionPolicy: sandbox`) with:
  - `Spec: odd/tasks/<feature>.md, T<n>, S#...`
  - `## Allowed edit surfaces` listing exact repo-relative files or globs
  - `## Verification` listing exact foreground commands
  - For 2+ concurrent writers on disjoint surfaces, pass `Workspace: "branch"` (or isolated branch/worktree) so parallel writers never overwrite each other.
- **Wake idle worker**: Use `send_message` to resume or redirect an existing subagent without losing its accumulated context.
- **Verify**: Invoke `gentle-ai-verify` for independent spec and runtime verification on High-Risk rubric items 1-6 or cross-cutting invariants.
- **Track**: Keep `odd/tasks/<feature>.md`, `todo` MCP, and Engram synchronized after each completed work unit.
- **Close with exact `Risk:` line**: Close every code-changing response with the exact line format `Risk: none` or `Risk: item N (<short reason>)` (matching the High-Risk rubric; never use vague prose such as `"Risk: Bajo..."`).
- **Incident**: On hook denial, surface conflict, or repeated verification failure, halt writes, inspect the exact error output, and either narrow scope or escalate to the user.

## 3. Hybrid OAuth + OpenAI-Compatible Routing

Model routing per role is defined in `.agents/odd/profiles.json` (default active profile: `agy-oauth`).

- **OAuth safety**: Never shell out to `agy -p` or `gemini -p` subprocesses. Native Antigravity OAuth roles always run through `invoke_subagent`. For `agy-native` `review-*` lenses, ALWAYS launch ALL pending lenses together in ONE single parallel `invoke_subagent` call (enforced by `PreToolUse`; never one by one).
- **External MCP roles**: When `.agents/odd/profiles.json` assigns a role to `external-mcp` (e.g., `deepseek-hybrid`, `9router-hybrid`, `omniroute-hybrid`, or custom `openai-compatible`), dispatch it via `call_mcp_tool` using `odd_external_run`, `odd_external_continue`, and `odd_external_status`.
- For `external-mcp` `review-*` lenses, call **`odd_review_run_lenses`** (`{"lineageId": "..."}`) once to execute all pending external lenses concurrently in a frozen-snapshot-only sandbox (`read_review_scope` + `submit_review_findings`) without manual finding copying; if `nativeLensesPending` is returned, launch all of them together in ONE single parallel `invoke_subagent` call. (`odd_external_run` with `lineageId` remains available for single-lens runs.)
- Pass the exact same `## Allowed edit surfaces`, `Spec:`, and `## Verification` contract to `odd_external_run` for writer roles; the Go runner enforces edit-surface bounds in code and falls back cleanly to `agy-oauth` if configured.

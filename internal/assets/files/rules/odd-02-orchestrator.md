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
- **Explore**: Invoke `gentle-ai-explore` (`flash`, read-only) via `invoke_subagent` to produce a `<=2k` token map with `path:line` evidence before planning.
- **Implement (Writer)**: Invoke `gentle-ai-worker` (`pro`, `inheritCustomizations: false`, `commandExecutionPolicy: sandbox`) with:
  - `Spec: odd/tasks/<feature>.md, T<n>, S#...`
  - `## Allowed edit surfaces` listing exact repo-relative files or globs
  - `## Verification` listing exact foreground commands
  - For 2+ concurrent writers on disjoint surfaces, pass `Workspace: "branch"` (or isolated branch/worktree) so parallel writers never overwrite each other.
- **Wake idle worker**: Use `send_message` to resume or redirect an existing subagent without losing its accumulated context.
- **Verify**: Invoke `gentle-ai-verify` for independent spec and runtime verification on High-Risk rubric items 1-6 or cross-cutting invariants.
- **Track**: Keep `odd/tasks/<feature>.md`, `todo` MCP, and Engram synchronized after each completed work unit.
- **Incident**: On hook denial, surface conflict, or repeated verification failure, halt writes, inspect the exact error output, and either narrow scope or escalate to the user.

## 3. Hybrid OAuth + OpenAI-Compatible Routing

Model routing per role is defined in `.agents/odd/profiles.json` (default active profile: `agy-oauth`).

- **OAuth safety**: Never shell out to `agy -p` or `gemini -p` subprocesses. Native Antigravity OAuth roles always run through `invoke_subagent`.
- **External MCP roles**: When `.agents/odd/profiles.json` assigns a role to `external-mcp` (e.g., `deepseek-hybrid`, `9router-hybrid`, `omniroute-hybrid`, or custom `openai-compatible`), dispatch it via `call_mcp_tool` using `odd_external_run`, `odd_external_continue`, and `odd_external_status`.
- Pass the exact same `## Allowed edit surfaces`, `Spec:`, and `## Verification` contract to `odd_external_run`; the Go runner enforces edit-surface bounds in code and falls back cleanly to `agy-oauth` if configured.

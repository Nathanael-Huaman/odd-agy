---
name: odd-doctor
description: Run odd-agy diagnostics checking Antigravity CLI, global engram, workspace .agents/ integrity, tool allowlist, and provider health.
---

# `/odd-doctor` — Workspace & Harness Diagnostics

Diagnose the local `odd-agy` installation and workspace health:

1. Execute `odd-agy doctor` via `run_command` to inspect:
   - `agy` CLI availability and version
   - Global `engram` availability
   - `.agents/rules/`, `.agents/agents/`, `.agents/skills/`, `.agents/hooks.json`, and `.agents/mcp_config.json` integrity
   - Valid Antigravity tool names across all agent definitions
   - Active profile and external provider reachability / env var readiness
   - Any colliding global `~/.gemini/` rules that could duplicate workspace rules
2. Summarize passes, warnings, and exact remediation commands without altering user files unless authorized.

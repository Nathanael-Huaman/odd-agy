---
name: skill-improver
description: Audit and refine an existing SKILL.md for clarity, Antigravity tool accuracy, conciseness, and observed execution gaps.
---

# `/skill-improver` — Refine Existing Skills

Improve an existing skill based on concrete friction or contract gaps:

1. **Inspect current skill & usage**: Read `.agents/skills/<name>/SKILL.md` and identify ambiguous instructions, missing verification steps, bloated prose, or invalid tool names.
2. **Surgical upgrade**:
   - Preserve `name` matching the directory and sharpen `description` so `agy` matches user intent accurately.
   - Replace vague advice with deterministic steps, exact tool names (`invoke_subagent`, `call_mcp_tool`, `ask_question`), and clear exit criteria.
   - Keep instructions concise and free of stale harness tokens.
3. **Verify**: Delegate edits via `gentle-ai-worker` (when in `full-delegation`) and run `odd-agy doctor` or asset contract tests.

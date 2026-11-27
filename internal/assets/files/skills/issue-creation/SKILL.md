---
name: issue-creation
description: Draft clear, reproducible GitHub/tracker issues with verified root cause, exact path:line anchors, and acceptance criteria.
---

# `/issue-creation` — Structured Issue Drafting

Produce actionable, evidence-backed issue reports:

1. **Gather verified evidence**: Use `gentle-ai-explore` or read-only inspection (`view_file`, `grep_search`) to confirm exact `path:line` anchors, observed output, and expected behavior. Never speculate on unverified causes.
2. **Structure the issue**:
   - **Title**: Concise symptom + affected component.
   - **Context & Impact**: Who/what is affected and severity.
   - **Reproduction**: Exact minimal commands or inputs + observed error/exit code.
   - **Root Cause / Anchors**: Verified `path:line` pointers (or open investigation bounds).
   - **Acceptance Criteria (`S#`)**: Concrete testable checks.
3. **Human confirmation**: Show the draft and ask via `ask_question` before running `gh issue create`.

---
trigger: model_decision
description: Detailed guidance for odd/tasks/*.md tracking, Engram canonical memory workflow, todo MCP synchronization, and work-unit commit slicing.
---

# ODD Tracking, Memory & Work-Unit Slicing

## 1. Feature File Structure (`odd/tasks/<feature>.md`)

Create `odd/tasks/<feature>.md` for Medium/Large tasks or any task passing the Resume Test:

- **Header**: Feature title, `- **Objective**:`, `- **Branch**:`, `- **Delivery strategy**:` (`ask-on-risk`), `- **Forecast**:`, `- **Test runner**:`.
- **`## Specs`**: Record verbatim user requirements alongside `S1`, `S2`, ... IDs and concrete `**Checks**:` criteria. `## Specs` is authoritative over summaries.
- **`## Tasks`**: Ordered checklist:
  `- [ ] **T1** (\`S1, S2\`) · Route: \`gentle-ai-worker\` · <scope> · Commit: pending`
- **`## Log`**: Chronological `L1`, `L2`, ... entries capturing verbatim user constraints, architectural choices, commits, verification results, and RDD review receipts.

## 2. Engram Canonical Topic Workflow

Persist durable project learnings (architecture decisions, root causes, non-obvious constraints) via Engram without duplicating topics:
1. **Search first**: `mem_search` on the bounded topic in the active project.
2. **Read in full**: `mem_get_observation` on plausible matches.
3. **Update vs Save**: Use `mem_update` with the existing ID and `topic_key` when updating the same topic; use `mem_save` with a deliberate `topic_key` only for genuinely new topics.
4. Never store secrets, tokens, raw file dumps, or unverified speculation.

## 3. `todo` MCP Synchronization

Use the `todo` MCP tool (`write`, `add`, `update`, `list`, `clear`) to mirror active execution steps:
- Keep at most **one** item `in_progress` at a time.
- Require a `note` whenever marking an item `blocked`.
- Prefer `dropped` over silent deletion when an active item is intentionally superseded.

## 4. Work-Unit Commits & Slice Strategy

- Commit each completed, verified work unit at natural boundaries (~400 authored changed lines per slice; hard-warn above 800 lines unless split or explicitly waived).
- Follow `ask-on-risk`: ask the user before bundling high-risk slices or pushing branches (`git push` / `gh pr create` require explicit consent and passing RDD when enabled).

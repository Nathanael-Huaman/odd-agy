---
trigger: always_on
description: Core ODD 7-step workflow, High-Risk rubric, task sizing, safety guards, and language boundary.
---

# ODD Core Discipline (`odd-agy`)

Follow **ODD = Organic Driven Development** proportionally on every task.

## 1. Seven-Step Mandatory Workflow

1. **Authorize**: Before mutating repository files, configs, or git state, confirm the task is clearly authorized. Discussing, exploring, and diagnosing are always safe; confirm before ambiguous, irreversible, or high-impact changes.
2. **Explore**: Inspect existing code first using `gentle-ai-explore` or targeted file reads to gather exact `path:line` anchors before planning or editing.
3. **Resolve uncertainty**: Never guess when requirements, scope, ownership, or trade-offs are ambiguous. Pause and ask via `ask_question` with concrete bounded choices and a recommended option.
4. **Classify**: Size the task (`Small`, `Medium`, `Large`) by changed lines, touched files, dependencies, and reversibility, and apply the **Resume Test**:
   - **Small tasks** (clearly understood, no High-Risk item 1–6, passes the resume test without a tracking file): **MUST NOT** create `odd/tasks/<feature>.md`, **MUST NOT** create a `todo` list unless requested by the user, and **MUST NOT** start RDD (`odd_review_start`) when `odd_review_assess` returns `reviewDue: false` (unless the user explicitly asks for a review).
   - **Medium / Large / High-Risk tasks** (or work that could pause across resets or spans multiple units): create/update `odd/tasks/<feature>.md`.
5. **Track**: For tasks that need tracking, maintain `odd/tasks/<feature>.md` (`## Specs` verbatim, `## Tasks`, `## Log`), mirror durable decisions in Engram (`mem_search` -> `mem_get_observation` -> `mem_update` or `mem_save`), and sync the active checklist via the `todo` MCP tool.
6. **Implement**: Execute task by task using test-first discipline (`RED` -> `GREEN` -> `PRESERVE` -> `REFACTOR`) and logical work-unit commits (~400 authored changed lines per slice).
7. **Close**: Conclude only with verified command evidence (`<exact command>: <observed result>`), RDD review status when enabled, and an explicit `Risk:` line.

## 2. High-Risk Rubric (Items 1-6)

Escalate to independent verification (`gentle-ai-verify`) and RDD review when the change touches any of:
1. Security, authentication/authorization, permissions, credentials, hooks, or safety guards.
2. Data persistence, migrations, schema/state formats, destructive/irreversible mutations, or external sync.
3. Concurrency, locking, race conditions, process/IPC boundaries, or lifecycle/cleanup.
4. Public CLI/API/protocol contracts, config schemas, or cross-module shared invariants.
5. Complex control flow, state machines, parsers, or error-recovery paths.
6. Changes where failure causes silent corruption, data loss, or broken user workflows.

## 3. Language Boundary

- Reply to the user in the user's language.
- Write all generated technical artifacts (code, comments, tests, commit messages, rules, agent prompts, skills, `odd/tasks/*.md`) in English unless the user explicitly asks for another language.

## 4. Continuity & Safety

- **Authorized continuity**: Once the user authorizes a task or plan, execute all authorized steps without unnecessary stops until complete, blocked, ambiguous, or reaching a delivery/push gate.
- **Safety rules**:
  - Never read, print, or store secrets (`.env`, `.ssh`, private keys, credentials, API tokens).
  - Never run destructive commands (`rm -rf`, `git reset --hard`, `git checkout --`, `git push --force`) without explicit instruction.
  - Never run `git push` or open PRs autonomously; publishing remains an explicit human decision.
  - Preserve unrelated dirty files and pre-existing worktree changes.

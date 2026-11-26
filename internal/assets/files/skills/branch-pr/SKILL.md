---
name: branch-pr
description: Prepare a clean feature branch and pull request summary after verification and RDD checks, without auto-pushing without consent.
---

# `/branch-pr` — Feature Branch & Pull Request Preparation

Prepare a verified branch for pull request review while keeping push/publish human-gated:

1. **Pre-flight checks**:
   - Confirm current branch is not `main`/`master` and working tree is clean (`git status --short`).
   - Verify all planned tasks in `odd/tasks/<feature>.md` are complete and authorized tests pass.
   - When RDD is enabled, confirm `odd_review_status` shows the candidate reviewed/acked (or run `odd_review_start` first).
2. **Draft PR payload**:
   - Summarize Objective, linked `S#` specs, changed modules, exact `<command>: <observed result>` validation lines, and explicit `Risk:` assessment.
3. **Human delivery gate**:
   - Ask the user via `ask_question` before executing `git push` or `gh pr create` (respecting `ask-on-risk` if the diff is large or spans multiple work units).

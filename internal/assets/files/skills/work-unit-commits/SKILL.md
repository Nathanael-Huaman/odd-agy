---
name: work-unit-commits
description: Slice large features into logical ~400-line work-unit commits with verified tests, clean staging, and ask-on-risk delivery checks.
---

# `/work-unit-commits` — Logical Work-Unit Commit Slicing

Keep history reviewable, bisectable, and bounded to ~400 authored changed lines per slice:

1. **Boundary check**: Group related implementation + tests for one coherent task (`T<n>`) into one commit. Warn and consult the user (`ask-on-risk`) if a single slice exceeds ~400 authored lines or mixes unrelated concerns.
2. **Pre-commit verification**:
   - Confirm `git status --short` contains no scratch files, tool caches, secrets (`.env`), or unrelated dirty files.
   - Ensure authorized tests and builds pass after the final edit.
   - If RDD is enabled and a review boundary is reached, run `odd_review_assess` / `odd_review_start` before delivery.
3. **Commit discipline**:
   - Stage only the exact files belonging to the work unit.
   - Write an English imperative subject (`<type>(<scope>): <summary>`) and cite verified checks and `Risk:` in the commit/report trail.
   - Never run `git push` without explicit user authorization.

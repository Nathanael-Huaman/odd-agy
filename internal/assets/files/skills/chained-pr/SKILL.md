---
name: chained-pr
description: Split oversized changes into stacked/chained PR slices (~400 lines each) with explicit base branches and ask-on-risk consent.
---

# `/chained-pr` — Stacked / Chained PR Slicing

Break large features cleanly into sequential dependent PR slices:

1. **Plan slices**: Identify natural dependency layers (core schema/domain -> installer/engine -> hooks/MCP -> UI/docs), targeting ~400 authored lines per slice.
2. **Track lineage**: Record each slice's base branch, commit range, and `T<n>` / `S#` coverage in `odd/tasks/<feature>.md`.
3. **Verify per slice**: Each stacked branch must compile and pass tests independently on top of its parent slice, and pass RDD review before stacking or publishing.
4. **Consent before publishing**: Present the slice plan and exact `git push` / `gh pr create --base <parent>` commands via `ask_question` before creating any remote branch or PR.

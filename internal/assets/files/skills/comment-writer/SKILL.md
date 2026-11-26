---
name: comment-writer
description: Write concise high-signal code comments and docstrings explaining why invariants exist rather than narrating obvious syntax.
---

# `/comment-writer` — High-Signal Code Comments & Docstrings

Keep comments purposeful, accurate, and aligned with repository style:

1. **Explain WHY, not WHAT**: Document non-obvious invariants, ordering constraints, protocol/tool-allowlist rules, concurrency guarantees, and edge-case rationale. Avoid restating variable names or control flow.
2. **Public API docs**: Write crisp doc comments on exported types, functions, and constants matching the language's idiomatic convention (e.g., Go `// Name ...` sentences).
3. **Delegation boundary**: In `full-delegation`, delegate comment edits to `gentle-ai-worker` with exact `## Allowed edit surfaces` and verify tests/linters stay green.

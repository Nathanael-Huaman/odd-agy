---
name: cognitive-doc-design
description: Design clear, low-cognitive-load technical documentation, READMEs, and architecture guides tailored to real reader tasks.
---

# `/cognitive-doc-design` — Low-Cognitive-Load Documentation

Structure technical docs for fast scanning, accurate mental models, and zero stale claims:

1. **Verify every command & path**: Inspect the codebase before writing so every flag, file path, schema key, and output example matches actual code.
2. **Progressive disclosure**:
   - Start with a 2-sentence purpose + quick-start block.
   - Group details by user task (Install -> Configure -> Operate -> Troubleshoot).
   - Use compact tables and exact examples over long prose walls.
3. **Delegation & verification**: Route doc edits through `gentle-ai-worker` when in `full-delegation`, and verify referenced commands/tests remain valid.

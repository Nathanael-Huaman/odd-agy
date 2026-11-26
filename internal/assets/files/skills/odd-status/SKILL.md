---
name: odd-status
description: Inspect active ODD delegation mode, RDD state, active routing profile, todo checklist, and open task tracking files.
---

# `/odd-status` — Project ODD & Harness Status

Show the current workspace harness state concisely without mutating repository files:

1. Read `.agents/odd/config.json` and `.agents/odd/profiles.json` (or run `odd-agy mode status`, `odd-agy rdd status`, and `odd-agy profile show` via `run_command`).
2. Call `todo` (`list`) via `call_mcp_tool` to summarize active/pending/blocked items.
3. Call `odd_review_status` via `call_mcp_tool` if RDD is enabled to report any active or pending review lineage.
4. Scan `odd/tasks/*.md` (if present) using `find_by_name` and summarize open tasks (`- [ ]`) vs completed tasks (`- [x]`).
5. Present a compact status summary (Delegation Mode, RDD, Active Profile, Pending Tasks, Open Reviews).

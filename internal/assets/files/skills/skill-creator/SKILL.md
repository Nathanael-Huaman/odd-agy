---
name: skill-creator
description: Create a new Antigravity-compatible skill under .agents/skills/<name>/SKILL.md with valid frontmatter and verified tool names.
---

# `/skill-creator` — Create a Portable Antigravity Skill

Author a new project or portable skill cleanly under `.agents/skills/<kebab-name>/SKILL.md`:

1. **Check placement & naming**:
   - Must sit at exact 1-level depth: `.agents/skills/<kebab-name>/SKILL.md`.
   - YAML frontmatter MUST include `name: <kebab-name>` (matching folder) and a clear, non-empty `description:`.
2. **Use only verified Antigravity tools**:
   - Reference only `view_file`, `write_to_file`, `replace_file_content`, `multi_replace_file_content`, `list_dir`, `find_by_name`, `grep_search`, `run_command`, `invoke_subagent`, `send_message`, `manage_subagents`, `ask_question`, `manage_task`, `call_mcp_tool`, or `odd-agy` CLI/MCP tools.
   - Never reference unmapped or external harness tool names.
3. **Validate**: Delegate creation to `gentle-ai-worker` in `full-delegation`, add/run contract checks (or `odd-agy doctor`), and verify no broken references exist.

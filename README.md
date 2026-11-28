# odd-agy

Project-scoped **ODD** (Orchestrated Delegation Discipline) and **RDD** (Review-Driven Delivery) harness for **Antigravity CLI (`agy`)** and **Antigravity IDE/App** via workspace `.agents/`.

Distributed as a single Go binary (`odd-agy`) wrapped for `pnpm` (`github.com/Nathanael-Huaman/odd-agy`) so hooks execute in milliseconds without touching global `~/.gemini/` configuration. Only `engram` is expected as a global binary.

---

## Quickstart

### Local dev dependency (`pnpm`)

```bash
pnpm add -D odd-agy
pnpm exec odd-agy init
pnpm exec odd-agy launch
```

### Standalone project install (`pnpm dlx`)

```bash
pnpm dlx odd-agy init --mode=standalone
odd-agy launch
```

`odd-agy init` installs project-scoped `.agents/` assets, hooks, MCP configuration, `.agents/odd/agy-session-settings.json`, and defaults in your repository while preserving any pre-existing hooks or MCP servers in `.agents/hooks.json` and `.agents/mcp_config.json`—without touching `~/.gemini/`.

### Recommended launch (`odd-agy launch` — zero global config)

Start Antigravity CLI via `odd-agy launch` to pass `.agents/odd/agy-session-settings.json` with `--settings`, select `odd-orchestrator`, and forward `--model` and `--effort` from the active profile's `orchestrator` route (when not overridden after `--`; `--effort` is omitted if the chosen model already carries an effort suffix):

```bash
odd-agy launch
odd-agy launch -- --model pro
odd-agy launch --no-agent -- --continue
```

This keeps all permission rules 100% project-scoped (`mcp(odd-agy/*)` allowed, `mcp(odd-agy/odd_review_consent)` still prompting for explicit human approval) with zero writes to `~/.gemini/`.

### Optional global opt-in (`--allow-mcp` / `odd-agy permissions`)

Plain `odd-agy init` remains 100% project-scoped. If you prefer running `agy` directly without `odd-agy launch`, or need global registration for `external-mcp` / RDD tools (including Antigravity Desktop App), you can opt in to registering the `odd-agy` MCP server in `~/.gemini/config/mcp_config.json` and adding the MCP permission rules to both Antigravity CLI (`~/.gemini/antigravity-cli/settings.json`) and Antigravity Desktop App (`~/.gemini/config/config.json`) (creates timestamped `0600` `.odd-agy.bak.*` backups, enforces symlink guards, and preserves existing keys and rules):

```bash
odd-agy init --allow-mcp
# or manage anytime (or toggle in the TUI Project Actions screen):
odd-agy permissions status
odd-agy permissions enable   # alias: allow-mcp
odd-agy permissions disable  # alias: revoke-mcp
```

When an active profile uses `external-mcp` roles without global registration, `odd-agy doctor` and `odd-agy launch` emit readiness warnings.

---

## Interactive TUI & CLI Commands

Launch the Bubble Tea terminal UI with no arguments or via `ui`:

```bash
odd-agy
odd-agy ui
```

Every TUI setting has an equivalent non-interactive CLI command:

| Command | Description |
|---|---|
| `odd-agy init [--mode=local\|standalone] [--delegation=full-delegation\|organic] [--profile=agy-oauth] [--binary=...] [--allow-mcp] [--force]` | Install `.agents/` rules, agents, skills, hooks, MCP config, and `.agents/odd/agy-session-settings.json` (use `--allow-mcp` only to opt into global `~/.gemini/antigravity-cli/settings.json`) |
| `odd-agy launch [--agent=<name>] [--no-agent] [-- <extra agy args...>]` | Recommended zero-global-config launcher passing `--agent`, orchestrator `--model` / `--effort`, and `--settings <abs-path-to-.agents/odd/agy-session-settings.json>` to `agy` |
| `odd-agy permissions [status\|enable\|disable\|allow-mcp\|revoke-mcp] [--binary=<path>]` | Inspect, grant, or revoke optional global `~/.gemini/` MCP permission rules (`antigravity-cli/settings.json` + `config/config.json`) and `~/.gemini/config/mcp_config.json` registration |
| `odd-agy sync [--binary=...] [--force]` | Refresh managed `.agents/` files against `.agents/odd/.managed-manifest.json` |
| `odd-agy doctor` | Verify `agy`, `engram`, `.agents/` assets, hooks, MCP config, provider readiness, and global `~/.gemini/` collisions |
| `odd-agy uninstall` | Remove only managed `.agents/` files and `odd-agy` hook/MCP entries, keeping user entries intact |
| `odd-agy mode [status\|full-delegation\|organic]` | View or switch delegation mode (`full-delegation` by default) |
| `odd-agy rdd [status\|enable\|disable]` | View or toggle frozen-snapshot RDD review |
| `odd-agy profile [list\|show\|use <name>\|providers\|test <provider>]` | Manage model routing profiles and probe external providers |
| `odd-agy hook <SessionStart\|PreToolUse\|PostToolUse\|PreInvocation\|Stop>` | Dispatch an Antigravity lifecycle hook via stdin/stdout JSON |
| `odd-agy mcp` | Run the unified stdio MCP server (`todo`, `odd_review_*`, `odd_external_*`) |
| `odd-agy upstream check [--lock=...] [--source-dir=...] [--ref=...]` | Check tracked Gentle Shell / Gentle AI upstream drift |
| `odd-agy upstream sync [--ref=...] [--lock=...] [--source-dir=...]` | Update `upstream/upstream.lock.json` digests and validate embedded contracts |
| `odd-agy --version` / `odd-agy --help` | Print version or CLI usage |

Pass `--cwd <dir>` before or after any command to target another workspace directory.

---

## Profiles, External Providers & OAuth Safety

### OAuth safety guarantee

`odd-agy` **never** spawns `agy -p` or `gemini -p` subprocesses. Personal Antigravity OAuth subscription credentials stay solely inside Antigravity itself (`agy-native` roles).

### Built-in profiles, roles & effort (`.agents/odd/profiles.json`)

- **`agy-oauth`** *(default)*: All 12 roles (`orchestrator`, `explore`, `worker`, `verify`, `jd-judge-a`, `jd-judge-b`, `jd-fix-agent`, `review-risk`, `review-resilience`, `review-readability`, `review-reliability`, `review-refuter`) run natively on Antigravity OAuth (`inherit` / `flash` / `pro`).
- **`deepseek-hybrid`**: Uses DeepSeek (`compatibility: "deepseek-thinking"`, preserving `reasoning_content` across tool-calling turns) for `verify`, `jd-judge-b`, and `review-*` roles while keeping `orchestrator`/`explore`/`worker` on native Antigravity OAuth.
- **`9router-hybrid`**: Routes `verify`, `jd-judge-b`, and `review-*` through a local 9Router proxy (`http://localhost:20128/v1`, `NINEROUTER_BASE_URL` / optional `NINEROUTER_API_KEY` on loopback).
- **`omniroute-hybrid`**: Routes `verify`, `jd-judge-b`, and `review-*` through a local OmniRoute proxy (`http://localhost:20128/v1`, `OMNIROUTE_BASE_URL` / optional `OMNIROUTE_API_KEY` on loopback).

Each role supports an optional `effort` (`low`, `medium`, `high`, `xhigh`, `max`, or empty/`default`). In the TUI **Roles Editor**, vertical arrow-navigable **Model** (`[m]`) and **Effort** (`[e]`) pickers configure per-role routes: native Antigravity subagent frontmatter models normalize to `inherit | flash | pro`, while `orchestrator` forwards `--model` and `--effort` through `odd-agy launch`.

### Provider templates, API key store & routing enforcement

`.agents/odd/profiles.json` ships ready-to-use templates for `deepseek`, `9router`, `omniroute`, and generic `openai-compatible` endpoints (OpenRouter, OpenAI, Ollama, LM Studio, etc.) with custom `headers`, `apiKeyEnv`, `baseUrlEnv`, and ordered per-role `fallback` chains that cleanly fall back to `agy-oauth` on missing keys, `429`, or unreachable endpoints.

- **User-scoped API key store (`0600`)**: Store provider API keys in `~/.config/odd-agy/secrets.json` via masked `[s] set key` in the TUI **Providers & Live Probe** screen. Environment variables always take precedence, and secrets are never written to `.agents/` or Git.
- **Deterministic `external-mcp` enforcement & SSE decoding**: `PreToolUse` blocks native `invoke_subagent` calls for roles routed to `external-mcp` so they execute via `odd-agy/odd_external_run` (permitting native fallback only after `fallback_to_native`). The LLM client sends `"stream": false` and decodes `text/event-stream` SSE responses from 9Router, OmniRoute, and other OpenAI-compatible gateways.

Probe a configured provider at any time:

```bash
odd-agy profile providers
odd-agy profile test 9router
```

---

## Antigravity `.agents/` Layout, Hooks & MCP Tools

Running `odd-agy init` installs:

```text
.agents/
├── rules/              # 5 modular ODD/RDD rules (always_on rules strictly < 24,000 bytes)
├── agents/             # 12 native Antigravity agents (verified Antigravity tool names only)
├── skills/             # 15 portable slash commands & ODD workflows (1-level depth SKILL.md)
├── odd/
│   ├── agy-session-settings.json  # project-scoped MCP allow/ask rules for `odd-agy launch`
│   ├── config.json     # delegationMode, rddEnabled, installMode, binaryCommand
│   ├── profiles.json   # activeProfile, providers, per-role routes & fallbacks
│   └── .managed-manifest.json
├── hooks.json          # merged PreToolUse, PostToolUse, SessionStart, PreInvocation, Stop hooks
└── mcp_config.json     # merged odd-agy stdio MCP server (+ engram when present on PATH)
```

### Unified MCP Server (`odd-agy mcp`)

- **Todo**: `todo` (`write`, `add`, `update`, `list`, `clear`) persisted under `.git/odd-agy/todo.json`.
- **Frozen-Snapshot RDD**: `odd_review_mode`, `odd_review_assess`, `odd_review_start`, `odd_review_consent` (always preceded by a plain-language explanation of risk tier, lenses, and consent impact), `odd_review_scope` (paginated + SHA-256 verified), `odd_review_run_lenses` (grouped concurrent external lens runner returning `nativeLensesPending`), `odd_review_submit` (strict `id`/`severity`/`causality`/`location`/`claim` schema for 4R lenses + refuter, max 1 correction), `odd_review_apply_correction`, `odd_review_status`, `odd_review_ack`.
- **Sandboxed External Runner**: `odd_external_profiles`, `odd_external_run`, `odd_external_continue`, `odd_external_status` — executes concurrent MCP requests in parallel, enforces `## Allowed edit surfaces` in code, blocks secrets/destructive commands/`git push`, and persists `reasoning_content` history under `.git/odd-agy/tasks/<taskId>.json`. For `review-*` roles given `lineageId`, reviewers run in a frozen-snapshot-only sandbox (`read_review_scope` with sensitive-file redaction + direct `submit_review_findings`, returning `submittedToLineage: true`).

### What's New in `v0.1.3`

- **Antigravity Desktop App global permissions (`odd-agy permissions enable` / `--allow-mcp`)**: Also manages `userSettings.globalPermissionGrants` (`allow`: `mcp(odd-agy/*)`, `ask`: `mcp(odd-agy/odd_review_consent)`) inside `~/.gemini/config/config.json` alongside Antigravity CLI (`~/.gemini/antigravity-cli/settings.json`) and `~/.gemini/config/mcp_config.json`, preserving all existing keys and rules with timestamped `0600` backups and three-file symlink/JSON preflight.
- **Antigravity Desktop App multi-project `workspaceRoot` & MCP `rootUri`**: All 15 MCP tools accept an optional `workspaceRoot` parameter and `initialize` adopts `rootUri`/`rootPath`/`workspaceFolders` when started in `/`, keeping per-workspace `todo`, RDD, and external runner state isolated across concurrent projects.
- **Code-enforced parallel native lens batching (`PreToolUse`)**: `PreToolUse` blocks one-by-one `invoke_subagent` calls for pending `agy-native` 4R review lenses and requires launching all pending native lenses together in a single parallel batch (permitting single-lens calls only for `fallback_to_native` or a single remaining lens).
- **Grouped external review tool (`odd_review_run_lenses`)**: Executes all pending `external-mcp` 4R lenses concurrently for a `lineageId`, aggregates submission results, and reports `nativeLensesPending` (including any `fallback_to_native` lenses) for single-batch native dispatch.
- **`SEVERE` schema enum alignment**: Aligns the external reviewer `submit_review_findings` tool schema with `odd_review_submit` by accepting `"SEVERE"` alongside `"BLOCKER"`, `"WARNING"`, and `"SUGGESTION"`.

### What's New in `v0.1.2`

- **Concurrent MCP dispatch**: The stdio MCP server handles parallel tool calls concurrently so multiple `review-*` lenses execute in parallel.
- **Frozen-snapshot-only external reviewer sandbox**: Passing `lineageId` to `odd_external_run` for `review-*` roles restricts the reviewer to `read_review_scope` (with sensitive-file content redaction) and direct `submit_review_findings` (`submittedToLineage: true`), preventing live-worktree reads or manual finding copying.
- **Strict `odd_review_submit` schema**: Enforces structured `id`, `severity` (`BLOCKER`/`SEVERE`/`WARNING`/`SUGGESTION`), `causality` (`candidate`/`pre_existing`/`unknown`), `location` (`file:line`), and `claim` fields.
- **Aligned pre-consent & `Risk:` closing protocol**: Rules, skills, and orchestrator prompts require explaining risk tier, lenses, and grant/decline impact before invoking `odd_review_consent`, and closing code-changing replies with `Risk: none` or `Risk: item N (<short reason>)`.

---

## Upstream Tracking & Sync

`upstream/upstream.lock.json` tracks non-UX ODD/RDD prompt, skill, and enforcement files from `Gentleman-Programming/gentle-shell` and `Gentleman-Programming/gentle-ai` with SHA-256 digests:

```bash
odd-agy upstream check --source-dir /path/to/checkouts
odd-agy upstream sync --source-dir /path/to/checkouts --ref v5.0.0
```

`odd-agy upstream sync` verifies embedded asset contracts (`assets.ValidateAll()`) before updating `upstream/upstream.lock.json` and fails closed without modifying the lock file if validation fails.

---

## Testing

Run unit + integration + compiled-binary/`pnpm` E2E tests:

```bash
go test -count=1 ./...
go vet ./...
pnpm test
pnpm run test:e2e
```

Run the opt-in live Antigravity CLI smoke check (uses an isolated `mktemp -d` Git repo and never modifies `~/.gemini/`):

```bash
bash scripts/e2e-live-agy.sh
```

# odd-agy

Project-scoped **ODD** (Orchestrated Delegation Discipline) and **RDD** (Review-Driven Delivery) harness for **Antigravity CLI (`agy`)** and **Antigravity IDE/App** via workspace `.agents/`.

Distributed as a single Go binary (`odd-agy`) wrapped for `pnpm` (`github.com/Nathanael-Huaman/odd-agy`) so hooks execute in milliseconds without touching global `~/.gemini/` configuration. Only `engram` is expected as a global binary.

---

## Quickstart

### Local dev dependency (`pnpm`)

```bash
pnpm add -D odd-agy
pnpm exec odd-agy init
```

### Standalone project install (`pnpm dlx`)

```bash
pnpm dlx odd-agy init --mode=standalone
```

This installs project-scoped `.agents/` assets, hooks, MCP configuration, and defaults in your repository while preserving any pre-existing hooks or MCP servers in `.agents/hooks.json` and `.agents/mcp_config.json`.

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
| `odd-agy init [--mode=local\|standalone] [--delegation=full-delegation\|organic] [--profile=agy-oauth] [--binary=...] [--force]` | Install `.agents/` rules, agents, skills, hooks, and MCP config |
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

### Built-in profiles (`.agents/odd/profiles.json`)

- **`agy-oauth`** *(default)*: All 11 roles (`explore`, `worker`, `verify`, `jd-judge-a`, `jd-judge-b`, `jd-fix-agent`, `review-risk`, `review-resilience`, `review-readability`, `review-reliability`, `review-refuter`) run natively on Antigravity OAuth (`flash` / `pro`).
- **`deepseek-hybrid`**: Uses DeepSeek (`compatibility: "deepseek-thinking"`, preserving `reasoning_content` across tool-calling turns) for `verify`, `jd-judge-b`, and `review-*` roles while keeping `explore`/`worker` on native Antigravity OAuth.
- **`9router-hybrid`**: Routes `verify`, `jd-judge-b`, and `review-*` through a local 9Router proxy (`http://localhost:20128/v1`, `NINEROUTER_BASE_URL` / optional `NINEROUTER_API_KEY` on loopback).
- **`omniroute-hybrid`**: Routes `verify`, `jd-judge-b`, and `review-*` through a local OmniRoute proxy (`http://localhost:20128/v1`, `OMNIROUTE_BASE_URL` / optional `OMNIROUTE_API_KEY` on loopback).

### Provider templates & fallback chains

`.agents/odd/profiles.json` ships ready-to-use templates for `deepseek`, `9router`, `omniroute`, and generic `openai-compatible` endpoints (OpenRouter, OpenAI, Ollama, LM Studio, etc.) with custom `headers`, `apiKeyEnv`, `baseUrlEnv`, and ordered per-role `fallback` chains that cleanly fall back to `agy-oauth` on missing keys, `429`, or unreachable endpoints.

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
│   ├── config.json     # delegationMode, rddEnabled, installMode, binaryCommand
│   ├── profiles.json   # activeProfile, providers, per-role routes & fallbacks
│   └── .managed-manifest.json
├── hooks.json          # merged PreToolUse, PostToolUse, SessionStart, PreInvocation, Stop hooks
└── mcp_config.json     # merged odd-agy stdio MCP server (+ engram when present on PATH)
```

### Unified MCP Server (`odd-agy mcp`)

- **Todo**: `todo` (`write`, `add`, `update`, `list`, `clear`) persisted under `.git/odd-agy/todo.json`.
- **Frozen-Snapshot RDD**: `odd_review_mode`, `odd_review_assess`, `odd_review_start`, `odd_review_consent`, `odd_review_scope` (paginated + SHA-256 verified), `odd_review_submit` (4R lenses + refuter, max 1 correction), `odd_review_apply_correction`, `odd_review_status`, `odd_review_ack`.
- **Sandboxed External Runner**: `odd_external_profiles`, `odd_external_run`, `odd_external_continue`, `odd_external_status` — enforces `## Allowed edit surfaces` in code, blocks secrets/destructive commands/`git push`, and persists `reasoning_content` history under `.git/odd-agy/tasks/<taskId>.json`.

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

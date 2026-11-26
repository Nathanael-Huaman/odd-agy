#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/odd-agy-live-XXXXXX")"

cleanup() {
  rm -rf "$TMP_DIR"
}
trap cleanup EXIT

if ! command -v agy >/dev/null 2>&1; then
  echo "error: 'agy' (Antigravity CLI) not found on PATH" >&2
  exit 1
fi

AGY_VERSION="$(agy --version)"
echo "Found agy: ${AGY_VERSION}"

BIN_PATH="${TMP_DIR}/odd-agy"
echo "Building odd-agy into ${BIN_PATH}..."
(cd "$REPO_ROOT" && go build -o "$BIN_PATH" ./cmd/odd-agy)

WORKSPACE="${TMP_DIR}/workspace"
mkdir -p "$WORKSPACE"
git -C "$WORKSPACE" init -q
git -C "$WORKSPACE" config user.email "live-smoke@example.com"
git -C "$WORKSPACE" config user.name "Live Smoke"
printf '# Smoke Workspace\n' > "${WORKSPACE}/README.md"
git -C "$WORKSPACE" add README.md
git -C "$WORKSPACE" commit -q -m "initial commit"

echo "Running odd-agy init in temporary workspace ${WORKSPACE}..."
"$BIN_PATH" --cwd "$WORKSPACE" init --mode=standalone --binary="$BIN_PATH"

echo "Running odd-agy doctor..."
"$BIN_PATH" --cwd "$WORKSPACE" doctor

for req in \
  "${WORKSPACE}/.agents/rules/odd-01-core.md" \
  "${WORKSPACE}/.agents/agents/odd-orchestrator.md" \
  "${WORKSPACE}/.agents/skills/odd-status/SKILL.md" \
  "${WORKSPACE}/.agents/hooks.json" \
  "${WORKSPACE}/.agents/mcp_config.json" \
  "${WORKSPACE}/.agents/odd/config.json" \
  "${WORKSPACE}/.agents/odd/profiles.json"; do
  if [[ ! -f "$req" ]]; then
    echo "error: expected initialized file missing: $req" >&2
    exit 1
  fi
done

echo "Checking Antigravity CLI project-scoped discovery via agy acp (zero LLM calls, no ~/.gemini mutations)..."
if command -v python3 >/dev/null 2>&1; then
  python3 - "$WORKSPACE" <<'PY'
import json
import os
import select
import subprocess
import sys
import time

workspace = sys.argv[1]
expected_skills = [
    "odd-status",
    "odd-rdd",
    "odd-profile",
    "odd-doctor",
    "work-unit-commits",
    "judgment-day",
    "branch-pr",
    "chained-pr",
    "issue-creation",
    "comment-writer",
    "cognitive-doc-design",
    "gentle-ui-design",
    "skill-creator",
    "skill-improver",
    "rdd-defect-workflow",
]

proc = subprocess.Popen(
    ["agy", "acp"],
    cwd=workspace,
    stdin=subprocess.PIPE,
    stdout=subprocess.PIPE,
    stderr=subprocess.PIPE,
    bufsize=0,
)

try:
    init_req = {
        "jsonrpc": "2.0",
        "id": 1,
        "method": "initialize",
        "params": {"protocolVersion": 1},
    }
    proc.stdin.write((json.dumps(init_req) + "\n").encode("utf-8"))
    proc.stdin.flush()

    fd = proc.stdout.fileno()
    buf = bytearray()
    deadline = time.monotonic() + 15.0
    sent_session_new = False
    discovered_commands = None

    while time.monotonic() < deadline and discovered_commands is None:
        remaining = max(0.05, deadline - time.monotonic())
        readable, _, _ = select.select([fd], [], [], min(0.5, remaining))
        if not readable:
            if proc.poll() is not None:
                break
            continue

        chunk = os.read(fd, 65536)
        if not chunk:
            break
        buf.extend(chunk)

        while b"\n" in buf:
            nl_idx = buf.index(b"\n")
            raw_line = bytes(buf[:nl_idx]).decode("utf-8", errors="replace").strip()
            del buf[: nl_idx + 1]
            if not raw_line:
                continue

            try:
                msg = json.loads(raw_line)
            except json.JSONDecodeError:
                continue

            if isinstance(msg, dict) and msg.get("error") is not None:
                sys.stderr.write(f"error: agy acp returned JSON-RPC error: {msg['error']}\n")
                sys.exit(1)

            if msg.get("id") == 1 and not sent_session_new:
                sent_session_new = True
                sess_req = {
                    "jsonrpc": "2.0",
                    "id": 2,
                    "method": "session/new",
                    "params": {"cwd": workspace, "mcpServers": []},
                }
                proc.stdin.write((json.dumps(sess_req) + "\n").encode("utf-8"))
                proc.stdin.flush()

            params = msg.get("params")
            if isinstance(params, dict):
                update = params.get("update")
                if isinstance(update, dict) and update.get("sessionUpdate") == "available_commands_update":
                    cmds = update.get("availableCommands")
                    if isinstance(cmds, list):
                        discovered_commands = [
                            c.get("name") for c in cmds if isinstance(c, dict) and isinstance(c.get("name"), str)
                        ]
                        break

    if discovered_commands is None:
        stderr_tail = ""
        try:
            proc.terminate()
            _, err_bytes = proc.communicate(timeout=3)
            stderr_tail = err_bytes.decode("utf-8", errors="replace").strip()
        except Exception:
            proc.kill()
        sys.stderr.write("error: timed out waiting for available_commands_update from 'agy acp'\n")
        if stderr_tail:
            sys.stderr.write(f"agy acp stderr:\n{stderr_tail}\n")
        sys.exit(1)

    discovered_set = set(discovered_commands)
    missing = [s for s in expected_skills if s not in discovered_set]
    if missing:
        sys.stderr.write(
            f"error: agy acp missed {len(missing)}/{len(expected_skills)} project skills: {', '.join(missing)}\n"
        )
        sys.exit(1)

    print(
        f"Verified agy acp project discovery: all {len(expected_skills)}/15 project skills "
        f"discovered ({len(discovered_commands)} total available commands)."
    )
finally:
    if proc.poll() is None:
        proc.terminate()
        try:
            proc.wait(timeout=3)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait()
PY
else
  echo "Skipping 'agy acp' JSON-RPC discovery check (python3 not found on PATH)."
fi

echo "Running live smoke check of odd-agy hook SessionStart in ${WORKSPACE}..."
HOOK_OUT="$(printf '{"workspacePaths":["%s"]}\n' "$WORKSPACE" | (cd "$WORKSPACE" && "$BIN_PATH" hook SessionStart))"
printf '%s\n' "$HOOK_OUT"
if ! printf '%s' "$HOOK_OUT" | grep -q '"injectSteps"' || ! printf '%s' "$HOOK_OUT" | grep -q 'ODD harness active'; then
  echo "error: odd-agy hook SessionStart did not return expected injectSteps payload" >&2
  exit 1
fi

echo "Running live smoke check of odd-agy mcp (initialize + tools/list) in ${WORKSPACE}..."
MCP_OUT="$(
  printf '%s\n%s\n' \
    '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"e2e-live-agy","version":"1.0.0"}}}' \
    '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}' \
    | "$BIN_PATH" --cwd "$WORKSPACE" mcp
)"
for expected_tool in '"todo"' '"odd_review_assess"' '"odd_external_run"'; do
  if ! printf '%s' "$MCP_OUT" | grep -q "$expected_tool"; then
    echo "error: odd-agy mcp tools/list missing expected tool ${expected_tool}" >&2
    printf '%s\n' "$MCP_OUT" >&2
    exit 1
  fi
done
echo "Verified odd-agy mcp exposes todo, odd_review_assess, and odd_external_run."

echo "Hint: inside an interactive 'agy' session, inspect project customizations with /agents, /hooks, /mcp, and /skills."
echo "Live Antigravity CLI smoke check completed cleanly (temporary workspace will be removed)."

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

echo "Checking Antigravity CLI project discovery (read-only, no ~/.gemini mutations)..."
AGENT_OUT="$(cd "$WORKSPACE" && agy agent 2>&1 || true)"
printf '%s\n' "$AGENT_OUT"
if ! printf '%s\n' "$AGENT_OUT" | grep -q "odd-orchestrator"; then
  echo "warning: 'agy agent' did not list odd-orchestrator in stdout (check agy version/project discovery)" >&2
fi

MCP_OUT="$(cd "$WORKSPACE" && agy mcp list 2>&1 || true)"
printf '%s\n' "$MCP_OUT"

echo "Live Antigravity CLI smoke check completed cleanly (temporary workspace will be removed)."

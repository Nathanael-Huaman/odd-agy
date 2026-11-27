---
trigger: model_decision
description: Test-first RED/GREEN/PRESERVE discipline, writer self-review, parent spot check, independent gentle-ai-verify handoff, and correction bounds.
---

# ODD Verification & Test-First Discipline

## 1. Test-First Cycle (RED -> GREEN -> PRESERVE -> REFACTOR)

1. **RED**: Write behavior-level tests asserting public outputs (exit code, stdout, stderr, persisted state, error cases) and capture the observed failure before changing production code.
2. **GREEN**: Implement the minimum clean change and capture the focused tests passing.
3. **PRESERVE**: Whenever changes touch shared options, parsers, helpers, or validation, add a regression test proving prior behavior holds and run a differential check comparing base behavior vs. new behavior on existing commands.
4. **REFACTOR**: Improve clarity only while focused and full tests remain green.

If test-first is inapplicable (e.g., pure documentation or missing runner), state the exact justification and run proportionate structural/functional checks.

## 2. Layered Verification Gates

- **Writer self-review**: Workers run every command under `## Verification` in the foreground and report exact `<command>: <observed result>`.
- **Parent spot check**: Orchestrator inspects `files_changed`, `tdd_evidence`, `validation`, and `risks` before marking any `T<n>` complete.
- **Independent verification (`gentle-ai-verify`)**: Required for High-Risk rubric items 1-6 or cross-cutting invariants. The verifier checks every `S#` on isolated state (`mktemp -d`) without editing repository files.

## 3. Correction Limit & Preparation Failures

- **1-correction limit**: Allow at most **1 bounded correction** attempt when a verification check fails (plus 1 setup fix if a brand-new test harness fails to run). If the check still fails after one real fix, stop, mark `partial`, and surface the exact command, output, and diagnosed root cause.
- **Preparation failures**: If a verifier fails due to environment setup (missing binary, stale lock, broken temp state), report the preparation failure explicitly with evidence rather than masking it as a pass or silently bypassing verification.

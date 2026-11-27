---
name: judgment-day
description: Run the dual blind adversarial Judgment Day review protocol (jd-judge-a, jd-judge-b, jd-fix-agent) until defects converge.
---

# `/judgment-day` — Dual Blind Adversarial Review

Execute an independent two-judge adversarial audit on a feature, PR diff, or high-risk change:

1. **Dispatch blind judges in parallel**:
   - Invoke `jd-judge-a` and `jd-judge-b` independently (via `invoke_subagent`, or `odd_external_run` when routed to external models in `.agents/odd/profiles.json`).
   - Supply the target diff/branch, `odd/tasks/<feature>.md` `## Specs`, and exact verification commands. Never share Judge A's output with Judge B.
2. **Synthesize findings**:
   - Deduplicate `CRITICAL`, `WARNING`, and `SUGGESTION` findings with exact `path:line` evidence.
   - Discard speculative noise only when refuted by concrete code evidence.
3. **Surgical remediation**:
   - Delegate confirmed `CRITICAL` / `WARNING` fixes to `jd-fix-agent` with strict `## Allowed edit surfaces` and test-first regression tests.
4. **Re-verify**:
   - Re-run verification and confirm zero open `CRITICAL` defects remain (max 2 remediation rounds before escalating to the user via `ask_question`).

---
name: rdd-defect-workflow
description: Resolve severe candidate-caused findings surfaced by RDD review within the single bounded correction budget and re-submit.
---

# `/rdd-defect-workflow` — Bounded RDD Defect Resolution

Handle `severe` candidate-caused findings returned by `odd_review_submit`:

1. **Inspect frozen findings**: Call `odd_review_status` and read each severe finding (`origin: "candidate"`). Pre-existing advisories (`origin: "pre-existing"`) do not block acceptance.
2. **Optional adversarial refutation**: If a severe finding appears false or pre-existing, dispatch `review-refuter` against `odd_review_scope` before editing code.
3. **Single bounded correction**:
   - Within the 1-correction budget, delegate a surgical fix to `gentle-ai-worker` with exact `## Allowed edit surfaces` and test-first regression coverage.
   - Re-run authorized verification (`go test`, etc.).
4. **Re-assess & conclude**: Start/update the frozen candidate review for the amended diff or escalate to the user via `ask_question` if the correction budget is exhausted.

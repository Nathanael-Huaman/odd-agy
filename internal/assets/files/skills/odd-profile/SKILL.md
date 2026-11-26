---
name: odd-profile
description: List, inspect, or switch hybrid Antigravity OAuth and OpenAI-compatible model routing profiles (DeepSeek, 9Router, OmniRoute).
---

# `/odd-profile` — Model Routing Profile Manager

Inspect or switch `.agents/odd/profiles.json` routing profiles safely:

1. Run `odd-agy profile list`, `odd-agy profile show`, and `odd-agy profile providers` via `run_command` to display available profiles (`agy-oauth`, `deepseek-hybrid`, `9router-hybrid`, `omniroute-hybrid`) and provider endpoints.
2. When switching profiles, ask the user via `ask_question` if not already specified, then execute `odd-agy profile use <name>` (or launch `odd-agy ui` for interactive per-role model and provider tuning).
3. Remind the user that `agy-oauth` keeps all roles on native Antigravity OAuth without external subprocesses, while hybrid profiles route configured roles through `odd_external_run` with fallback to `agy-oauth`.

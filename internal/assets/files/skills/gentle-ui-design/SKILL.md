---
name: gentle-ui-design
description: Design accessible, cohesive CLI/TUI and web UI flows with clear hierarchy, keyboard navigation, and state feedback.
---

# `/gentle-ui-design` — Deliberate UI & TUI Design

Craft clean, predictable user interfaces (including Bubble Tea TUIs and web components):

1. **Clarify user flow first**: Identify primary actions, keyboard shortcuts, empty/loading/error states, and non-interactive equivalents before writing layout code.
2. **Visual & ergonomic principles**:
   - Establish clear visual hierarchy, consistent spacing, and high-contrast status cues.
   - Support full keyboard navigation (`Tab`, arrows, `Enter`, `Esc`, `q`) and deterministic state updates.
   - Surface validation errors inline without losing user input.
3. **Testability**: Verify UI state transitions (`Update`/`View` or component tests) alongside accessibility and terminal-width bounds.

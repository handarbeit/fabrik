# ADR 024: OSC 8 Hyperlinks Over Mouse Capture

**Status**: Accepted  
**Date**: 2026-04-13

## Problem

The Fabrik TUI used `tea.WithMouseCellMotion()` to capture mouse events for row selection and scroll forwarding. This option captures all mouse events at the terminal protocol level (via the `1002` mouse mode escape sequence), which prevents OSC 8 hyperlinks from being clickable. When mouse capture is active, the terminal delivers clicks to the application rather than letting the OS open the hyperlink in a browser.

As a result, the board title in the footer was rendered with an OSC 8 hyperlink but could not be activated by clicking — the click was intercepted and treated as a row selection event instead.

## Decision

Remove `tea.WithMouseCellMotion()` from both `cmd/root.go` (main TUI) and `watch/model.go` (watch view). Do not add any alternative mouse capture mechanism.

Add OSC 8 hyperlinks to `#NNN` issue numbers in the history and active/in-progress panels so they open the corresponding GitHub issue in the browser when clicked in a supporting terminal.

## Consequences

### What we gain

- Issue numbers (`#NNN`) in the In Progress and History panes are clickable links to the GitHub issue page in terminals that support OSC 8 (Ghostty, iTerm2, WezTerm, Kitty).
- The board title in the footer becomes a working clickable link (it was already rendered as OSC 8 but was blocked by mouse capture).
- No application-level click handling needed — the terminal OS handles link opening natively.

### What we lose

- Mouse-based row selection in the In Progress and History panes. Users must use `Up`/`Down`/`j`/`k` to navigate.
- Mouse wheel scrolling in the history viewport. Users must use arrow keys or `Page Up`/`Page Down`.
- Click-to-switch-tab in the watch view (`fabrik watch`). Users must use `Left`/`Right` arrow keys.

All lost functionality has direct keyboard equivalents that were already implemented and documented.

### Terminals affected

OSC 8 links are silently invisible in unsupported terminals — `supportsOSC8()` gates injection, so no escape sequences are emitted where they would show as garbage. There is no user-visible regression in non-supporting terminals.

## Implementation Constraints

**lipgloss strips OSC 8**: `github.com/charmbracelet/lipgloss v1.1.0` removes OSC 8 escape sequences on any `Style.Render()` call. OSC 8 must be injected **after** all lipgloss rendering — using `strings.Replace(styledLine, "#NNN", termenv.Hyperlink(url, "#NNN"), 1)` on the already-styled string. This is the same pattern used in `renderWithOSC8` in `tui/footer.go` and must be followed consistently.

**Future mouse support**: Re-enabling mouse support would require an alternative approach that does not use `WithMouseCellMotion`. The `1006` extended mouse mode or bracketed paste mode may allow co-existing with OSC 8 in some terminals, but this is not explored here. Any future contributor adding mouse support must verify OSC 8 compatibility.

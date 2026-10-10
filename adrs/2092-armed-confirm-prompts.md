# ADR 2092: Armed TUI confirm prompts are a derived banner, self-cancelling, and cannot stop the daemon unseen

## Status

Accepted

## Context

Report #1948: the TUI armed its confirms (`u` upgrade/reconcile, `s` stop, the `OVERWRITE` typed confirm) by writing the prompt into the header's single status message. The header lays out title, timer and badges first and gives the status the leftover width, so at 80 columns the `[y/N]` or `[1]/[2]/[3]` answer keys were cut off and below ~60 columns the prompt vanished. The quit and clear-all prompts lived in the History pane's title line and were truncated the same way.

An armed confirm also survived every unrelated key and had no timeout. The reconcile answer `1` returned `tea.Quit` directly, and `runTUI` (`cmd/root.go`) SIGTERMs the engine on **every** TUI exit — so a stray `1` stopped the daemon, bypassing the active-workers confirmation that `q` requires.

## Decision

1. **One derived prompt, rendered on its own line.** `confirmPrompt()` (`tui/confirm.go`) builds the text of whichever confirm is armed from the `confirm*` flags; nothing else builds prompt text. `Model.View` renders it as a word-wrapped banner directly under the header, with a dynamic height that `updateLayout` subtracts (`confirmBannerHeight()`). The header is untouched, so badges, the timer and terminal width cannot truncate a prompt. The quit and clear-all prompts moved out of the History pane (`SetLayout` lost its `confirmQuit`/`activeCount` arguments). Header priority was rejected: the prompts run 27–119 characters and the header can offer ~32 columns at 80 columns and none at 60.
2. **Cancel and consume.** A pre-step in `Update` cancels the armed confirm on any key that `isConfirmAnswer` rejects, and the key does nothing else. Predictability beats saving a keystroke on a safety path. `ctrl+c` is never intercepted. For `OVERWRITE`, a key is an answer only if it keeps `typed+key` a prefix of the word (case-sensitive) or is esc/backspace/ctrl+h/delete.
3. **10 s timeout on `TickEvent.At`.** `Update` wraps `update` and restamps `confirmArmedAt` whenever the confirm's signature (kind + detail) changes — observed rather than set at each arming site, which also covers the confirm the history pane arms itself. Typing an `OVERWRITE` letter restarts the clock. Expiry runs after the header's tick handling, so a prompt cannot revive itself.
4. **Only one confirm at a time.** `armConfirm` clears every other confirm first.
5. **R3: `1` goes through the active-workers confirmation.** Because every TUI exit stops the engine and changing `runTUI` is out of scope, `1` with jobs running arms the quit confirm in a "quit for reconciliation" variant; `pendingReconcilePrompt` is set only when `q` confirms. With no jobs, `1` quits immediately, exactly as `q`/`ctrl+c` do.

## Consequences

- A new confirm must be added to `armedKind`, `confirmPrompt`, `isConfirmAnswer` and `armConfirm`; it then inherits visibility, cancellation and the timeout.
- A navigation key pressed while a prompt is armed dismisses the prompt without scrolling.
- Typing a letter outside the `OVERWRITE` prefix cancels immediately instead of after nine characters.
- #1948 is mentioned for context only; it is closed by hand once a release containing the fix ships.

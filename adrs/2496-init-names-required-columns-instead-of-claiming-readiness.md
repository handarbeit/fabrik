# ADR 2496: `fabrik init` names the required columns instead of claiming readiness

## Status
Accepted

## Context
`fabrik init` without `--create-board` ended with "Fabrik is ready" although nothing had created or checked the board's Status columns; a mismatch then failed the first run in `checkStageColumnAlignment`. `--create-board` and `repair-board` (ADR 1714) already fix this, but `init` mentioned neither.

## Decisions
1. **Branch on `--create-board`.** That path keeps its closing lines verbatim. Every other path (plain, `<project-url>`, `--github-app` alone, user-owned) prints the column list and what remains, and never says "ready".
2. **The list is `boardColumnNames` over the stage configs on disk** (`stages.LoadAll`), so skipped existing files and customised sets count. It is a deliberate **superset** of the startup requirement: the startup check skips cleanup and unmanaged stages and treats a missing holding column as fatal only with `merge_train: on`, while `boardColumnNames` drops only unmanaged stages. The relationship is documented next to the print, not claimed to be identity.
3. **AC1 via a test seam, not a refactor.** `engine.StartupRequiredColumnsForTest` runs the real `checkStageColumnAlignment` once per stage against a board missing that one column, so `startup.go` is untouched and a change to its rule changes the answer. `cmd`'s `TestInitColumnsCoverStartupCheck` drives printer and check from one fixture.
4. **Offline.** Only local files are read. "Board configured" is read from `.fabrik/config.yaml` (`config.LoadProjectConfig`), which covers a URL, the interactive prompt and an existing config alike; a read error shows the `--create-board` hint (harmless advice, no false claim). `owner_type: user` selects the create-by-hand wording, since `--create-board` and `repair-board --apply` refuse user-owned boards (#770).
5. **An unreadable stage file does not fail `init`.** The files are already written; `init` warns, skips the list, and still makes no readiness claim.

## Consequences
The printed list can name `Done`/`Queued` even where the startup check would only warn. Changing either rule is caught by `TestInitColumnsCoverStartupCheck`.

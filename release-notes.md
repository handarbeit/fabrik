# Fabrik v0.0.5

## Bug Fixes

### Issue Body Updates No Longer Silently Lost (#205, #212)

When a stage (typically Specify) emits `FABRIK_ISSUE_UPDATE_BEGIN/END` markers in an early assistant turn — before additional tool calls — and then emits `FABRIK_STAGE_COMPLETE` in a later turn, the engine was silently dropping the issue body update. The `result` field in Claude's JSON output only contains the text from the **final** assistant turn, so updates written earlier were invisible to the engine.

The fix scans all assistant turns in the raw output stream for the last `FABRIK_ISSUE_UPDATE` block and applies it. This resolves a long-standing frustration where the spec appeared unchanged after Specify ran successfully.

### Done History No Longer Repeats the Same Issue (#124, #213)

The Done pane in the TUI was showing the same completed issue repeated many times. Two complementary fixes: a worktree existence guard in the dispatch loop now prevents Done items without an active worktree from being re-dispatched (the root cause), and a deduplication layer in TUI history collapses duplicate entries as a defensive backstop. Together, they stop the "repeating Done cleanup loop" that accumulated history entries across poll cycles.

## Upgrading

```bash
# From a previous release binary
fabrik upgrade

# Or download directly
gh release download --repo handarbeit/fabrik --pattern '*.tar.gz' -O - | tar xz
```

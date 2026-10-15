# ADR 2234: Dirty dev builds rebuild by SHA, and every self-upgrade check logs its outcome

**Date**: 2026-10-14
**Status**: Accepted
**Issue**: #2234 — a `+dirty` dev build never rebuilds once its checkout has been pulled

## Context

`extractBinarySHA` accepted only `dev(<sha>)`. A build from a tree with uncommitted or untracked
files is stamped `dev(<sha>)+dirty` (`cmd/version.go`), so it returned `""`, `CompareDevBuild`
skipped the binary-vs-HEAD check, and a checkout already pulled to `origin/main` by something
else compared as "up to date". Nothing was rebuilt and nothing was logged. On 2026-10-13 a dev
daemon kept running a stale `dev(ca4c0a2)+dirty` binary because of one untracked file. The code
is shared with Pruefer (ADR-1196), which had the same defect.

## Decisions

1. **Suffix-tolerant parse.** `parseDevVersion` takes the SHA up to the first `)`; whatever follows
   is an opaque suffix. `+dirty` is recorded as `DevBuildStatus.BinaryDirty` but never affects the
   rebuild decision. An empty SHA (`dev()`) stays unparseable: every ref has `""` as a prefix, so
   accepting it would disable the staleness check.
2. **R2 — a `+dirty` build at the same SHA as HEAD is not rebuilt.** The decision keys on the SHA
   alone, so a dirty binary is compared exactly like a clean one. A rebuild at the same SHA would
   pick up no committed change and be stamped dirty again; the dirt in the checkout is what needs
   fixing, not the binary. The mitigation is visibility: the outcome line says the binary was built
   from a dirty tree at the current commit, and the checkout's dirty paths are logged (below).
3. **Outcome reasons are data, logged by `CheckAndRebuildDev`.** `CompareDevBuild` fills
   `DevBuildStatus.Reason` (not a source checkout, up to date, local ahead or diverged, plus an
   unparseable-version note and the dirty-at-current-commit note) and does not log. The read-only
   staleness reporter (`checkSourceStaleness`, ADR-1464) calls it every ~15 minutes and therefore
   gains no log output. `CheckAndRebuildDev` logs `Reason` on the not-applicable return and the
   no-rebuild `default:` branch, so every check logs exactly one outcome line. There is no
   repeat-suppression: one line per check (about one a minute on an idle board) is what R3 asks for.
4. **Dirty checkout, reported once per process.** `reportDirtyCheckout` runs `git status
   --porcelain --untracked-files=normal` (unscoped, matching why the build is stamped dirty) and
   logs up to 10 paths plus `and N more`. A package-level `atomic.Bool` makes it once per process;
   `DevBuildConfig` is rebuilt by value on every call in both daemons, so caller-owned state would
   need shared pointers in each. A clean checkout or a git error logs nothing and does not consume
   the report. `CompareDevBuild` stays free of this extra git call.

## Consequences

- Fabrik and Pruefer dev daemons both rebuild a stale `+dirty` binary whose SHA differs from HEAD.
- A dirty binary on a tree that has since been cleaned stays running at the same SHA (decision 2).
- The 30-minute external puller that produced the HEAD = origin/main state is not addressed here.

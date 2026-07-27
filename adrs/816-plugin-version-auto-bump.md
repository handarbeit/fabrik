# ADR 816: Auto-Bump `plugin/fabrik` Version on Source Change in `cut-release.sh`

**Date**: 2026-07-27
**Status**: Accepted
**Issue**: #816 — feat(release): auto-bump plugin versions on cut-release when source changed

## Context

Claude Code's `/plugin update` detects a new plugin release by comparing the cached `plugin.json` `version` field against the version at `ref: main` in `marketplace.json`. Same version number means no refresh fires, even if the plugin's source content changed. This bit the shadoworg → handarbeit migration: `plugin/fabrik/skills/*/SKILL.md` was updated to reference the new domain, but the manifest stayed at `0.1.0`. Users running `/plugin update fabrik` were told they already had the latest version and kept seeing stale content, requiring a manual `0.1.0 → 0.1.1` bump (PR #790) just to force a cache refresh.

`.claude-plugin/marketplace.json` lists exactly one plugin — `fabrik` (source path `plugin/fabrik`, `ref: main`). `plugin/fabrik-workflows` has never been listed there; it is embedded directly into the Go binary (`plugin/embed.go`) and distributed via `fabrik init`/`fabrik upgrade`, which detect changes by content hash (`ComputeEmbeddedVersion`, `plugin/known_embedded_versions.go`), not by comparing `plugin.json`'s `version` field. It is therefore not subject to the staleness bug this ADR's mechanism exists to fix, and is out of scope.

A prior prototype (PR #809, closed unmerged) implemented the detection-and-bump logic but used `jq` (a dependency this issue explicitly forbids), bumped both plugins (over-broad scope), and had no opt-out flag, no release-notes entry, and no automated test.

## Decision

Add `tools/bump-plugin-version/`, a small standalone Go program (mirroring the existing `tools/print-plugin-hash/` invocation shape and `tools/verify-release-artifact/`'s pure-function/`main()` split for offline testability) with two pure functions:

- `sourceChanged(repoDir, pluginDir, manifestPath, prevTag string) (bool, error)` — runs `git diff --name-only <prevTag>..HEAD -- <pluginDir> ':(exclude)'<manifestPath>` and reports whether the output is non-empty.
- `bumpPatchVersion(manifestPath string) (oldVer, newVer string, err error)` — extracts the sole `"version": "MAJOR.MINOR.PATCH"` field via a regexp, increments the patch component, and replaces the exact matched substring in the file's bytes. It errors if the field is missing, malformed, or appears more than once.

`main()` takes `<plugin-dir> <manifest-path> <prev-tag>`, prints `"<old> <new>"` to stdout when a bump fires, prints nothing and exits 0 when it doesn't, and exits non-zero on git or manifest errors.

`scripts/cut-release.sh` gains a new step 4b, sequenced after the existing "commit release notes" push and before the tag step:

1. `PREV_TAG` is captured via `git describe --tags --abbrev=0 --match='v*'` right before step 4 (tags were already fetched in pre-flight); empty if there is no prior tag.
2. If `--no-plugin-bump` was passed, or `PREV_TAG` is empty, the step is skipped with a warning.
3. Otherwise the tool is invoked for `plugin/fabrik`. If it reports a bump, the script appends a changelog line (`- Auto-bumped fabrik plugin to <new> (source changed since <prevTag>)`) to that release's `release-notes/<version>.md`, then commits `plugin.json` and the notes file together as `arbeithand <handarbeit@handarbeit.io>` (reusing the exact `GIT_AUTHOR_*`/`GIT_COMMITTER_*` + author-verification + credential-helper-nuked PAT-in-URL push pattern already used for the release-notes commit), and pushes to `main` before the tag is created.

The pre-flight `DIRTY=` allowlist is extended to also permit a modified `plugin/fabrik/.claude-plugin/plugin.json`, alongside the existing `release-notes/<version>.md` and `plugin/known_embedded_versions.go` entries.

## Rationale

### Why Go, not bash/awk/jq, for the detection and patch logic?

The issue explicitly forbids requiring the standalone `jq` binary — PR #809's prototype used it for both reading and writing `version`. Research found no existing shell-test harness in this repo (no `scripts/lib/`, no `*_test.sh`), and the issue requires at least one automated test runnable without GitHub access. Putting both the diff-detection and the version-patch logic in one small Go tool — following `tools/verify-release-artifact`'s pure-function/`main()` split, the strongest existing precedent for exactly this shape of git-repo-dependent tool test — is the only option that satisfies "no `jq`" and "testable offline" without inventing new shell-testing infrastructure.

### Why a targeted string replace instead of a full `encoding/json` marshal round-trip?

`encoding/json` is used only implicitly, via the regexp validating the extracted version's shape. The actual mutation replaces the exact `"version": "<old>"` substring, erroring if it doesn't appear exactly once. A full unmarshal/marshal round-trip would risk reformatting the entire file (key order, indentation, trailing newline) for what should be a single-field patch — unnecessary blast radius, and it would make the resulting diff noisy for reviewers of the auto-generated commit.

### Why a second commit for the release-notes changelog entry, not an amend?

By the time step 4b runs, `release-notes/<version>.md` has already been committed and pushed in step 4 (its own separate push, which the script has always done this way). Amending that pushed commit would require a force-push, which the script never does anywhere else and which would violate the same non-destructive philosophy documented at the top of the script ("On failure after the tag is pushed, the script does NOT auto-clean"). A second commit, touching both `plugin.json` and the notes file together, is consistent with the script's existing pattern of sequential, non-destructive bot commits — the same reasoning already applies to the existing two-commit sequence for `known_embedded_versions.go` vs. `release-notes/<version>.md` within step 4 itself (both staged and committed together there, but that's a single commit since both are new/dirty at the same point — here the notes file is already committed, forcing a second one).

### Why anchor the changelog insertion on the `## Internal` heading rather than the section's end?

Locating a markdown section's *end* boundary reliably in `awk` is fragile (the next `##` heading, or EOF, or edge cases with trailing whitespace). Anchoring on the heading line itself is a single well-defined match. Bullet order within a changelog section doesn't matter, so inserting immediately after the heading (or appending a new `## Internal` section if none exists) is sufficient and mirrors the existing `known_embedded_versions.go` insert-before-closing-brace `awk` pattern already used earlier in the same script.

### Why is `plugin/fabrik-workflows` out of scope?

It is not listed in `.claude-plugin/marketplace.json` and is never installed via `/plugin update` — it ships embedded in the Fabrik binary and is refreshed by `fabrik init`/`fabrik upgrade` via independent content-hash-based change detection (`plugin/embed.go`, `plugin/known_embedded_versions.go`). It was never actually subject to the staleness bug this ADR's mechanism exists to fix, so bumping it here (as PR #809's prototype did) would be scope creep with no corresponding user-facing benefit.

### Why patch-only, not automatic minor/major bumps?

A patch bump is sufficient to force `/plugin update`'s cache-invalidation comparison to see a different version; it makes no claim about the *size* of the underlying change. Minor/major version semantics (breaking changes, new capabilities) require human judgment about what the plugin's version number should communicate to users, which an automated diff-presence check cannot infer. This remains a manual override: nothing in this mechanism prevents someone from hand-editing `plugin.json` to a minor/major bump before running `cut-release.sh`, at which point the diff since that new commit (once tagged) naturally resets.

## Consequences

**Positive:**
- Every release that touches `plugin/fabrik`'s source now force-invalidates `/plugin update`'s cache automatically — the exact failure mode from the shadoworg → handarbeit migration cannot recur silently.
- No new external dependency (`jq` avoided entirely; the tool uses only the Go standard library plus the `git` binary already required elsewhere in the script).
- The bump is visible in the GitHub Release changelog via the auto-appended `## Internal` line, so it isn't a silent commit a maintainer has to notice separately.
- `--no-plugin-bump` gives an explicit escape hatch for hotfix releases where the extra commit is undesirable.
- Fully covered by offline unit tests (`tools/bump-plugin-version/main_test.go`) exercising both the "bump fires" and "no bump" paths in real temporary git repositories, independent of the rest of `cut-release.sh` (which needs `.env`/`FABRIK_TOKEN`/`gh`/network and can't be exercised offline).

**Negative / Trade-offs:**
- **Double-push failure window**: two separate bot commits (release notes, then plugin bump) are now pushed to `main` in sequence before the tag. If the second push fails after the first succeeds, `main` has a release-notes commit but no plugin-bump commit; the script's `die` message for this step names the exact state and gives explicit recovery commands (push the still-local commit manually, or discard it and retry), consistent with the script's existing no-auto-clean philosophy — this is a message-quality mitigation, not a logic fix, since the script already fails loudly and stops before tagging either way.
- **No live end-to-end test of `cut-release.sh` itself** (it requires `FABRIK_TOKEN`/`gh`/network to run for real) — coverage relies on the Go tool's unit tests plus a `bash -n` syntax check of the script; this mirrors the same accepted limitation noted in ADR 071 for this script's other guards.
- A previous plugin bump commit is excluded from the next release's change-detection diff (the manifest path is pathspec-excluded), so this cannot recursively re-trigger itself — verified directly by `TestSourceChanged_ManifestOnlyChangeExcluded` and the end-to-end test in `main_test.go`.

## Testing

`tools/bump-plugin-version/main_test.go` builds real temporary git repos (via the real `git` binary, guarded by the project's standard `skipIfNoGit` convention) and covers:
- `sourceChanged` returns `true` when a non-manifest file under `plugin/fabrik` changed since the tag.
- `sourceChanged` returns `false` when only files outside `plugin/fabrik` changed.
- `sourceChanged` returns `false` when only the manifest itself changed (pathspec exclusion working as intended).
- `sourceChanged` errors on an empty `prevTag`.
- `bumpPatchVersion` correctly increments `0.2.0 → 0.2.1` and leaves the rest of the manifest untouched.
- `bumpPatchVersion` errors on a malformed, missing, or duplicated `version` field.
- An end-to-end scenario mirroring two consecutive release cycles: a plugin-source change triggers a bump and tag, and a subsequent unrelated change does not re-trigger a bump (guarding against the manifest-exclusion regression described above).

`scripts/cut-release.sh` was syntax-checked with `bash -n`; the new step 4b logic was reviewed by inspection against the existing step 4/5 patterns it reuses, since a full run requires bot credentials and network access unavailable in this environment.

## Related Work

- PR #790 — the original manual `0.1.0 → 0.1.1` bump this ADR's automation makes unnecessary going forward.
- PR #809 — closed, unmerged prototype of `bump_plugin_if_changed()`. Confirmed the mechanism's overall shape but used `jq`, bumped both plugins, and lacked the opt-out flag, release-notes entry, and tests this ADR's implementation adds.
- ADR 071 (`071-release-artifact-vcs-verification.md`) — the precedent for a comparably-scoped `cut-release.sh` guard, including the `DIRTY=` allowlist rationale this ADR extends to a third file.
- The Plan for this issue also called for a best-effort sync of `.claude/skills/cut-release/SKILL.md`'s step list and flags list. That edit was attempted and skipped during Implement — a harness permission policy denied modification of files under `.claude/skills/`, the same limitation ADR 071's Implement hit. `docs/USER_GUIDE.md` alone carries the mechanism's documentation; it is the canonical doc the issue requires.

**References:** [docs/USER_GUIDE.md §Built-in Skill: `/cut-release`](../docs/USER_GUIDE.md)

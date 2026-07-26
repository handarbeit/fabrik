# ADR 071: Release-Artifact VCS Verification via goreleaser Build Hooks

**Date**: 2026-07-26
**Status**: Accepted
**Issue**: #1070 — release: guard against publishing artifacts built from a dirty tree

## Context

The published `v0.0.75` release artifact was built from a dirty git working tree — its embedded Go build info recorded `vcs.modified=true`. The binary corresponded to no committed revision: not reproducible (rebuilding the tagged commit would not reproduce it), not auditable (`git bisect` cannot find a regression introduced by uncommitted code), and silently masked from users, since `-ldflags -X .../cmd.Version` makes `fabrik --version` print a clean version string regardless of the underlying VCS state. The dirt only surfaced via `go version -m`.

Root cause was not identified before this issue was filed, and the guard is deliberately built to not depend on identifying it: `scripts/cut-release.sh` only tags and pushes (it does not build or upload what ships), `.github/workflows/release.yml` has no tree-mutating step before the goreleaser build, and `.goreleaser.yaml` had no hooks at all. Because the source of the dirt is unknown, the only reliable guard is one that inspects the **built artifact** itself, not the working tree at any single point in time.

Separately, issue #1073 (merged the day before this issue was filed) added `/dist/` to `.gitignore`, on the theory that goreleaser's own untracked `dist/` output directory was itself making the tree look dirty to Go's VCS stamping during the CI build. That fix is plausible and was independently reproduced during Research, but unverified in production — no release has been cut since v0.0.75. This guard is the safety net for the case where #1073 turns out to be incomplete, or a different cause recurs.

## Decision

Add `tools/verify-release-artifact/`, a small standalone Go program (mirroring the existing `tools/print-plugin-hash/` pattern) that reads a built binary's embedded VCS build info via `debug/buildinfo.ReadFile` — not `runtime/debug.ReadBuildInfo`, which only inspects the calling process, and not by executing the binary, which would fail for cross-arch targets (e.g. inspecting a darwin binary from the `ubuntu-latest` runner). It asserts `vcs.modified != "true"` and `vcs.revision == <expected>`, where `<expected>` is the tag's target commit, exported by `.github/workflows/release.yml` as `TAG_COMMIT` (via `$GITHUB_ENV`, in the workflow's existing "Verify tag is on main" step, which already computes this value). On failure it prints the offending binary's path, its actual `vcs.revision`/`vcs.modified`, and the expected revision, then exits non-zero.

It is wired into `.goreleaser.yaml` in two parts:

1. A global `before.hooks` entry builds the verifier once, natively (no `GOOS`/`GOARCH` override), into `.release-tools/verify-release-artifact` — **not** `dist/verify-release-artifact`. goreleaser requires `dist/` to be empty when it enters the build phase (confirmed empirically: writing into `dist/` from a `before.hooks` step under `--clean` produced `error=dist is not empty, remove it before running goreleaser or use the --clean flag`, even though `--clean` had already run). `.release-tools/` is a new directory outside goreleaser's own managed output tree.
2. Each `builds[].hooks.post` entry invokes that already-built native binary — `./.release-tools/verify-release-artifact {{ .Path }} {{ .Env.TAG_COMMIT }}` — once per matrix target, immediately after that target's binary is compiled and before it is archived, checksummed, or published.

`.release-tools/` is gitignored, with a comment explicitly cross-referencing the `/dist/` entry from #1073: if the verifier's own build output were left as an untracked file in the working tree, its mere presence would itself make every subsequently-built `fabrik` binary report `vcs.modified=true` — reproducing the exact bug this issue exists to guard against, self-inflicted by the guard's own tooling. This was caught and fixed during Implement, before any CI run, by reasoning through the ordering: `before.hooks` runs *before* the matrix builds, so anything it leaves untracked in the tree is dirt the matrix builds will themselves pick up.

## Rationale

### Why per-build (`builds[].hooks.post`) hooks, not global `before:`/`after:`?

`before:` hooks run before any binary exists — nothing to verify yet. `after:` hooks run after the *entire* release, including archiving, checksumming, and publishing — too late; the requirement is to fail before any artifact is published. Per-build post hooks are the only integration point that is both per-target (satisfying "check every binary in the matrix, not just one") and strictly between "binary compiled" and "binary archived."

### Why build the verifier once natively instead of `go run`-ing it inline in each per-build hook?

`builds[].hooks.post` inherits that target's build environment — `GOOS`, `GOARCH`, `CGO_ENABLED=0` from `.goreleaser.yaml`'s `builds[].env`. Invoking `go run ./tools/verify-release-artifact` directly from inside that hook would try to cross-compile *and execute* the verifier itself under the target's `GOOS`/`GOARCH` — e.g. attempting to run a `darwin/arm64` binary on the `ubuntu-latest` CI runner, an immediate `exec format error` unrelated to the actual guard logic. This is not a hypothetical: it is the direct consequence of how goreleaser's per-build hook environment is scoped, and it would have silently broken the guard for exactly the cross-arch targets the guard most needs to cover (CI cannot execute darwin binaries). Building the verifier once, natively, in the neutral `before.hooks` environment — which runs before any matrix `GOOS`/`GOARCH` override is set — sidesteps this class of failure entirely. An alternative considered and rejected: resetting `GOOS=`/`GOARCH=` inline within the per-build hook command to force host defaults. This works but is fragile (depends on Go's empty-env-var-means-host-default behavior continuing to hold) and would recompile the same tool four times instead of once.

### Why pass `TAG_COMMIT` explicitly instead of relying on `github.sha`?

For a lightweight tag (which is what `cut-release.sh` creates — `git tag "$VERSION" "$TAG_COMMIT"`, no `-a`/`-m`) on a tag-push-triggered workflow, `github.sha` happens to equal the tag's target commit. But that equivalence is an implicit property of the trigger type, not a documented contract, and relying on it silently would make the guard's correctness depend on tag-annotation style never changing. `release.yml`'s "Verify tag is on main" step already computes `TAG_COMMIT` explicitly via `git rev-parse "${{ github.ref_name }}^{commit}"` to do its own ancestor check; exporting that already-derived value via `$GITHUB_ENV` and reading it in the goreleaser hook via `{{ .Env.TAG_COMMIT }}` means the expected revision is computed exactly once and passed identically into all four per-target invocations, with no risk of drift between them.

### Why `debug/buildinfo.ReadFile` rather than shelling out to `go version -m`?

Both approaches read the same embedded build-info blob. `debug/buildinfo.ReadFile` is the stdlib API `go version -m` itself is built on, avoids a subprocess and output-parsing step, and keeps the guard as ordinary testable Go code with typed access to `.Settings` — mirroring the pattern `cmd/version.go` already uses for `runtime/debug.ReadBuildInfo` on the *running* process. Confirmed empirically during Research that `-ldflags "-s -w"` (already used in `.goreleaser.yaml`) does not strip this build-info blob.

### Why not fold this into `cut-release.sh`?

`cut-release.sh` runs on a developer's machine and only tags and pushes — it does not build or upload the artifacts that ship. Only `.github/workflows/release.yml` (via goreleaser) produces the binaries a user actually downloads. A guard that ran solely in `cut-release.sh` would verify nothing about the artifact that matters; it must run in the same process that builds what's published.

## Consequences

**Positive:**
- A dirty or wrong-revision binary now aborts the entire `goreleaser release` invocation before any archive, checksum, GitHub Release, or Discussions announcement is created — confirmed empirically (see Testing below), not just asserted from goreleaser's documented hook semantics.
- The check runs against every matrix target independently; any one of the four (darwin/linux × arm64/amd64) being dirty or at the wrong commit fails the whole release, not just that target.
- Failure output names the offending binary's path, its actual `vcs.revision`/`vcs.modified`, and the expected revision — sufficient for a maintainer to diagnose without re-deriving anything.
- Zero new external dependencies (`debug/buildinfo` is stdlib).
- A clean release at the correct commit is unaffected: no new manual steps, no behavior change to a passing build.

**Negative / Trade-offs:**
- This guard only covers the CI-built path. It says nothing about `cut-release.sh`'s own local `go build ./...` sanity check, which remains a separate, earlier gate with different purpose (fail fast locally, not verify what ships). A maintainer seeing a `verify-release-artifact` hook failure in the Action logs must not confuse it with a `cut-release.sh` failure — documented in `docs/USER_GUIDE.md` and `.claude/skills/cut-release/SKILL.md`.
- The guard depends on goreleaser's per-build hook failure semantics continuing to abort the whole release on non-zero exit. This was smoke-tested locally against the installed goreleaser version (see Testing) but not against whatever version `goreleaser-action@v6`'s `version: latest` resolves to at actual release time — version drift is a standing, low-probability risk noted in Research and accepted rather than pinned, consistent with the existing workflow's use of `version: latest`.
- This guard cannot fix `v0.0.75` — that artifact is out of scope (see issue Problem: `proxy.golang.org` has already cached its checksum; republishing different content under the same tag is unsafe). The next real release is this guard's first live production test.

## Testing

Verified locally end-to-end in a **plain clone** of the repo (not a linked git worktree — see note below) via `goreleaser build --snapshot --clean`, covering all three required cases:

- **Clean checkout at the correct commit**: all four targets built, all four hook invocations printed `clean at <sha>`, build succeeded.
- **Dirty tree** (uncommitted changes present at build time): all four hook invocations failed with `built from a dirty working tree (vcs.modified=true, ...)`, and the build aborted (`⨯ build failed`) before any archiving step ran.
- **Wrong revision** (`TAG_COMMIT` set to a value other than the actual built commit): all four hook invocations failed with `built from the wrong commit (vcs.revision=..., expected=...)`, and the build aborted the same way.

Additionally confirmed that `mkdir -p .release-tools` writing into a directory *other than* `dist/` is required — an earlier iteration that wrote the verifier into `dist/verify-release-artifact` failed goreleaser's own "ensure distribution directory is empty" check, since the `before.hooks` step runs before that check but after `--clean`'s own wipe.

**Note on the plain-clone requirement**: initial smoke-testing was done directly in this issue's own Fabrik-managed git worktree (`.fabrik/worktrees/.../issue-1070/`) and produced a confusing, consistently-wrong `vcs.revision` from `go build` that did not match `git rev-parse HEAD` run directly in the same directory. This was tracked down to a `go1.26.2`-specific VCS-stamping quirk with linked git worktrees (`git worktree add`), not a bug in the verifier or the goreleaser wiring — a plain, non-worktree clone (`git clone` + `git checkout <branch>`, exactly what `actions/checkout@v4` does in CI) stamps the correct revision. This is noted here because it is exactly the kind of environment-specific gotcha that would otherwise cause someone to distrust a correct guard based on a misleading local repro; it does not affect the CI path, which never uses a linked worktree.

Unit tests (`tools/verify-release-artifact/main_test.go`) build real binaries in temp git repos (via the real `git` binary, guarded by the project's standard `skipIfNoGit` convention) and cover the same three cases directly against `verify()`, independent of goreleaser.

## Related Work

- #1073 — `.gitignore`'d `/dist/` to prevent goreleaser's own build output from making the CI-built tree look dirty. This guard is the safety net in case that fix is incomplete or a different cause of dirt recurs; it does not supersede or depend on #1073.
- #1069/#1074/#1076 — a separate incident cluster from the same `v0.0.75` release, covering `--auto-upgrade` re-exec and `SemverGreater`'s inability to compare suffixed versions (e.g. `+dirty`). Not modified by this issue; cross-referenced here because the optional `+dirty` `--version` surfacing added alongside this guard (`cmd/version.go`) produces exactly the kind of suffixed version string that cluster's `SemverGreater` gap concerns — a pre-existing, out-of-scope limitation, not a regression introduced here.

**References:** [docs/USER_GUIDE.md §Built-in Skill: `/cut-release`](../docs/USER_GUIDE.md), [.claude/skills/cut-release/SKILL.md](../.claude/skills/cut-release/SKILL.md)

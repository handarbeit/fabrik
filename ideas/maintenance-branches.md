# Maintenance branches / hotfix releases

**Status:** idea, not needed yet. Investigated 2026-08-02, after v0.0.76 shipped and work began queuing for the next release.

**Motivating worry:** "a fast-moving bug on 0.0.76 that can't wait for the next release's e2e fix cycle."

Nothing here is scheduled. This records the findings so the analysis doesn't have to be redone under pressure, when a live bug is the reason we're asking.

> **Update 2026-08-04:** v0.0.77 shipped on the existing `0.0.X` scheme, so the
> Recommendation's "cut `v0.1.0` rather than `v0.0.77`" was overtaken by events.
> Findings 1–3 are unaffected — the version-scheme change is still a prerequisite
> for any maintenance line, it simply now applies to whichever release comes next.
> Nothing else here has been re-verified since 2026-08-02.

---

## TL;DR

The instinct is "cut a `release/0.0.76.x` branch." Three things block that, and the third is the one that actually matters:

1. **No valid hotfix number exists** under the current `0.0.X` scheme. `v0.0.76.1` is not a legal Go module version.
2. **Two hardcoded `main` gates** in the release tooling would reject the tag.
3. **Auto-upgrade has no concept of a release channel.** `GET /releases/latest` is a single global pointer — two active release lines cannot both be "latest", so one population is always mis-served.

The stated fear is *gate latency*, not branch topology. Fix that directly and the branch is unnecessary. See [Recommendation](#recommendation).

---

## Finding 1 — there is no valid hotfix number today

Verified with `golang.org/x/mod/semver`:

| tag | `semver.IsValid` | vs `v0.0.77` |
| --- | --- | --- |
| `v0.0.76` | true | `-1` |
| `v0.0.76.1` | **false** | — |
| `v0.1.0` | true | `+1` |
| `v0.1.1` | true | `+1` |
| `v0.0.77-hotfix.1` | true | `-1` |

`0.0.76` already spends the PATCH component on the release counter, so there is no component left underneath it. Go modules require exactly `vMAJOR.MINOR.PATCH`; a fourth numeric segment is rejected outright, which breaks `go install` — the documented install path.

`v0.0.77-hotfix.1` is legal but sorts *below* `v0.0.77`. It reads as "an early cut of 0.0.77", not "a patch on 0.0.76", and it consumes 0.0.77's prerelease space. Wrong tool.

**Therefore a maintenance line requires moving to `0.MINOR.PATCH`:** next feature release becomes `v0.1.0` (not `v0.0.77`), hotfixes are `v0.1.1`, `v0.1.2`, and the following feature release is `v0.2.0`.

### The scheme change is safe for existing daemons

Tested against Fabrik's own `SemverGreater` (`internal/selfupgrade/semver.go`) — all pass:

| latest | running | expected | why it matters |
| --- | --- | --- | --- |
| `v0.1.0` | `0.0.76` | upgrade | existing daemons must cross the scheme change |
| `v0.1.1` | `0.1.0` | upgrade | hotfix reaches the maintenance line |
| `v0.1.0` | `0.1.1` | **no** | a feature release must not downgrade a hotfixed daemon |
| `v0.2.0` | `0.1.1` | upgrade | next feature release supersedes the hotfix |
| `v0.1.0` | `0.0.76+dirty` | upgrade | suffixed/source builds still cross it (#1074) |

Comparison is on the numeric core with zero-padding, so `0.1.0` beats `0.0.76` on the MINOR segment. No code change needed for the scheme switch itself.

---

## Finding 2 — two hardcoded `main` gates

- `scripts/cut-release.sh:80` — `[ "$BRANCH" = "main" ] || die "must be on main"`. Also fast-forwards and pushes to `main` explicitly at :206 and :284.
- `.github/workflows/release.yml:22-27` — **"Verify tag is on main"**: `git merge-base --is-ancestor "$TAG_COMMIT" origin/main`, aborting the publish otherwise.

The workflow gate is the harder one: even a hand-pushed tag from a maintenance branch would be refused at publish time.

Both would need to accept `main` *or* `release/*`, keeping the intent (the tag must sit on a known release lineage, not an arbitrary commit). Neither guard should simply be deleted — they exist to stop a release being cut from a random branch.

---

## Finding 3 — auto-upgrade has no channel concept (the real blocker)

`PerformReleaseUpgrade` (`internal/selfupgrade/release.go:54`) calls `FetchLatestRelease`, which is `GET /repos/{owner}/{repo}/releases/latest` (`github/client.go:193`). That endpoint returns **one** release: whichever GitHub currently flags as latest.

Two active release lines cannot both hold that pointer. Both dispositions are wrong:

- **Maintenance release flagged latest** (GitHub's default on publish). Publish `v0.1.2` after `v0.2.0` and `/releases/latest` returns `v0.1.2`. Every 0.2.x daemon polls, computes `SemverGreater("v0.1.2", "0.2.0") == false`, and **silently stalls** — no upgrade, no warning — until something newer is flagged latest. Not a downgrade (the comparison guard holds), but a stall.
- **Maintenance release published with `make_latest=false`.** `/releases/latest` keeps returning `v0.2.0`, so 0.1.x daemons **never receive the hotfix automatically** — the entire point of cutting it. They'd be offered the feature line instead, which is precisely what someone pinned to a maintenance line is avoiding.
- **Marked prerelease** is worse: `/releases/latest` skips prereleases entirely.

There's a documentation footgun on the same pointer: the `Upgrading` block in release notes uses an untagged `gh release download --repo handarbeit/fabrik --pattern "..."`, which resolves to whatever holds the latest flag.

**Consequence:** a maintenance branch is not just branch plumbing. It needs a release-channel notion in the upgrader — e.g. config `release_channel: stable | maintenance`, with the maintenance channel querying `GET /releases` (list) and selecting the highest tag matching the running MINOR, instead of trusting the global pointer. That is real feature work with its own tests and failure modes, not a workflow tweak.

---

## Recommendation

**Do not build maintenance branches until a bug actually forces it.**

The stated fear is that a 0.0.76 bug must wait for the next release's e2e cycle. That is a *gate-latency* problem, and branch topology is an expensive way to route around it.

### Preferred: keep `main` always-releasable, with a scoped hotfix gate

Define a cheaper gate for a hotfix than the full two-mode release gate:

- the `-race` unit suite (~2000 tests in the engine package, about a minute), plus
- only the e2e slice covering the touched area — **not** both merge-train legs.

That ships a fix from `main` in well under an hour, with no new versioning scheme, no workflow surgery, and no channel feature. It works whenever `main` is releasable at the moment the bug lands.

### The branch only earns its cost when `main` is not releasable

i.e. half-finished work for the next release is sitting in `main` and cannot ship. If that becomes common, revisit — and then the full cost is Findings 1 + 2 + 3, in that order, with Finding 3 being the majority of the work.

### Cheap prep worth doing anyway

**Switch to `0.MINOR.PATCH` at the next release** — cut `v0.1.0` rather than `v0.0.77`.

This is the only piece that is painful to retrofit. It costs one decision at cut time, is verified safe for existing daemons (Finding 1), and it keeps the option open. Everything else — the branch, the workflow guards, the channel support — can wait until it is genuinely needed.

### If it is ever built, don't forget

- **Forward-port discipline.** The failure mode is a fix shipping in `v0.1.1` and regressing in `v0.2.0`. Prefer landing on `main` first and backporting to the maintenance branch, rather than fixing on the branch and hoping the forward-port happens.
- **Fabrik can already drive the fix itself.** `base:<branch>` is live and complete — fork, rebase, PR targeting, and close-on-merge (ADR-1096 / ADR-1097, shipped in v0.0.76). A hotfix issue labelled `base:release/0.1.x` flows through the pipeline normally. This is the one part that needs no new work.

---

## References

- `internal/selfupgrade/semver.go` — `SemverGreater`, numeric-core comparison (#1074)
- `internal/selfupgrade/release.go:54` — `PerformReleaseUpgrade` → `FetchLatestRelease`
- `github/client.go:193` — `GET /releases/latest`
- `scripts/cut-release.sh:80` — on-main preflight
- `.github/workflows/release.yml:22-27` — tag-on-main publish gate
- ADR-1096 / ADR-1097 — `base:<branch>` and close-on-merge for non-default bases

# ADR 2228: CI's `-race` Sim Time — Per-OS Parallelism Ceiling (Linux 16), Sim Sharding Rejected

**Date**: 2026-10-14
**Status**: Accepted
**Issue**: #2228 — tests/sim: cut CI's -race sim time — measure Linux at higher parallelism, shard in CI only if it pays
**Amends**: [ADR 1677](1677-release-gate-hardening.md) (the `-race` parallelism cap), [ADR 1857](1857-no-per-pr-test-selection.md) (its revisit trigger)

## Context

ADR 1857 rejected per-PR test selection and recorded a revisit trigger, "`tests/sim` grows toward
its timeout". It fired on 2026-10-09. The issue quoted `tests/sim` at ~629 s on CI's 4-vCPU runner.
Recent CI (run 38562353113, 2026-10-14, after #2210's parallelisation) already showed ~363–371 s
for the package, so the figure was re-baselined before comparing anything.

`default_race_parallel` (ADR 1677, `scripts/lib/parallel.sh`) capped `-race` at `min(4, cores)`
because 8 crashed ~1 run in 5 on a 28-core **macOS** host: a wedged `fork/exec`, `git: signal:
segmentation fault`, `CHECK failed: tsan_rtl.cpp`. It had never been measured on Linux, where CI
runs. v0.0.84 (#2248) already moved the local pre-gate off `-race`; this ADR concerns CI only.

Two levers were measured: raise parallelism on Linux (R1), and shard the sim across CI jobs (R2).

## Measurements

All numbers are from one GitHub Actions run, **38603145931** (2026-10-14, a temporary
`sim-shard-experiment` workflow on the issue branch): `ubuntu-latest`, **x86-64, 4 vCPU** — the same
shape as CI, not an emulating VM. Every job ran on its own runner, concurrently. All 26 jobs passed.

### R1 — Linux `go test -race -count=1 -parallel N ./tests/sim/...`, five runs per level

| `-parallel` | wall per run (s) | mean (s) | crashes (segv / tsan / fork-exec) | failures |
|---|---|---|---|---|
| 4  | 387, 398, 402, 418, 420 | **405** | 0 / 0 / 0 | 0 / 5 |
| 8  | 247, 252, 252, 252, 243 | **249** | 0 / 0 / 0 | 0 / 5 |
| 16 | 162, 182, 186, 189, 191 | **182** | 0 / 0 / 0 | 0 / 5 |

(Seconds are the in-script `go test` wall time, `R1RESULT` lines; whole-job time adds ~20 s of
checkout, Go setup and module download.)

### R2 — single job vs K sim shards (`-parallel 4`, same run)

The single job is `go test -race -parallel 4 ./...` (541 s job time, 10 billed minutes). A K-shard
layout is K jobs of `go test -race -run '^(…)$' ./tests/sim` plus one job for
`go test -race $(go list ./... | grep -v '/tests/sim$')`, which took 320 s (6 billed minutes) and is
the floor of every layout: `engine` is serial and ~220–240 s on its own.

| layout | slowest job (wall) | change vs single job | billed runner-minutes (rounded up per job) | vs single job |
|---|---|---|---|---|
| single job | 541 s | — | 10 | 1.0× |
| K=2 | 320 s (rest; slowest sim shard 283 s) | −41% | 16 | 1.6× |
| K=3 | 320 s (rest; slowest sim shard 225 s) | −41% | 18 | 1.8× |
| K=4 | 320 s (rest; slowest sim shard 215 s) | −41% | 20 | 2.0× |

Shards were hash-partitioned by test name, so they are uneven: at K=4 the sim shards ran 145–215 s.
`worker-shaped` runs the whole module too, so a sharded layout would double all of these.

## Decision

**R1 — adopted.** Linux was clean at every level, 15 runs of 15. `default_race_parallel` is now
per-OS: **16 on Linux, 4 elsewhere** (still `cores` when the host has fewer than 4 cores). The
macOS-specific cap stays at 4. On the CI runner the sim drops from ~405 s to ~182 s — below the
320 s the rest of the module needs — and the change applies to the `go test` step and the
`worker-shaped` job without a workflow edit, because both source the helper.

**R2 — rejected.** The criteria were written before measuring: adopt only if the slowest job is at
least 25% faster **and** total runner-minutes stay within 1.5× of the single job.

- K=2 meets the wall-time bar (−41%) but not the minutes bar (1.6×). It misses by more once
  `worker-shaped` is sharded too (the whole layout then costs about 32 minutes against ~16.5).
- K=3 and K=4 get the same wall time as K=2 — `engine` sets the floor — at 1.8× and 2.0× the cost.
- After R1, the sim is no longer the long pole (~182 s against `engine`'s ~320 s job), so sharding
  it would buy almost nothing at any K.

`.github/workflows/ci.yml` is not changed, and the partition helper and its completeness check,
built for the experiment, were not kept.

## Caveats

- **Five runs per level do not prove "no crash".** Five clean runs bound the crash rate at roughly
  45% or less (95% confidence). ADR 1677's macOS rate was ~1 in 5, which five runs would miss about
  a third of the time. The standing validation is CI: every PR now runs `-race` at 16 on Linux. If a
  fork/exec or TSan crash ever appears there, lower the Linux ceiling in `scripts/lib/parallel.sh`
  (one line).
- **The post-R1 single-job time was not measured.** The experiment compared layouts at `-parallel 4`.
  The R2 verdict does not depend on it: at 16 the single job can only get faster, and the minutes
  bar already fails. The first real PRs after this change will show the actual number.
- **One runner type, one date.** Other runner images and a growing sim will move these numbers.
  Quote the run ID when revisiting.
- `scripts/sim/measure-race-parallel.sh` is kept: it repeats the R1 measurement on any Linux host.

## Revisit if

- A Linux `-race` crash (segfault, `tsan_rtl`, wedged fork/exec) appears in CI at the new ceiling.
- `engine` (or another package) is sped up so that the sim is the long pole again, with the
  single-job wall time well above ~5 minutes.
- Runner-minutes stop being a constraint, which would make K=2 attractive.

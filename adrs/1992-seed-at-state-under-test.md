# ADR 1992: Seed live e2e scenarios at the state under test instead of driving the pipeline to reach it

## Status

Accepted (#1992). Builds on ADR-1933 (the per-test registry the new fields extend), ADR-1974 (the `awaitVisible` family every seed goes through), ADR-1975 (the sparse matrix removed redundancy *across* cells; this removes it *within* the suite) and the zero-cost seeding precedent of `seedLandingCandidate` / `seedReviewGateItem`. **ADR-1454 is unchanged:** every live scenario still runs live before every release. Nothing moves to the sim. What changes is how a live test gets to its subject.

## Context

About twenty live tests entered the pipeline at Specify and drove Specify → Research → Plan → Implement → Review → Validate with real model calls, real CI and real review bots. For several of them the traversal was set-up, not subject: a Validate-time gate, a landing decision, a post-merge heal. Each such traversal costs tens of minutes and its own GraphQL and quota spend, and each is another chance to hit a harness race or a model-judgement variance unrelated to the subject.

The harness already had the better pattern for 12 tests (7 at Validate, 5 at Queued).

## Decision

### 1. Two required registry fields, no default

`registry.Entry` gains `entry` (where the engine first sees the item: `Specify … Validate`, `Queued`, or `none` for a test that files no item) and `traversal` (`subject` | `none`). There is deliberately **no `setup` value**: a test whose traversal is merely the way to its subject must seed instead, so the vocabulary cannot express the thing we want to remove. `traversal_reason` (one line) is required for `traversal: subject` and for any `entry: Specify`, so the tests that still start at the beginning each say why.

A one-way scan cross-check (`ScanSpecifyDrivers`, following `ScanIdentityAssertCallers`' same-directory reference closure) fails a test that declares a late `entry` but still reaches `SetIssueStatus(..., "Specify")`. It is one-way on purpose: it catches a stale or dishonest claim cheaply, and cannot tell a primer filing from a subject entry, which is why `TestMergeTrainColdCacheBaseMember` (a cheap blocked primer that warms the base-branch cache) is the one documented exemption. Entry is otherwise hand-authored, like `auth`/`train`.

### 2. One generalised seed, as a pure description plus an executor

`tests/e2e/seedspec` (untagged) holds `Spec` → `Build`, a pure function returning the exact labels, Status, PR shape and optional prior-stage comments. `seedAtStage` (`tests/e2e/seed.go`, tagged) only executes the plan, waiting solely through the `Await*Visible` family. The split is what lets the fidelity check run in plain `go test ./...`.

`seedLandingCandidate` and `seedReviewGateItem*` stay as thin wrappers with a `Minimal` flag that preserves their original labelling, so the engine inputs of the 12 already-passing tests do not change. New seeds label the whole chain (`stage:Specify … <entry>:complete`), as a real traversal does.

Two seed shapes, because the subjects differ:
- **Completion** (`Column: X`): `stage:X:complete`, Status X — the state an item parks in. For subjects that begin at the landing decision (`TestYoloAutoMergeLabel`, `TestConvergenceRace`).
- **Arrival** (`Column: X, RunColumn: true`): stages before X complete, the engine runs X once — for subjects that need that stage's real output. This is what the CI-fix and conjunctive-gate tests need: a *complete* Validate cannot exist while CI is red (`stage:Validate:complete` is deferred until the CI gate clears), so seeding it would be an impossible state. Seeding the arrival at Validate keeps one real Validate invocation, which is what sets `fabrik:awaiting-ci`.

### 3. Fidelity: may omit, never invent — against fixtures recorded from the real engine

`seedspec.CheckFidelity` compares a plan with a recorded real-traversal fixture (`seedspec/testdata/<column>.json`): a seed may leave out labels a traversal carries, but every label it adds must be one the traversal carries, and the column and the PR's existence, base, linkage and draft state must match. A draft PR is the single declared deviation (it keeps the real review bot away, #1312). An arrival seed is compared with the previous stage's parked state.

The fixtures are recorded from the **real `Engine`**, driven through each stage by the sim bed's scripted invoker (`tests/sim`'s `TestSeedFixturesMatchEngineTraversal`, which also fails when the engine's state shape drifts from them; `-update-seed-fixtures` recaptures). Rejected alternative: fixtures captured from a live run. Implement cannot run the bed, a live capture could not be re-derived on every PR, and the sim exercises the same label/Status/PR code production does. The cost is the sim's own blind spot — it cannot see GitHub wire correctness (ADR-1454) — which this check does not need: it compares shapes the engine decides, not how GitHub renders them. An optional live cross-check is documented in the README.

### 4. The full-traversal set (R3)

`full_traversal: true` marks the named set that drives each pipeline path end to end once, and the completeness test fails if it is empty: `TestSmokeSingleRepoFullPipeline` (the ordinary yolo path to Done, including the Review → Validate hand-off the seeded Validate tests no longer exercise), `TestCruiseFullPipeline` (the cruise chain), `TestBaseBranchPipeline` (an engine-authored PR on a non-default base — a seed cannot make one) and `TestCrossRepoSpawn` (a Plan-declared spawn needs a real Plan). It is valid only with `entry: Specify` + `traversal: subject`.

### 5. Measurement

`gate report [--sha S] [--baseline B]` totals measured per-test `Elapsed` from the archive and joins the registry fields. **Model-quota use is not recorded** by the ledger or the archive (the engine log has no token counts; the footer is on the GitHub stage comments), so the report carries a labelled proxy — pipeline stages not driven — rather than an invented figure.

## Consequences

- **Coverage that changed, recorded.** The CI-fix reinvoke path loses its only full-traversal driver, and `TestCIFixReinvoke`'s Implement-time precondition (the Implement agent must not pre-empt the sentinel) is gone; the nonce precondition still guards the Validate agent and the sentinel is run-ID based (ADR-1991), so the seeded PR is red by construction. Restoring live coverage for the traversal itself is #1991's. `TestConjunctiveCIReviewGate` no longer proves Review's own gate clears before Validate; smoke covers the hand-off.
- **Review is out of the way for some seeds.** Harness PRs carry `pruefer:ignore`, so no real bot reviews a seeded PR. The yolo/CI-fix/convergence seeds add `expected-reviewers:none` (ADR-1283) — review is not their subject; the conjunctive test drives its own reviewer as before.
- **Convergence race:** both PRs add the same *new* file with different content (an add/add conflict) because the Contents API cannot edit an existing blob without its SHA; they are exposed to the engine together once both slow-gates are green.
- **Ledger invalidation.** Editing a live test or a same-package function it references changes its source hash (ADR-1972): every converted test, and every caller of a changed helper, re-runs for the next release. The wrappers keep their signatures to limit that.
- **Fixture staleness** is controlled by the sim test failing on drift, with the recapture command in the README.
- **Non-vacuity of the conversions** is recorded per test in its header comment (the engine behaviour whose neutralisation flips it). The live neutralisation runs need a bed and are operator steps, not claims; the seed-fidelity mutation self-tests run on every PR.

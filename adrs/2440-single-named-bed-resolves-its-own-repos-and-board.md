# ADR 2440: A single bed named by `E2E_BEDS` resolves its own repos and board

## Status

Accepted (#2440). Amends ADR 1976 decisions D1 (per-bed resolution) and D2 (`bedEnv`), which it extends to a one-element `E2E_BEDS`. Builds on ADR 1974 (the live preflights) and ADR 2426 (the webhook delivery preflight). ADR 1454 is unchanged.

## Context

A multi-bed run resolves each bed's repo pair and board (bed `.env`, then environment, then defaults) and injects them into its legs; a single-bed run built no per-bed view, so `Gate.bed` stayed nil. `bedEnv` then returned only `FABRIK_TEST_DIR` for an `E2E_BEDS` bed, and `resetConfig` — which feeds reset, the board-lag probe and the delivery preflight's scratch repo — read only the environment and defaults. Running `E2E_BEDS=<bed 2>` therefore filed the live fixtures on bed 1's repos and board; bed 2 never saw them and the test ended INCONCLUSIVE three times. The delivery preflight labelled its scratch issue on the default repo and passed only because the App is installed org-wide.

## Decisions

1. **`envBedSpec` resolves the single `E2E_BEDS` bed through `resolveBedSpec`.** Precedence is therefore literally the multi-bed rule, in one implementation, so a bed resolves identically alone or in a list.
2. **`bedEnv` and `resetConfig` both use it.** `bedEnvFor(spec)` is shared with the multi-bed branch. Fixing `resetConfig` fixes reset, the board-lag probe and the delivery probe together.
3. **`Gate.bed` is not set** for the single bed. It also drives the `[bed X]` output prefix, leg-log and archive names, the identity lookup and the multi-bed scheduler branches, none of which should change.
4. **The criterion is the `E2E_BEDS` environment variable, trimmed.** A whitespace-only value is unset, as `parseBeds` reads it.
5. **A bed named by `FABRIK_TEST_DIR` alone is unchanged**: `bedEnv` stays nil and `resetConfig` stays environment-then-default. The same latent gap exists there (a `.env` naming other repos is ignored); it is deliberately left open and is a candidate follow-up.

## Consequences

- An operator who overrode the board through the shell environment on an `E2E_BEDS` run now sees the bed's `.env` win, as in a multi-bed run.
- The delivery preflight now targets the bed's own alpha repo, so a bed whose token lacks `issues:write` there fails at `probe-setup` rather than passing silently. That surfaces a provisioning gap rather than creating one.
- Tests: `TestSingleEnvBedLegCarriesItsOwnReposAndBoard`, `TestSingleEnvBedResolvesLikeAMultiBedElement`, `TestBedNamedByTestDirAloneKeepsTheEnvironment` and `TestProbeWebhookDeliveryTargetsASingleEnvBedsOwnRepo`; reverting either `bedEnv` or `resetConfig` fails at least one.

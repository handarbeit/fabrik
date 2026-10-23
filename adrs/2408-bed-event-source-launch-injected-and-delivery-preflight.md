# ADR 2408: The bed event source is a launch-injected leg input, with a per-leg delivery preflight

## Status

Accepted. Builds on ADR 2143 (embedded Tailscale receiver), ADR 2417 (bed timeouts injected at launch),
ADR 1974 (environment probes) and ADR 1454 (live probes run after the pre-gate). The matrix axis and
ledger keys for the event source are #2427; this ADR records the interim arrangement.

## Context

Neither gate bed received webhooks: both ran poll-only, so no live test exercised event-driven ingestion
and every webhook-dependent assertion self-skipped (`TestWebhookChangeSurvivesReconcile` had never run in
a gate). The original premise — a PAT leg with gh-forward delivery — is obsolete: the direct and Tailscale
transports are App-auth transports, and the Tailscale one is the bed transport.

## Decisions

1. **The event source is a leg-scoped input injected at launch**, like the review/CI timeouts (ADR 2417).
   `E2E_EVENT_SOURCE` (`poll` default, `tailscale`) is read once by the gate and reaches the restarted bed's
   harness through the leg environment. A new untagged package, `tests/e2e/bedevents`, is the single
   definition (parse, effective source, injected environment, ambient variables to strip, cadence) shared by
   the gate's `BedStartCmd`, the harness's `StartFabrikTestBed` and the tests. Both launch sites strip the
   ambient `FABRIK_EVENT_SOURCE` and receiver variables and then inject an explicit value; the process
   environment beats the bed's `.env`, so a poll leg is genuinely poll-only and a shell or `.env` value
   cannot flip a leg. The tagged half is pinned by a source-reading test, as for `bedtimeout`.
2. **PAT is always poll.** `Effective(requested, auth)` is `tailscale` only for an `app` leg; the engine
   refuses the transport under a PAT, so forcing poll avoids a startup crash.
3. **The initial gate start is poll.** Every leg restarts the bed with its own source, so the initial start
   only needs the safe state.
4. **A webhook leg polls every 180 s** (poll legs keep 60 s): longer than the 90 s the webhook test waits and
   six times the production default, so a quick effect is evidence of the webhook path. Overridable with
   `E2E_BED_POLL_SECONDS`. The selection applies to every leg of the run, so a webhook run is a dedicated,
   test-selected run; scenarios whose fixed waits assume 60 s are not audited here.
5. **A per-leg delivery preflight** runs after the leg's bed restart and identity check, before the first
   phase, for effective-webhook cells only. It is a live probe (a scratch-issue label write), so it follows
   ADR 1454 by running after the pre-gate, but it cannot be a `LivePreflights` entry because it needs the
   running bed with its transport. Steps: `probe-setup` (the probe cannot make its own write), `transport-not-up`, `hook-url-mismatch` (App config vs the URL the
   daemon logged, exact), `no-delivery` (the daemon's last-delivery time must move past a baseline taken
   before the write, confirmed by the App's delivery log when readable so a replayed older delivery cannot
   pass). It retries within `E2E_DELIVERY_PROBE_WAIT` so a fresh Funnel name can settle.
6. **An unreadable reading blocks** this probe (exit 4 naming the bed and step) — a deliberate carve-out from
   ADR 1974's "unavailable never blocks", because the probe exists to stop a leg that would otherwise silently
   skip or go inconclusive. Results are recorded per leg and bed in `probes.json`; no secret is read into any
   message or record.
7. **A static provisioning check** (`bed-webhook-provisioning`, exit 7, beside `bed-topology`) verifies per
   bed a distinct hostname, the webhook secret variable and an auth key or stored login — presence only — so a
   mis-provisioned bed fails with a named message rather than from the engine's startup refusal.
8. **Interim coverage arrangement.** `TestWebhookChangeSurvivesReconcile` registers `skip_ok_legs: ["pat/*"]`
   (structurally accurate: a PAT leg can never run the transport) and its poll-leg skip cites #2427, so it stays
   a known skip rather than a missing one on a default poll run until the matrix axis lands. The cold-cache
   train test keeps its skip on a webhook-source bed (the cold window is genuinely absent) and cites #2427 too.

## Consequences

- One definition of the event source, so a bed and its tests cannot disagree.
- A broken Funnel, a wrong App URL or an unsubscribed App is a named exit-4 before any test runs.
- Live prerequisites (Funnel enabled for the tag, the App's event subscriptions) are verifiable only live; the
  preflight names the cause. Nothing in CI exercises a real tailnet node.
- Until #2427, only a run that selects `E2E_EVENT_SOURCE=tailscale` exercises the webhook path, and one run
  does not prove the poll leg (or vice versa).

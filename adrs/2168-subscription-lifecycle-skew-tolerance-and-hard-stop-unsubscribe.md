# ADR 2168: Subscription lifecycle: skew tolerance and hard-stop unsubscribe

## Status

Accepted. Amends ADR-1966-b: its #2129 note "No alias is accepted by `subscribe`" no longer holds (decision 1), and its at-most-once delivery contract gains a precise unsubscribe boundary (decision 3).

## Context

Two defects were observed in a live MCP session on 2026-10-11.

1. **Skew empties the subscription.** After #2129 renamed `claude-limit-suspended` / `claude-limit-lifted`, a `fabrik mcp` shim built before the rename still advertised the old names. The daemon's `Subscription.Validate` rejected the *whole* request (`bad_request: unknown event type "claude-limit-suspended"`) and the session was left with no subscription. Skew is the normal case, not a corner: the daemon self-upgrades (`--auto-upgrade`, SIGHUP re-exec) while the shim is a long-lived child of a Claude Code session and keeps the schema and validation it was compiled with.
2. **Unsubscribe did not stop a flood.** During a restart flood (#2123), `fabrik_unsubscribe` returned `removed: 1` and about 250 more `label-applied` pushes still arrived.

## Decision

### 1. Skew is tolerated in the daemon: map what is known, drop what is not, report both

Of the three options in the issue, the daemon takes **(a) and (c) together**; **(b)** is not used.

- **(b)** (`hello` advertises the live catalogue, the shim builds its schema from it) cannot help the observed case. A shim already running has a fixed schema and validation; it never reads a catalogue. It would also need `hello` at `tools/list` time, which is static and daemon-independent today.
- **(a)** alone fixes only the known renames. **(c)** alone would drop `claude-limit-suspended` instead of honouring what the subscriber meant. Together they fix the observed case, and drop (with a report) any future type the daemon removes or has not got.

`channelevents.NormalizeEvents` (`legacy.go`, beside `legacyEventTypes`) is a pure helper: a legacy name becomes its current name (*mapped*), a subscribable current name passes through, anything else, including a synthetic type, is *rejected*. The kept list is de-duplicated in request order. `channelStreamer.Subscribe` runs it before `SubscribeNew`, so the idempotency key sees the current names and re-subscribing with an old name is the same subscription. `Hub` and `Subscription.Validate` stay strict; `TestSubscribeRejectsLegacyEventTypes` still holds.

`SubscribeResult` gains additive `omitempty` fields `mapped` (`[{from, to}]`) and `rejected` (`[]string`). The shim prints daemon results verbatim, so a stale shim shows them with no change, and a request of current names alone is byte-identical to before.

**An empty event list means every event**, so dropping unknown names must never empty the filter. A request that named events and keeps none is refused with `bad_request` naming each rejected type; nothing is stored and any existing subscription is untouched.

**Not covered:** a new shim against an old daemon. The daemon that rejects a name cannot be changed. The shim does not retry a subscribe without the rejected names; the version note below is the signal.

### 2. Skew is visible

`hello` gains `daemon_version` (`localapi.Server.Version`, set from `cfg.Version`; omitted when empty). The shim compares it with its own `Version` as opaque strings and, the first time they differ, appends one text block to a tool result: both versions and "restart the Claude session to pick up the new server". It then never repeats. `fabrik_health` already returns the daemon's `version`; the shim adds a "Versions — daemon X, MCP server Y" line to it, and reuses that version for the check instead of a second call.

Any other tool triggers a `hello` check until the note has been shown, so a daemon that upgrades *mid-session* is noticed too, which is the typical way skew arises. A check that finds no skew (equal builds, or a daemon without `daemon_version`) is trusted for five minutes (`skewRecheckInterval`) before the next call asks again, so a session whose builds match does not pay a `hello` on every call; the `hello` runs outside the shim's skew lock so concurrent tool calls do not queue behind it. A failed `hello` is retried by the next call. (Changed after review: the first version re-checked on every call.) A failed `hello`, a daemon without `daemon_version`, or a shim with no version of its own produces no note and no error. Nothing on the daemon side depends on shim-reported data.

### 3. Unsubscribe is a hard stop on the daemon's side

Reading the code found four distinct leaks; the ~250 pushes were not reproduced (see Consequences).

1. `drain` took a batch, then called `Deliver` with no lock `Unsubscribe` respects, and the batch survived the queue clear.
2. `Publish` and `CatchUp` resolve their targets under `h.mu` and enqueue after releasing it, so an enqueue that matched before an unsubscribe could land after the queue was cleared.
3. The queue was cleared only when the *last* subscription went. Removing one of several left entries only that subscription matched, and delivery never re-matched them.
4. Delivery to an attachment with zero subscriptions was not prevented by anything but the (racy) clear.

The fix, all in `internal/channelevents`:

- **Epoch.** `subscriber.epoch` is bumped under `s.mu` by every `Unsubscribe` that removed something. `drain` records it with each batch and rechecks it, together with the attachment, immediately before each `Deliver`; a stale batch is abandoned without being removed from the queue.
- **Purge.** `queueFile.retainMatching` discards entries no remaining subscription matches (and, with none left, the drop counters). `Unsubscribe` runs it for every removal, and `drain` runs it with a fresh subscription snapshot at the top of every cycle, which closes leak 2.
- **Fence.** After purging, `Unsubscribe` takes and releases `sendMu`, which `drain` holds across a whole cycle. Every `Deliver` in flight has therefore finished before `Unsubscribe` returns, and every later cycle starts from the purged queue and the current subscriptions. The contract is: **no `Deliver` for a removed subscription completes after `Unsubscribe` returns.** The cost is that `Unsubscribe` can wait for an in-flight write, bounded by the session's 5 s write deadline.
- **Attachment.** It stays bound after the last removal. Disconnecting would only make the shim's `RunStream` reconnect and re-attach. With nothing queued and nothing subscribed, delivery has stopped, and a later `subscribe` resumes it on the same attachment.

**Lock order:** `drain` takes `h.mu` (the snapshot) while holding `sendMu` but never while holding `s.mu`; `Unsubscribe` releases `h.mu` before taking `s.mu` or `sendMu`; `Prune` keeps `h.mu` → `s.mu`. Nothing takes `sendMu` under `h.mu`.

### 4. Not done: a shim-side fence

"Delivered" still means "accepted by the socket", as in ADR-1966-b. Bytes already written when `Unsubscribe` returns sit in the kernel socket buffer, the shim's stdout pipe (about 64 KB, roughly 200 notifications) and Claude Code's own queue. The daemon cannot recall them, and an in-band marker frame would arrive behind them. The likeliest source of the ~250 pushes is exactly this buffering, so the guarantee is documented as a daemon-side one and the tool description says that events already written may still surface.

## Consequences

- A stale shim keeps a working subscription across a rename, and says what was mapped or dropped. A new rename needs one `legacyEventTypes` entry to be honoured; without it the old name is merely dropped and reported.
- `Unsubscribe` is no longer wait-free: it can block up to the write deadline behind a stuck peer.
- Delivery re-matches queued entries against the *current* subscriptions, once per drain cycle, over a queue bounded by `--channel-held-max` (200 by default).
- A held entry that no subscription of its subscriber matches is now discarded when the subscriber next attaches. This can only be an entry left by a removed subscription; a test fixture that queued an unmatched entry had to be corrected.
- The ~250-push flood was not reproduced here. On unmodified code, the regression test did show `Unsubscribe` returning while a `Deliver` was still in flight, which then completed after the call returned (leak 1/fence). Whether transit buffering accounts for the rest is unresolved, which is why decision 4 is stated rather than hidden.

## References

ADR-1966-b, ADR-2125, #2129 (rename), #2123 (restart flood), #1966.

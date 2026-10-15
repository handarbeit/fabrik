# Engine state audit (2026-10-15)

**Question.** Are polling and webhooks two channels into one central item state, or have
paths drifted apart? This audit was set as the prerequisite for the tailscale/webhook work
(#2081, #2143, #2144) after the 0.0.84 drift fixes (#2159, #2160, #2178, #2180) landed.

**Answer.** Not yet. The `itemstate` Store is the central state, but it has no general
ordering rule. The one rule it has, the engine-write protection of #2159/#2178, covers
engine writes on the two listing paths, for Status, labels and closed. Every other
producer (webhook deltas, deep fetch, the poll's own write-back, the Layer 1 status
refresh) overwrites without comparing when its value was observed. Decisions then mix
three sources (the Store, a poll-start snapshot, live GitHub reads that are not written
back). A faster webhook source makes most of this worse, because the cache is ahead of
the listings more often.

**Method.** Five read-only passes over `main` at 4845422d: Store write paths, the webhook
path, decision read sources, engine-private state, freshness rules. Findings marked ✔
were re-checked against the code by hand for this document; the rest were checked by the
pass that reported them. Nothing was reproduced at runtime.

Severity: **high** = can cause a wrong engine action (advance, merge, pause, dispatch) or
a stuck item; **medium** = wrong for a bounded time, or a duplicate side effect;
**low** = cost, noise or cosmetic. "Tailscale" says how a millisecond webhook source
changes it.

## 1. Fix now: correctness bugs independent of the webhook work

| # | Finding | Sev | Tailscale |
|---|---|---|---|
| 1.1 ✔ | **A pause does not stop a landing.** `handleStageComplete` and `runCatchUpPhase2` refresh labels live, and `attemptMergeOnValidate` re-reads the item live, but none of them, nor `landValidated`/`advanceToNextStage`/`advanceToQueued`, checks `fabrik:paused`. A human pausing a yolo item during its Validate run (or before the listing shows the label) does not prevent the merge or the move to Queued. The only pause filter is the poll loop's snapshot check (poll.go:1749). | high | B unchanged |
| 1.2 ✔ | **The ordinary landing merges an unpinned head.** The CI gate judges the head from the cached `FetchLinkedPR` (≤45 s old); `landValidated` then calls `EnablePullRequestAutoMerge`/`MergePR` with no expected head. `MergePRAtHeadSHA` exists but only the train's singleton fast path uses it (merge_train.go:2337). On a repo with no required checks, a push between the gate and the merge lands unchecked. `settleSHAInvalidationScan` runs after the landing in the same poll. | high | better |
| 1.3 ✔ | **The Validate completion SHA is lost on restart.** `ValidateCompletedSHA` lives only in the in-memory Store and nothing rebuilds it; `settleSHAInvalidationScan` skips an item whose value is empty (poll_settle.go:116). After a SIGHUP or self-upgrade, a push to a Validate-complete item's PR no longer invalidates Validate, so a yolo landing proceeds on gates that do not re-run Validate. | high | none |
| 1.4 ✔ | **Each poll writes its poll-start snapshot back over current state.** `selectDeepFetchCandidates` applies `ItemDeepFetched{FreshState: board.Items[i]}` after every successful `FetchItemDetails` (poll.go:2582), cache hit included. Status always (the deep fetch never reads it), and on a cache hit labels and closed too, come from the listing taken at the start of the poll, and `applyProjectItem` writes them unprotected. A webhook move/label or a worker's own write that lands during the deep-fetch loop is reverted, a spurious `StatusChanged` fires, and the poll may dispatch on the reverted value. Same pattern at ci_settle.go:183, poll_settle.go:53, dependencies.go:200, boardcache.go:953. | high | worse |
| 1.5 ✔ | **A cache hit wipes merge-queue membership.** `copyDeepFieldsFromState`/`snapshotToProjectItem` omit `IsInMergeQueue`, `IsMergeQueueEnabled`, `MergeQueueEntry`; 1.4's write-back then sets them false. `prInMergeQueue` gates rebase and push (item.go:780, comments.go:412, merge_queue.go:56/75), so the engine can push to a PR in GitHub's merge queue and eject it. Repos using GitHub's merge queue only. | high | worse |
| 1.6 ✔ | **`projects_v2_item` is not filtered by project or field.** The payload's `project_node_id`/`field_node_id` are never read (delta.go:219). Org-level delivery covers every org project. A `created` on another board (an engine issue also on the public triage board) writes that board's item ID and an empty Status into ours; later triage edits become our Status, a triage archive removes our item, and `liveLandingState` reads the wrong board's Status and can skip a landing as "already completed". On our own board, editing any other single-select field (Priority, Size) is read as a Status change. Self-heals at the next drift reconcile. | high | prerequisite |
| 1.7 ✔ | **A comment-review spec update is masked; the next stage reads the old spec.** `comments.go:1002` updates the issue body with no write-through, then `SelfWriteObserved` moves the freshness baseline past GitHub's edit time. The cached `Body` stays old, and `context.go:98` writes the cached body to `issue.md`. The code comment ("issue body is not read from cache for dispatch decisions") is wrong for this use. Hidden when webhooks echo the edit; real on a poll-only daemon, which includes the dev daemon today. | high | hidden by webhooks |
| 1.8 | **The Layer 1 status refresh records a GitHub read as an engine write.** On every `issues`/`issue_comment` webhook, `applyLayer1StatusRefresh` writes the live Status through `LocalStatusUpdated` (reconcile.go:198,207): it stamps `EngineWriteAt["status"]`, clears `StatusAtGitHub`, attributes the change to `fabrik`, and runs unordered against `projects_v2_item.edited` and the engine's own moves. A stale read overwrites a newer move and is then shielded from the correcting listing for 30 s: #2178's bypass, reintroduced. | medium–high | worse |

## 2. The state foundation: needed before a second webhook source

These share one cause: **only engine writes carry an "observed at" that later writes must
respect.**

- **2.1 Webhook state is unprotected from older listings** (high, worse). A human move or
  label change applied by webhook in ~1 s is reverted by the next probe (every poll) or
  reconcile listing that lags GitHub. `LightReconcile` makes it worse: it reads the newer
  webhook value as drift, marks the stream unhealthy and runs `Reconcile`, which applies
  the older listing (boardcache.go:466, 1474). Status/labels flap; a dispatch can happen in
  the stale column.
- **2.2 Webhook writes have no ordering** (medium–high). `IssueLabeled`/`IssueUnlabeled`
  and `ProjectV2ItemEdited` apply unconditionally and ignore the payload time
  (store.go:447–455, 1238). A late echo of an earlier engine write re-adds a removed label
  (`fabrik:editing` gates dispatch), and `protectLabels` then shields the wrong value for
  30 s because it believes the engine wrote it. Handlers run concurrently per request
  (webhook.go:621), so order is not guaranteed even without retries.
- **2.3 Head SHA can regress, destructively** (high, worse). Eight writers set `HeadSHA`
  with no ordering. A lagging listing or a replayed `pull_request` event after a
  `synchronize` sets the old head back, which wipes check runs and the CI verdict and
  re-keys `shaToKey`; new-head `check_run` events then buffer as unknown. The #2122
  agreed-head guards mitigate; whether a gate then clears on the old head is unverified.
- **2.4 Partial payloads applied as complete** (medium). `issues.opened` and
  `projects_v2_item.created` build an item with empty Status, ItemID, comments and
  `UpdatedAt`; a late or duplicate delivery wipes an existing item. `IssueOpened` also
  unions labels, so it can never remove one.
- **2.5 `Reconcile` drops items the listing has not caught up with** (medium, worse).
  Unlike `LightReconcile` it ignores `preFetchKeys`, so an item a webhook or a spawn added
  during the fetch is removed with its counters, worker handle and engine-write records.
- **2.6 Deep fetch is still the #2159 class** (medium). `ItemDeepFetched` carries no
  `FetchedAt`; a live read issued before a concurrent worker write or spawn
  `BlockedByEdgeAdded` reverts it when applied.
- **2.7 The freshness baseline uses the daemon clock (#1945 is still real)** (high).
  `SelfWriteObserved` sets `LastSeenSourceUpdatedAt = time.Now()` (store.go:1036). A human
  issue-layer change made before an engine write reads as "fresh" indefinitely, not for
  one poll. Webhook-carried facts are rescued by the delta; facts no webhook carries are
  not: blockedBy edges, sub-issues, comment edits, and on a poll-only daemon body edits and
  non-gate labels (`model:`, `effort:`, `base:`). Evidence on the poll path also mixes
  GitHub time with webhook-receipt time (`localDeltaAt`).
- **2.8 Review lists differ by producer** (medium). Webhooks keep every review; deep fetch
  keeps the latest per author. A superseded `CHANGES_REQUESTED` kept only in the webhook
  list holds the advance gate (fails safe; the landing gate reads live).
- **2.9 Echo registration happens after the write** (low today, real with Tailscale).
  `RegisterEcho` runs after the write-through; a sub-millisecond echo arrives first, is
  attributed as external, and leaves a pending echo that can swallow a later identical
  human event. Under Hookdeck, echo matching is a no-op entirely.

## 3. Decisions that read one thing and act on another

- **3.1 Dispatch evaluates snapshots taken before the same poll's own writes** (medium–high).
  `dispatchCandidates` runs `itemNeedsWork` on copies from `selectDeepFetchCandidates`;
  Phase 1/2 and the settle scans mutate the same items in between (pause, revalidate,
  remove a completion label). Only `Worker()` is re-read. Reasoned example: Phase 1 pauses
  an item at a cycle limit, and the same poll still dispatches a comment pass on it.
- **3.2 Live reads that are not written back** (medium). `processItem`'s lock verify reads
  live labels and keeps only the lock (item.go:1090); the feedback gate's live
  `FetchItemDetails` is discarded (its gap repair covers threads and review bodies, not
  issue comments); `classifyLinkedPRTerminal`/`itemPRAlreadyLanded` read the merged state
  live and leave the Store's `LinkedPR` stale for up to 45 s. Each lets the next decision
  regress to the stale value.
- **3.3 Advance and landing review gates read different sources** (medium). `checkReviewGate`
  uses cached requests/reviews; `reviewGateBlocksLanding` reads live. A reviewer requested
  after the cache filled is invisible to the advance; the landing catches it (wasted
  Validate, not a bad merge).
- **3.4 A new blockedBy edge may be invisible** (medium, partly unverified). `checkDependencies`
  reads live only when the item is already blocked, no webhook carries the edge (#1995),
  and 2.7 can mask it. The landing re-checks live, so a merge is guarded; stage work is not.
- **3.5 A READY check-run set is trusted for its head forever** (low–medium). A rerun or a
  late check on the same SHA is invisible without a `check_run` webhook; `ciSuiteHold`'s
  live suite read covers the CI and landing gates.

## 4. Engine-private state that changes decisions and does not survive a restart

Besides 1.3:

- **4.1 Merge-train caps** (medium). `mergeTrainEjectionCounts` is neither consecutive
  (as its comment says) nor tied to the head, and resets on restart; the trial-rate window
  and auto-repair attempts reset too. A frequently restarted daemon can run past the caps
  that bound expensive loops.
- **4.2 The Claude usage-limit hold** (medium). Held only in memory (engine.go:366). After a
  restart `settleClaudeLimitLabelSweep` strips `fabrik:claude-limit` everywhere, every slot
  dispatches into the limit, and each item gets its explanatory comment again.
- **4.3 Reinvoke workers have no cancel handle** (medium, TUI path unverified). `issueCtxs`
  is registered only for stage runs, so a stop request on a comment/review/CI-fix/rebase
  reinvoke pauses the item while the worker keeps running and pushing.
- **4.4 `feedbackGaps` is never cleared** (medium, #2160 follow-on). Once the repair budget
  for a gap is spent, the gate holds until a restart.

Everything else the engine keeps outside the Store (dedup sets, caches, presentation
memory, the persisted train run, sessions) is legitimately private; see the inventory in
the slice D notes. Anomaly retries/holds duplicate the Store's `PendingRetry`/`GateHold`
and should be derived from it.

## 5. What a second, direct webhook source needs (in addition to §2)

- One engine-level `X-GitHub-Delivery` dedupe in front of `deltaFn`, ideally persisted
  (today gh-forward has none and Hookdeck has a private per-source one).
- A per-item serial apply queue, and a fast ack: GitHub times out at 10 s and does not
  retry, but `deltaFn` makes network calls before the 200.
- Signature verification with a configured secret (the gh-forward rotation logic must not
  apply); the listener's 10 MiB cap is below GitHub's 25 MB.
- Code that assumes two transports: the single `e.webhookMgr`, `RefuseHookdeckWithWebhooks`,
  the type switches in `transitionMgrHealthState` and the reconcile coverage checks
  (reconcile.go:58–62, 107–114). A third type gets no health transitions silently.
- Subscriptions for facts the engine uses but never hears about: `issue_dependencies`,
  `sub_issues`, `pull_request_review_thread`, `label` renames, `milestone` edits.
- The 1.6 filter, or a direct org/App hook delivers every project and repo.
- Hookdeck's reconnect does not trigger the catch-up reconcile its documentation promises.

## 6. Proposed work, in order

**Fix now** (milestone 0.0.88, each a spec-kit issue):

1. Respect `fabrik:paused` at every advance and landing site (1.1): #2341.
2. Pin every engine merge and auto-merge to the head the gates judged (1.2): #2342.
3. Persist or rebuild the Validate completion SHA (1.3): #2343.
4. Stop the poll's snapshot write-back of shallow fields, and carry the merge-queue fields
   through the cache (1.4, 1.5): #2344.
5. Filter `projects_v2_item` by our project and Status field IDs (1.6): #2345.
6. Write the comment-review body through to the Store (1.7): #2346.
7. Apply the Layer 1 refresh as a read-sourced, time-stamped mutation, not an engine write (1.8): #2347.

**State foundation** (milestone `statefix`, a blocked-by chain; #2143 is blocked by #2345, #2351, #2352 and #1995):

8. One "observed at" rule for every write into the Store: listings carry `FetchedAt`, deep
   fetches carry their fetch time, webhooks carry the payload's GitHub time or the
   delivery's receipt order. A write older than the field's last observation is dropped,
   whoever made either one. This subsumes the engine-write protection and closes §2.1–2.6: #2348.
9. A GitHub-clock freshness baseline: #1945 (existing), with the body fix above.
10. Durable decision state: train caps keyed to head/stage entry, the usage-limit hold
    (seed from the label), cancel handles for every worker kind, `feedbackGaps` reset (§4): #2349.
11. Dispatch re-reads each candidate from the Store after the poll's own writes; live reads
    in gates are applied to the Store before deciding (§3.1, 3.2). One "read live, apply,
    decide from the Store" helper: #2350.
12. Delivery dedupe, per-item serial apply, fast ack and the transport-neutral manager
    (§5): #2351; then the missing subscriptions: #1995 (existing) and #2352.

Each fix needs a sim twin (tests/sim) and, for 1.1, 1.2, 1.6 and item 8, a live e2e case,
since the sim cannot model GitHub's listing lag.

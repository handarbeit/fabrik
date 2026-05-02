---
description: Summarise what Fabrik is doing right now — board state, in-flight workers, worktrees with uncommitted changes
---

Check the current state of the Fabrik project. Run these probes in parallel where possible, then summarise.

1. **Running workers.** Look for live Claude Code workers spawned by the engine:
   ```bash
   ps aux | grep "claude.*print" | grep -v grep
   ```

2. **Project board state.** Read `.fabrik/config.yaml` to find the configured `owner` and `project` number, then fetch the board via GraphQL:
   ```bash
   gh api graphql -f query='
     query($owner: String!, $number: Int!) {
       organization(login: $owner) {
         projectV2(number: $number) {
           items(first: 100) {
             nodes {
               content {
                 ... on Issue {
                   number
                   title
                   labels(first: 20) { nodes { name } }
                 }
               }
               fieldValues(first: 20) {
                 nodes {
                   ... on ProjectV2ItemFieldSingleSelectValue {
                     name
                     field { ... on ProjectV2SingleSelectField { name } }
                   }
                 }
               }
             }
           }
         }
       }
     }' -f owner="$OWNER" -F number="$NUM"
   ```
   Note: if `owner_type` in config.yaml is `user`, use `user(login: ...)` instead of `organization(login: ...)`.

3. **Worktrees with uncommitted changes.** Catches issues whose workers may have been interrupted:
   ```bash
   for d in .fabrik/worktrees/*/issue-*/; do
     status=$(git -C "$d" status --short 2>/dev/null)
     if [ -n "$status" ]; then
       echo "$d:"
       echo "$status"
     fi
   done
   ```

4. **Engine log tail** (optional, only if something looks wrong):
   ```bash
   tail -50 .fabrik/fabrik.log
   ```

## Summary format

Group by stage column. For each issue in flight, show:

- `#N — Title (Stage)` — labels of interest (`fabrik:locked:*`, `fabrik:awaiting-*`, `stage:*:in_progress`, `stage:*:failed`, `model:*`, `effort:*`, `fabrik:yolo`/`cruise`).
- Any worker process associated with it.
- Worktree dirty status if any.

End with a short **Attention** section calling out:

- Issues with `stage:*:failed` (hit retry limit).
- Issues with `fabrik:awaiting-input` (need a comment to resume).
- Issues with `fabrik:paused` (manually paused).
- Worktrees with uncommitted changes that don't correspond to a running worker (possible interrupted state).
- Anything else genuinely unusual.

If nothing needs attention, say so in one line.

Keep the summary scannable — bullets, not prose.

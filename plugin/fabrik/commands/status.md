---
description: Summarise what Fabrik is doing right now — board state, in-flight workers, worktrees with uncommitted changes
---

Check the current state of the Fabrik project. Run these probes (in parallel where possible), then summarise.

## 1. Read the project config

Use the `Read` tool on `.fabrik/config.yaml` to extract:
- `owner` (GitHub org or user)
- `project` (project number, integer)
- `owner_type` if present (`organization` or `user` — defaults to `organization` when omitted)

You'll inline these values into the GraphQL query in step 3. Don't try to set shell variables from config.yaml in a heredoc — just substitute the values directly into the `gh api graphql` invocation.

## 2. Find running workers

Fabrik spawns workers as `claude` with stage flags including `--output-format stream-json` and `--plugin-dir .fabrik/plugin`. Either pattern is a reliable detector:

```bash
ps aux | grep -E "claude.*(--output-format stream-json|--plugin-dir.*\.fabrik/plugin)" | grep -v grep
```

Each match line contains the working directory in the args (`--add-dir` or `cwd` via `lsof` if needed); cross-reference with worktree paths in step 4 to associate workers to issues.

## 3. Fetch the project board

Use the values you read in step 1. Pick `organization(login: ...)` for `owner_type: organization` (the default) or `user(login: ...)` for `owner_type: user`. Substitute `<OWNER>` and `<NUM>` with the literal values from config.yaml:

```bash
gh api graphql -f query='
  query {
    organization(login: "<OWNER>") {
      projectV2(number: <NUM>) {
        items(first: 100) {
          nodes {
            content {
              ... on Issue {
                number
                title
                state
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
  }'
```

If you prefer parameterised variables: `gh api graphql -F owner=<OWNER> -F number=<NUM> -f query='query($owner: String!, $number: Int!) { ... }'` works too.

## 4. Worktrees with uncommitted changes

Catches issues whose workers may have been interrupted (the worktree carries the partial work):

```bash
for d in .fabrik/worktrees/*/issue-*/; do
  status=$(git -C "$d" status --short 2>/dev/null)
  if [ -n "$status" ]; then
    echo "$d:"
    echo "$status"
  fi
done
```

## 5. Engine log tail (optional)

Only run this if step 2 or 4 surfaces something odd:

```bash
tail -50 .fabrik/fabrik.log
```

## Summary format

Group by stage column. For each issue in flight, show:

- `#N — Title (Stage)` — labels of interest (`fabrik:locked:*`, `fabrik:awaiting-*`, `stage:*:in_progress`, `stage:*:failed`, `model:*`, `effort:*`, `fabrik:yolo`/`cruise`).
- Any worker process associated with it (match the worker's working directory to the issue's worktree path).
- Worktree dirty status if any.

End with a short **Attention** section calling out:

- Issues with `stage:*:failed` (hit retry limit).
- Issues with `fabrik:awaiting-input` (need a comment to resume).
- Issues with `fabrik:paused` (manually paused).
- Worktrees with uncommitted changes that don't correspond to a running worker (possible interrupted state).
- Anything else genuinely unusual.

If nothing needs attention, say so in one line. Keep the summary scannable — bullets, not prose.

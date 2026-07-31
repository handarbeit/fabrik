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

Fabrik spawns every worker with `--plugin-dir <abs-path-to-.fabrik/plugin>` (`runClaude` appends this flag on every invocation). This is the **only** reliable detector.

**`--output-format stream-json` must never be used as a detector, alone or in combination.** It's the generic Claude Code headless flag — any headless `claude` process on the machine matches it, including other plugins' background daemons (e.g. `claude-mem`), other agent harnesses, or a user's own scripts. None of those processes carry `--plugin-dir .fabrik/plugin`. Matching on it (even as one arm of an "either/or") reintroduces false positives; don't reintroduce it here even for "redundancy."

Scope the match to **this project's absolute plugin-dir path**, not just the relative `.fabrik/plugin` suffix — Fabrik commonly runs as a fleet of concurrent per-repo instances on one host, and a relative match would also count a *different* repo's Fabrik workers as this project's:

```bash
FABRIK_PLUGIN_DIR="$(pwd)/.fabrik/plugin"
ps aux | grep -F -- "--plugin-dir $FABRIK_PLUGIN_DIR" | grep -v grep
```

Use `$(pwd)`, not `realpath`/`pwd -P` — Fabrik itself derives this path from `os.Getwd()` without resolving symlinks, so a symlink-resolving form could produce a path that no longer textually matches what a running worker actually passed on its command line. Use `grep -F` (fixed string), not an interpolated regex, so the path's literal characters can't be misread as regex metacharacters.

Two distinct edge cases, handled differently — don't conflate them:

- **`.fabrik/plugin` doesn't exist for this project**: Fabrik never emits `--plugin-dir` in this case (the flag is guarded by `claudePluginDir != ""`), so the detector above correctly finds zero matches. Report "0 workers running" — this is a "detects nothing" outcome, not an error, and there is no fallback to apply.
- **`$(pwd)` can't be resolved**: fall back to a relative match, `ps aux | grep -F -- "--plugin-dir .fabrik/plugin" | grep -v grep`, and the summary **must** state the caveat: "other Fabrik instances on this host may be included." Only use this fallback when the absolute path itself is unavailable — never as a default.

Each match line contains the worker's working directory in the args (`--add-dir` or `cwd` via `lsof` if needed); cross-reference with worktree paths in step 4 to associate workers to issues. A matched worker whose working directory cannot be cross-referenced to a worktree in this project must be reported as **unassociated** — never silently attributed to an issue.

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

- `#N — Title (Stage)` — labels of interest (`fabrik:locked:*`, `fabrik:awaiting-*`, `stage:*:in_progress`, `stage:*:failed`, `effort:*`, `fabrik:yolo`/`cruise`).
- **Model**: report only from these sources, in this precedence order, and never guess:
  1. The issue's own `model:<name>` label, if present.
  2. Otherwise, the stage's `model:` field in `.fabrik/stages/<stage>.yaml`.
  3. Otherwise, the `--model` argument of the **specific worker process matched to this issue** in step 2 — read it from that single matched `ps` line only, never from a global grep for `--model` across all `ps` output (that would reattach one worker's model to another issue, the same class of bug this fix corrects). Only use this source for a worker positively identified as this project's worker and associated with this issue in step 4; never read a model from an unassociated or unmatched process.
  If none of these three sources yields a value, report the model as **unknown** — do not guess from any other string. Always print the value verbatim as configured (e.g. `sonnet`, `opus`) — never expand an alias to a resolved version string (e.g. never render `opus` as `claude-opus-5`); Fabrik itself never resolves aliases, so any expansion here would itself be a guess.
- Any worker process associated with it (match the worker's working directory to the issue's worktree path, per step 2). If no match is found, say so explicitly rather than omitting the line.
- Worktree dirty status if any.

End with a short **Attention** section calling out:

- Issues with `stage:*:failed` (hit retry limit).
- Issues with `fabrik:awaiting-input` (need a comment to resume).
- Issues with `fabrik:paused` (manually paused).
- Worktrees with uncommitted changes that don't correspond to a running worker (possible interrupted state).
- Anything else genuinely unusual.

If nothing needs attention, say so in one line. Keep the summary scannable — bullets, not prose.

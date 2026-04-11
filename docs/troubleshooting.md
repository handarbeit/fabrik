---
layout: docs
title: Troubleshooting
---

# Troubleshooting

Common issues and how to resolve them.

## Duplicate Comments on Issues

**Symptom:** A single stage run produces multiple identical or near-identical comments on the issue, each with the `🏭 Fabrik — stage:` header.

**Cause:** Globally-installed Claude Code plugins (especially `superpowers`) inject SessionStart hooks that cause Claude to spawn parallel Agent subagents. Each subagent produces separate output that Fabrik posts as individual comments.

**Fix:**
- Remove interfering global plugins: `rm -rf ~/.claude/plugins/cache/claude-plugins-official/superpowers`
- Check for other global plugins that inject hooks: `ls ~/.claude/plugins/cache/claude-plugins-official/`
- Fabrik's behavior should be governed only by the repo's CLAUDE.md and the Fabrik plugin — not personal global plugins

**Prevention:** Avoid installing global Claude Code plugins on machines that run Fabrik. If you need plugins for interactive use, consider running Fabrik from a dedicated user account or CI environment.

## Multiple Fabrik Instances

**Symptom:** Issues get processed multiple times concurrently, with overlapping comments and wasted API credits.

**Cause:** Two or more Fabrik processes are running against the same project board. Each has its own in-memory deduplication, so they can't detect each other.

**Fix:**
- Check for running instances: `pgrep -la fabrik`
- Kill duplicates and keep one
- Fabrik v0.0.30+ includes a PID file lock (`.fabrik/fabrik.lock`) that prevents starting a second instance for the same project

## Startup Board Validation Failure

**Symptom:** Fabrik exits immediately with an error about mismatched stage names.

**Cause:** Stage names in `.fabrik/stages/*.yaml` don't match the column names on your GitHub Project board.

**Fix:** Compare your stage YAML `name:` fields with the board column names. They must match exactly (case-sensitive).

## Max Turns Exceeded

**Symptom:** Stage posts partial output with "Stage incomplete" and retries.

**Cause:** Claude used all allowed turns without emitting `FABRIK_STAGE_COMPLETE`.

**Fix:**
- Increase `max_turns` in the stage YAML
- Split the issue into smaller pieces
- Check if the prompt is too broad or ambiguous

## Rate Limit Exhaustion

**Symptom:** Fabrik logs `GraphQL rate limit low` warnings or `API rate limit already exceeded` errors.

**Cause:** Too many API calls in the rate limit window. Common triggers: multiple Fabrik instances, very short poll intervals, or many items on the board requiring deep-fetch.

**Fix:**
- Ensure only one Fabrik instance is running (`pgrep -la fabrik`)
- Increase `poll` interval in `.fabrik/config.yaml` (default: 15 seconds)
- Fabrik automatically backs off when rate limits are low, but recovery takes up to 1 hour

## Merge Conflicts in Worktree

**Symptom:** Stage fails or produces unexpected output after a worktree update.

**Cause:** `updateWorktreeFromMain` merges `origin/main` into the issue branch. If there are conflicts, they're left as conflict markers for Claude to resolve.

**Fix:** Check `git status` in the worktree (`.fabrik/worktrees/<repo>/issue-N/`). Claude should resolve conflicts on the next stage run. If it can't, resolve manually and remove `fabrik:paused`.

## Claude Auth Errors

**Symptom:** Claude exits with "Not logged in" or authentication errors.

**Cause:** The Claude Code CLI isn't authenticated, or the auth token has expired.

**Fix:** Run `claude login` to re-authenticate. Fabrik uses whatever auth method Claude Code is configured with (OAuth, API key, etc.).

## Worktree Clone Failures

**Symptom:** Issues get `fabrik:paused` + `fabrik:awaiting-input` labels with a "cannot clone repo" comment.

**Cause:** Fabrik couldn't bare-clone the repository. Common reasons: SSH key expired, network issues, or permission denied.

**Fix:**
- Check SSH keys: `ssh-add -l` and `ssh -T git@github.com`
- Verify the GitHub token has `repo` scope
- Remove `fabrik:paused` label after fixing to retry

# ADR 049: Worktree Boundary Enforcement

**Date**: 2026-05-24  
**Status**: Accepted

## Context

Fabrik's tool permission model grants `Bash(git:*)` and `Bash(gh:*)` globally — these restrict which *commands* can run, but not *where*. A Claude session in the Implement stage can `cd` to any directory and run git operations or `gh pr create` against an arbitrary repository. This is not theoretical: `acme/widgets-graph#42` produced an unexpected cross-repo PR (`acme/widgets#785`) by Claude navigating into the user's local working copy and creating a branch + PR there directly, completely bypassing Fabrik's worktree model.

The forthcoming multi-repo orchestration spec adds prompt-level guardrails, but prompt-level prohibition is not sufficient — a sufficiently confused or adversarial Claude session can still escape. This ADR describes the engine-level enforcement layer that makes worktree boundaries structurally unbreakable regardless of what Claude's prompt or the user's issue spec says.

## Decision

Deploy a **dual enforcement layer** — proactive (tool-permission) and reactive (post-run audit) — applied to all non-read-only, non-unrestricted stage invocations.

### Layer 1: Proactive Tool-Permission Restriction

In `buildClaudeArgs` (`engine/claude.go`), replace bare `"Edit"` and `"Write"` entries in `--allowedTools` with path-scoped variants:

```
--allowedTools Edit(<workDir>/**)
--allowedTools Write(<workDir>/**)
```

Claude Code's `--allowedTools` flag accepts the same `Tool(pattern)` syntax as `Bash(git:*)`. Path-scoped `Edit`/`Write` entries restrict Claude's file-editing tools to the assigned worktree directory. Attempts to edit outside the path result in a tool-level error returned to Claude; the stage continues running (the violation is blocked, not the stage).

This is implemented in `applyWorktreeBoundary` (`engine/boundary.go`) — a pure function that takes the tool list and workDir and returns a new list with bare `Edit`/`Write` replaced. Called from `buildClaudeArgs` when `!unrestricted && !stage.ReadOnly && workDir != ""`.

### Layer 2: Reactive Post-Run Audit

Before the extension loop in `processItem` (`engine/item.go`), snapshot branch refs across all registered bare-clone repositories via `snapshotAllRepoRefs`. After the loop, compare via `crossRepoViolations`. Any ref that is new, changed, or deleted in a repo *other than* the active issue's repo is a boundary violation. Repos not present in the before-snapshot (lazy-registered during the run or whose pre-audit snapshot failed) are excluded from comparison to prevent false positives.

On violation:
- A comment is posted on the issue naming the specific refs mutated.
- `fabrik:paused` is added so `itemNeedsWork` skips the issue until the user investigates. Without `fabrik:paused`, the `clearFailedStage` path in `processItem` would auto-clear the failed label on the next poll cycle.
- `stage:<name>:failed` is added. `StageAttempted` is recorded (cooldown applies). `MaxRetries` is NOT consumed — violations require human investigation, not auto-retry.
- `EnginePaused` is recorded in the store so that `clearFailedStage` fires (removing the failed label) when the user removes `fabrik:paused`.
- No automatic cleanup of unauthorized external state (branches created, PRs opened in wrong repos) — surface to human.

The audit is implemented in `snapshotRepoRefs` and `crossRepoViolations` (`engine/boundary.go`), with orchestration in `snapshotAllRepoRefs` and `handleBoundaryViolation` (`engine/item.go`).

### Gating Conditions

Both layers gate on `!unrestricted && !stage.ReadOnly`:

- **`fabrik:unrestricted`**: passes `--dangerously-skip-permissions` instead of `--allowedTools`, bypassing all tool restrictions. Worktree-boundary enforcement is also bypassed — consistent with the existing semantics of that label.
- **Read-only stages** (Specify, Research, Plan): stash/restore worktree changes and do not produce commits or file writes. Neither layer applies.
- **Single-repo projects**: the path restriction still applies (preventing out-of-worktree file writes). The cross-repo audit produces no violations — the active repo is excluded, and there are no other registered repos.

## Rationale

### Why not OS-level sandboxing?

macOS `sandbox-exec` profiles and Linux mount namespaces (`unshare`) could enforce filesystem isolation at the kernel level, making bypass structurally impossible. These were rejected because:

1. **Platform-specific**: `sandbox-exec` is macOS-only and deprecated; Linux namespaces require elevated permissions or `newuidmap`/`newgidmap` helpers. Fabrik runs on both platforms; a cross-platform solution would require maintaining two separate sandbox implementations.
2. **Elevated permissions**: Namespace-based isolation (e.g., `unshare -m`) typically requires root or specific capability grants (`CAP_SYS_ADMIN`). Fabrik is a CLI tool; requiring elevated permissions is a non-starter for most users.
3. **Tool-layer enforcement is sufficient for the threat model**: The primary concern is a confused Claude session, not an adversarial one with root access. Tool-permission enforcement blocks the `Edit`/`Write` tools that Claude Code uses for file writes. The post-run audit catches the git-layer mutations that shell commands might produce.

### Why git refs rather than filesystem state?

The post-run audit targets git refs (branch/tag names and their SHAs) rather than filesystem writes because:

1. **Filesystem monitoring is expensive and platform-specific**: `inotify` (Linux), `kqueue` (macOS), and `FSEvents` (macOS) are all platform-specific. Polling `find /` for new files after each invocation is too slow and produces too much noise.
2. **Git refs are the observable harm**: The incident that motivated this ADR (`acme/widgets-graph#42`) caused harm by pushing commits and opening a PR in the wrong repo — git-layer mutations. A file write outside the worktree that is never committed and never pushed is recoverable; a pushed branch or opened PR is not.
3. **`git for-each-ref` is fast and cross-platform**: On typical Fabrik deployments (1-5 repos, a few hundred branches), the snapshot takes <10ms per repo and has no platform-specific dependencies.

### Why not just restrict `Bash(git:*)` to the worktree path?

The `Bash(cmd:*)` pattern restricts by command name, not by argument path. `Bash(git:*)` cannot be scoped to `git --work-tree=/path:*` at the tool-permission layer — the pattern matches the subcommand token, not the full argument string. This is a known limitation of Claude Code's `--allowedTools` syntax. The post-run audit is the mitigation.

### Why not increment MaxRetries on violation?

Boundary violations are not transient errors (network blips, rate limits) that benefit from automatic retry. They indicate a structural problem — a confused Claude session, a misconfigured spec, or a potential prompt-injection attack. Retrying without human review could repeat the violation. `fabrik:paused` and `stage:<name>:failed` are both added, matching the pattern of `escalateFailedStage`. `StageAttempted` is recorded (cooldown applies) but `StageRetryIncremented` is not called (MaxRetries is preserved). When the user removes `fabrik:paused`, `clearFailedStage` removes the failed label and the stage can be retried cleanly.

## Known Limitations

1. **Bash shell writes are not path-constrainable**: `cat > /other/path`, `tee`, `cp`, and similar shell-level writes cannot be blocked at the tool-permission layer. Claude Code uses `Edit`/`Write` tools for most file writes; raw shell I/O to outside paths is less common but not impossible. The git-ref audit provides a backstop for the most harmful variant (commits pushed to wrong repos).

2. **Unregistered repos are not audited**: Repos are registered lazily when Fabrik first encounters them. If Claude navigates into a local git repo that is not in Fabrik's managed set (`worktreeManagers`), the audit does not see it. This is acceptable — Fabrik cannot audit repos it has never been told about.

3. **Active repo exclusion**: The active issue's repo is unconditionally excluded from the violation check — all ref changes there are assumed legitimate (commits, branch pushes from the stage's work). This is correct in single-stage mode; in a hypothetical future where one invocation manages multiple repos, the exclusion logic would need revisiting.

4. **Snapshot window**: The snapshot is taken before the extension loop and compared after. Concurrent Fabrik workers can mutate refs in other repos during the window. The design scopes the exclusion to the *active repo* rather than a time window, so Worker A's legitimate push to repo-X does not trigger a violation in Worker B (whose active repo is Y and thus audits repo-X). False positives from concurrent workers are therefore limited to: Worker B actively pushing to repo-Y (already excluded) while Worker A's active repo is also Y (prevented by the in-flight guard).

## References

- [ADR 005](005-claude-cli-invocation.md): Claude CLI shell-out and `--allowedTools`
- [ADR 006](006-git-worktrees.md): Worktree path structure
- [Stage Lifecycle §Phase 2: Worktree Boundary Enforcement](../docs/stage-lifecycle.md)
- [Stage Lifecycle §Phase 3: Post-Run Boundary Audit](../docs/stage-lifecycle.md)
- `engine/boundary.go`: `applyWorktreeBoundary`, `snapshotRepoRefs`, `crossRepoViolations`
- `engine/item.go`: `snapshotAllRepoRefs`, `handleBoundaryViolation`

# Fabrik v0.0.43

## Features

- **CI gate before auto-merge (#418).** New `wait_for_ci: true` stage config option (enabled by default on Validate). The engine checks CI check-run status via REST before allowing auto-merge. If checks are still running, the merge blocks until they complete. If checks fail, the engine dispatches a CI-fix reinvoke — Claude is re-invoked with the failure context and base-branch CI comparison to fix regressions. Cycle limit and timeout prevent runaway loops. New `fabrik:awaiting-ci` label tracks CI gate state.
- **Sync PR base branch on `base:<branch>` label change (#416).** When the `base:<branch>` label is added or changed mid-pipeline, Fabrik now updates the existing PR's base branch via the GitHub API. Previously, PRs created before the label change kept targeting the old base.

## Fixes

- **Stale default-branch detection after bare clone.** `DefaultBaseBranch` now runs `git remote set-head origin --auto` on every `ensureBareClone` entry, keeping `refs/remotes/origin/HEAD` in sync with the remote. The fallback ladder is reordered: authoritative `ls-remote` now takes precedence over the frozen bare-clone local HEAD. Fixes PRs targeting the wrong base when a repo's default branch changed after Fabrik's initial clone.
- **Copilot review findings on CI gate code** — API errors in `FetchLinkedPR`/`FetchCheckRuns` now block instead of silently clearing the gate. `CheckRun.ID` comment corrected. State machine doc Phase 1 ordering fixed.

## Improvements

- Advanced `base:<branch>` timing semantics documented in USER_GUIDE.md — full matrix of when the label takes effect cleanly vs. with caveats.
- Review gate documentation updated across USER_GUIDE.md and README.md for the v0.0.42 dual-condition behavior (`LinkedPRReviewRequests` empty AND `LinkedPRReviews` non-empty).
- State machine documentation expanded with CI gate sections (§6.x CI Gate and CI-Fix Reinvoke, new sub-states, transition tables, Mermaid diagrams).

## Internal

- New `engine/ci.go` + `engine/ci_test.go` — mirrors the review-reinvoke pattern (`checkCIGate`, `dispatchCIFixReinvoke`, `pauseForCITimeout`, `pauseForCIFixCycleLimit`).
- `GetPRBase` and `UpdatePRBase` added to `github/prs.go` + interface.
- `syncPRBase` called from `processItem` and `processComments` at each stage invocation.
- `WaitForCI` bypass added to `itemMayNeedWork` for items with stage-complete labels.

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly (auto-detects OS and architecture)
gh release download --repo handarbeit/fabrik \
  --pattern "fabrik_*_$(uname -s | tr A-Z a-z)_$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/').tar.gz" \
  -O - | tar xz
```

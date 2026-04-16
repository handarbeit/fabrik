# Fabrik v0.0.41

## Fixes

- `MergePR` now returns success when the PR is already merged (e.g., merged manually by a human). Previously, an already-merged PR had `mergeable: null`, which returned `ErrNotMergeable` — causing the yolo catch-up loop to skip advancement to Done forever. The issue would sit stuck in the Validate column on every poll cycle.
- Review and review-comment skills now prohibit bare `#N` ordinals when numbering findings (#410). GitHub's issue renderer auto-links `#N` tokens to unrelated issues in the same repo, producing confusing output where reviewer finding labels like "Gemini #1" expand to include the title of whatever issue #1 happens to be.
- Release download command in the `cut-release` skill and all published release notes now uses the canonical auto-detect form from the marketing site (correct repo `shadoworg/fabrik`, auto-detects OS and architecture via `uname`).
- Verification section auto-update gating condition and review cycle limit comment corrected.

## Improvements

- USER_GUIDE.md, README.md, and docs/index.md updated for v0.0.39 behavior changes (review-feedback processing for all issues, PR summary comments, idle backoff, rate-limit backoff).

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly (auto-detects OS and architecture)
gh release download --repo shadoworg/fabrik \
  --pattern "fabrik_*_$(uname -s | tr A-Z a-z)_$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/').tar.gz" \
  -O - | tar xz
```

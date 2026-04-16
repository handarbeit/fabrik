# Fabrik v0.0.42

## Fixes

- **Review gate now waits for actual review submission, not just requested reviewers.** `checkReviewGate` previously cleared immediately when `LinkedPRReviewRequests` was empty — but Copilot, Gemini, and other bot reviewers self-trigger via webhooks and never appear in the formal requested-reviewer list. With `fabrik:yolo` active, the pipeline raced through Validate → merge → Done in 30-60 seconds while bots were still processing their reviews. The gate now requires both `LinkedPRReviewRequests` empty AND `LinkedPRReviews` non-empty before clearing, which catches self-submitting bot reviews naturally. Existing `ReviewWaitTimeout` (default 15 min) is the fallback when no reviews ever arrive.
- **`fabrik-implement`, `fabrik-review`, and `fabrik-validate` skills now require per-test timeouts.** A hanging pytest suite with no timeout flag kept a Claude CLI process alive for 39+ minutes after the Review stage completed (burning three full Review runs before manual intervention). Skills now instruct: always include `--timeout=60` for pytest, `-timeout 5m` for `go test`, `--testTimeout=30000` for jest, etc.

## Improvements

- Documentation and grammar refinements to `fabrik:yolo` and yolo auto-merge sections.

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly (auto-detects OS and architecture)
gh release download --repo shadoworg/fabrik \
  --pattern "fabrik_*_$(uname -s | tr A-Z a-z)_$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/').tar.gz" \
  -O - | tar xz
```

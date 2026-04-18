# Fabrik v0.0.44

## Fixes

- **Rate-limit backoff hysteresis (#420).** Rate-limit backoff no longer resets eagerly on any activity detection. The backoff now uses a hysteresis threshold: it only relaxes when GraphQL quota recovers above 50%, not on the first `updatedAt` change. Idle backoff (no dispatches) still resets on activity as before. The `fabrik:blocked` deep-fetch bypass that contributed to quota pressure has been removed — blocked items now use the standard `processedSet` cooldown for re-evaluation.
- **Removed `update_issue_body` stage config flag (#419).** This flag was a silent breaking change for existing projects — it defaulted to false but the Specify stage depended on it being true. The feature has been removed entirely; all references cleaned from docs, stage YAMLs, and engine code.
- **Narrowed `#N` auto-link prohibition in skills (#410).** The blanket prohibition on `#N` was too strict — it blocked intentional issue references like "see #392". The skill guidance now only prohibits `#N` when used as ordinal labels (e.g., "Copilot #1", "finding #2"); deliberate issue/PR references are allowed.
- **Corrected MaxTurns in pipeline table.** Specify and Review were showing incorrect defaults (20 and 30); corrected to 50 to match actual stage YAML config.
- **Set `processedSet` on blocked items** so cooldown-based re-evaluation works correctly after the `fabrik:blocked` deep-fetch bypass was removed.

## Improvements

- `fabrik:awaiting-ci` label and CI gate description added to README and USER_GUIDE label tables.
- CI Gate feature card added to the marketing site features grid.
- Rate-limit backoff and dependency detection documentation updated in state-machine.md.
- All `*-comment` skills now carry the narrowed `#N` ordinal prohibition (R2 from #410).

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly (auto-detects OS and architecture)
gh release download --repo shadoworg/fabrik \
  --pattern "fabrik_*_$(uname -s | tr A-Z a-z)_$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/').tar.gz" \
  -O - | tar xz
```

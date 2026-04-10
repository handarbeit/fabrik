# Fabrik v0.0.28

## Improvements

- **Releases now published to shadoworg/fabrik** — Binary releases are published to the public repo via goreleaser cross-repo support. The auto-upgrade feature will be updated to fetch from the new location in a future release.

## Internal

- Documentation updates for v0.0.27 auto-archive changes.

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly
gh release download --repo shadoworg/fabrik --pattern '*darwin_arm64*' -O - | tar xz
```

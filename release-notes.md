# Fabrik v0.0.6

## Bug Fixes

### Auto-Upgrade Asset Name Mismatch Fixed (#214, #217)

The `fabrik upgrade` command failed silently when checking for new releases. The
upgrade code constructed the expected asset filename using the raw git tag (e.g.
`v0.0.6`), producing `fabrik_v0.0.6_darwin_arm64.tar.gz`. However, GoReleaser
strips the `v` prefix when building asset names — the actual file is
`fabrik_0.0.6_darwin_arm64.tar.gz`. The asset lookup found no match and the
upgrade appeared to do nothing. The fix applies `strings.TrimPrefix` to remove
the leading `v` before constructing the filename, so `fabrik upgrade` now
correctly downloads and installs new releases.

## Upgrading

```bash
# From a previous release binary
fabrik upgrade

# Or download directly
gh release download --repo tenaciousvc/fabrik --pattern '*.tar.gz' -O - | tar xz
```

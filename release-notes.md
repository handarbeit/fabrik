# Fabrik v0.0.7

## Bug Fixes

### Duration display shows correct zero-padded minutes (#219)

The elapsed-time counter in the TUI was missing zero-padding for minutes below
10. Durations like 1 minute 5 seconds appeared as `1:5` instead of the correct
`01:05`. All durations now display consistently as `MM:SS` (e.g., `00:30`,
`01:05`, `10:00`).

## Upgrading

```bash
# From a previous release binary
fabrik upgrade

# Or download directly
gh release download --repo handarbeit/fabrik --pattern '*.tar.gz' -O - | tar xz
```

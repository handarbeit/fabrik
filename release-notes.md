# Fabrik v0.0.19

## Fixes

- **Advanced items stuck after yolo catch-up** — The yolo catch-up loop evicted the `updatedAt` cache after advancing an item, but the deferred cache update at the end of the poll re-cached the old timestamp. The item appeared "unchanged" on the next poll and the new stage never ran. Fixed by excluding advanced items from the deferred cache update.

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly
gh release download --repo tenaciousvc/fabrik --pattern '*darwin_arm64*' -O - | tar xz
```

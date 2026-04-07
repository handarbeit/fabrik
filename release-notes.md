# Fabrik v0.0.12

## What's New

### Issue dependency gating (#228)

Issues can now declare blockers using GitHub's native "blocked by" relationship. When Fabrik detects that an issue is blocked, it applies the `fabrik:blocked` label, posts a comment explaining which issue is blocking progress, and skips the item until the blocker is resolved. Once the blocking issue is closed, Fabrik automatically removes the label and resumes normal processing. The TUI also displays blocked status inline so you can see at a glance which issues are waiting on dependencies.

## Bug Fixes

### Auto-upgrade fixed for private repos (#228)

The binary upgrade downloader was using `browser_download_url` from the GitHub releases API. GitHub redirects that URL to S3, which rejects authentication headers — causing private-repo downloads to fail with a 403. Fixed to use the GitHub API asset URL with `Accept: application/octet-stream`, which handles authentication correctly and streams the binary without redirect issues.

### Auto-upgrade was using wrong repo owner (#228)

The `fabrikOwner` constant was hardcoded to `verveguy` instead of `tenaciousvc`. This caused the upgrade check to look in the wrong repository for new releases. Fixed to use the correct organization.

### Yolo catch-up loop skipped dependency unblocking (#228)

In yolo mode, the catch-up loop had an early `fabrik:blocked` check that returned before `checkDependencies` could run. This meant blocked issues were never re-evaluated and would stay blocked even after their blocker was resolved. Removed the early skip so dependency checks always execute.

### TUI header timer alignment (#228)

The elapsed-time counter in the TUI header was misaligned with the bordered pane content edge by one character. Fixed by adjusting the horizontal offset to `width-4`.

### Log file selection used wrong match field (#228)

The log file selector was matching by full filename instead of the timestamp suffix. This caused Fabrik to tail the wrong log file when multiple log files were present. Fixed to match on the timestamp component only.

### Upgrade check rate limiting removed (#228)

The upgrade check was rate-limited to once per several poll cycles, making it slow to detect new releases. Since the check is a single lightweight REST call, the rate limit was unnecessary. The check now runs every idle poll cycle.

## Documentation

This release includes comprehensive documentation updates:

- **USER_GUIDE.md**: Added documentation for the `fabrik:yolo` label, TUI keyboard shortcuts, watch mode shortcuts, log file paths, and a troubleshooting section.
- **README.md**: Added descriptions of the `model:<name>` and `fabrik:yolo` labels, and documented the `fabrik upgrade` command.
- **CLAUDE.md**: Updated stage config field reference, labels reference, startup board validation description, and multi-repo worktree paths.
- **docs/**: Updated the stage lifecycle page and the documentation index.

## Upgrading

```bash
# From a previous release binary
fabrik upgrade

# Or download directly
gh release download --repo tenaciousvc/fabrik --pattern '*.tar.gz' -O - | tar xz
```

---
name: fabrik-status
description: Check the current state of the Fabrik project board and running workers
---

Check the current state of the Fabrik project:

1. Run `ps aux | grep "claude.*print" | grep -v grep` to find running Claude workers
2. Run `gh api graphql` to fetch the project board and show:
   - Issues by stage (column) with their titles
   - Any issues with `fabrik:editing` or `fabrik:locked` labels
   - Open PRs and their mergeable status
3. Check for any worktrees with uncommitted changes: `for d in .fabrik/worktrees/issue-*/; do echo "$d:"; git -C "$d" status --short 2>/dev/null; done`

Present a concise summary of what Fabrik is doing and what needs attention.

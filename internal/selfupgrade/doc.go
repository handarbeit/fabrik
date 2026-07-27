// Package selfupgrade implements generic binary self-management: compare the
// running version against a source of truth, fetch a replacement, verify it,
// and re-exec. It supports two paths — a GitHub-Releases-backed release build
// (PerformReleaseUpgrade) and a git-source-checkout dev build
// (CheckAndRebuildDev) — both extracted from Fabrik's engine/upgrade.go.
//
// This package must never import engine. It exists precisely so that a
// second daemon (Pruefer) can gain self-upgrade capability without depending
// on engine's board/stage concepts — the boundary ADR-1113 established for
// Pruefer. Callers supply their own identity (binary name, release-asset
// owner/repo, version string, log function) and optional hooks
// (PostBuildHook, StatusFn/StatusClearFn); nothing here hardcodes "fabrik".
//
// See adrs/1196-extract-self-upgrade-package.md for the extraction rationale,
// including why this case was extracted rather than duplicated (the opposite
// call ADR-1113 Decision 6 made for process-group-kill logic).
package selfupgrade

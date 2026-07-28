// Package selfupgrade implements generic binary self-management: compare the
// running version against a source of truth, fetch a replacement, put it in
// place, and re-exec. "Put it in place" is deliberately not "verify": beyond
// an HTTP status check on the download and, on darwin, re-signing so the
// binary passes AMFI, there is no checksum or signature verification of
// release asset contents. It supports two paths — a GitHub-Releases-backed
// release build (PerformReleaseUpgrade) and a git-source-checkout dev build
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

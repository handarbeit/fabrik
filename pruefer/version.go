package pruefer

import "runtime/debug"

// Version is the pruefer binary version. For local/development builds this
// defaults to "dev" and is enriched with the short VCS revision at startup.
// Release builds inject the git tag via ldflags:
//
//	-X github.com/handarbeit/fabrik/pruefer.Version={{.Version}}
//
// This duplicates cmd/version.go's versionWithSHA/appendDirtyIfModified
// rather than importing package cmd, which pulls in engine — pruefer must
// never import engine (ADR-1113). See ADR-1197 for the duplication call.
var Version = "dev"

func init() {
	info, ok := debug.ReadBuildInfo()
	Version = versionWithSHA(Version, info, ok)
	Version = appendDirtyIfModified(Version, info, ok)
}

// versionWithSHA returns v unchanged if it is not "dev" (i.e. a real release
// version was injected via ldflags). For "dev" builds it distinguishes a
// working-tree build from an installed module:
//
//   - If vcs.revision is present, this binary was built by `go build`/`go run`
//     in a source checkout — report "dev(<short-sha>)". This check comes FIRST
//     because modern Go (1.24+) stamps a VCS-derived pseudo-version (e.g.
//     "v0.0.74-0.20260716173320-6198e8102f90+dirty") into Main.Version for such
//     builds, not the old "(devel)" sentinel. Keying on Main.Version would
//     otherwise report that pseudo-version, which breaks the "dev"-prefixed
//     auto-upgrade gate (pruefer/upgrade.go checkAndUpgrade) — the build would
//     take the release-download path and never rebuild from origin/main.
//   - Otherwise (no vcs.revision), a `go install pkg@vX.Y.Z` build carries the
//     resolved module version in Main.Version — report it directly.
//   - If neither is available, return "dev" unchanged.
func versionWithSHA(v string, info *debug.BuildInfo, ok bool) string {
	if v != "dev" {
		return v
	}
	if !ok || info == nil {
		return v
	}
	// Working-tree build: vcs.revision present → dev(<sha>), regardless of the
	// (possibly pseudo-version) Main.Version that modern Go stamps.
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			short := s.Value
			if len(short) > 7 {
				short = short[:7]
			}
			return "dev(" + short + ")"
		}
	}
	// No VCS info: an installed module (go install pkg@vX.Y.Z) carries its
	// resolved version in Main.Version.
	if mv := info.Main.Version; mv != "" && mv != "(devel)" {
		return mv
	}
	return v
}

// appendDirtyIfModified appends "+dirty" to v when the running binary's own
// embedded VCS build info reports vcs.modified=true — regardless of whether
// v is a ldflags-injected release version or a dev build. This surfaces a
// locally-built or improperly-released binary instead of it masquerading as
// a clean release.
func appendDirtyIfModified(v string, info *debug.BuildInfo, ok bool) string {
	if !ok || info == nil {
		return v
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.modified" && s.Value == "true" {
			return v + "+dirty"
		}
	}
	return v
}

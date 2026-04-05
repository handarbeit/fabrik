package cmd

import "runtime/debug"

// Version is the fabrik binary version. For local/development builds this
// defaults to "dev" and is enriched with the short VCS revision at startup.
// Release builds inject the git tag via ldflags:
//
//	-X github.com/handarbeit/fabrik/cmd.Version={{.Version}}
var Version = "dev"

func init() {
	info, ok := debug.ReadBuildInfo()
	Version = versionWithSHA(Version, info, ok)
}

// versionWithSHA returns v unchanged if it is not "dev" (i.e. a real release
// version was injected). For "dev" builds it appends the short VCS SHA from
// the build info, producing e.g. "dev(abc1234)". If VCS info is unavailable
// it returns "dev" unchanged.
func versionWithSHA(v string, info *debug.BuildInfo, ok bool) string {
	if v != "dev" {
		return v
	}
	if !ok || info == nil {
		return v
	}
	var revision string
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			revision = s.Value
			break
		}
	}
	if revision == "" {
		return v
	}
	// Use at most 7 characters of the SHA.
	short := revision
	if len(short) > 7 {
		short = short[:7]
	}
	return "dev(" + short + ")"
}

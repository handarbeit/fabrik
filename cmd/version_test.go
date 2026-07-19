package cmd

import (
	"runtime/debug"
	"testing"
)

func buildInfoWithRevision(rev string) *debug.BuildInfo {
	return &debug.BuildInfo{
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: rev},
		},
	}
}

func buildInfoWithMainVersion(mv string) *debug.BuildInfo {
	return &debug.BuildInfo{
		Main: debug.Module{Version: mv},
	}
}

func TestVersionWithSHA(t *testing.T) {
	tests := []struct {
		name     string
		v        string
		info     *debug.BuildInfo
		ok       bool
		expected string
	}{
		{
			name:     "injected release version passes through unchanged",
			v:        "v1.2.3",
			info:     buildInfoWithRevision("abc1234567890"),
			ok:       true,
			expected: "v1.2.3",
		},
		{
			name:     "dev with no build info degrades to dev",
			v:        "dev",
			info:     nil,
			ok:       false,
			expected: "dev",
		},
		{
			name:     "dev with empty VCS revision degrades to dev",
			v:        "dev",
			info:     buildInfoWithRevision(""),
			ok:       true,
			expected: "dev",
		},
		{
			name:     "dev with 7+ char SHA produces dev(short-sha)",
			v:        "dev",
			info:     buildInfoWithRevision("abcdef1234567"),
			ok:       true,
			expected: "dev(abcdef1)",
		},
		{
			name:     "dev with short SHA uses full value",
			v:        "dev",
			info:     buildInfoWithRevision("abc12"),
			ok:       true,
			expected: "dev(abc12)",
		},
		{
			name:     "dev with exactly 7 char SHA",
			v:        "dev",
			info:     buildInfoWithRevision("abcdef1"),
			ok:       true,
			expected: "dev(abcdef1)",
		},
		{
			name:     "go install pkg@vX.Y.Z reports Main.Version",
			v:        "dev",
			info:     buildInfoWithMainVersion("v0.0.71"),
			ok:       true,
			expected: "v0.0.71",
		},
		{
			name: "local checkout with (devel) Main.Version falls through to VCS SHA",
			v:    "dev",
			info: &debug.BuildInfo{
				Main: debug.Module{Version: "(devel)"},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "abcdef1234567"},
				},
			},
			ok:       true,
			expected: "dev(abcdef1)",
		},
		{
			// Regression: modern Go (1.24+) stamps a VCS-derived pseudo-version
			// into Main.Version for working-tree builds instead of "(devel)".
			// vcs.revision is still present, so this must report dev(<sha>) — not
			// the pseudo-version — or the "dev"-prefixed auto-upgrade gate breaks
			// and the instance never rebuilds from origin/main.
			name: "working-tree build with pseudo-version Main.Version still reports dev(sha)",
			v:    "dev",
			info: &debug.BuildInfo{
				Main: debug.Module{Version: "v0.0.74-0.20260716173320-6198e8102f90+dirty"},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "6198e8102f90bc3757461fdfc50707d0a4767cc7"},
					{Key: "vcs.modified", Value: "true"},
				},
			},
			ok:       true,
			expected: "dev(6198e81)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := versionWithSHA(tt.v, tt.info, tt.ok)
			if got != tt.expected {
				t.Errorf("versionWithSHA(%q, ...) = %q, want %q", tt.v, got, tt.expected)
			}
		})
	}
}

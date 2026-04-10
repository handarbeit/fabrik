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

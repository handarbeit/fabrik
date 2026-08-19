package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeManifest writes content to a temp file and returns its path.
func writeManifest(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "marketplace.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing manifest: %v", err)
	}
	return path
}

func TestGitSubdirPluginDirs(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []string
	}{
		{
			name: "returns git-subdir paths in file order",
			content: `{"plugins":[
				{"name":"a","source":{"source":"git-subdir","path":"plugin/a"}},
				{"name":"b","source":{"source":"git-subdir","path":"plugin/b"}}
			]}`,
			want: []string{"plugin/a", "plugin/b"},
		},
		{
			name: "skips non-git-subdir sources",
			content: `{"plugins":[
				{"name":"a","source":{"source":"git-subdir","path":"plugin/a"}},
				{"name":"remote","source":{"source":"github","path":"ignored"}}
			]}`,
			want: []string{"plugin/a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := gitSubdirPluginDirs(writeManifest(t, tt.content))
			if err != nil {
				t.Fatalf("gitSubdirPluginDirs() error = %v", err)
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("gitSubdirPluginDirs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGitSubdirPluginDirsErrors(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantSub string
	}{
		{
			name:    "no git-subdir plugins is an error, not an empty list",
			content: `{"plugins":[{"name":"remote","source":{"source":"github"}}]}`,
			wantSub: "no git-subdir plugins listed",
		},
		{
			name:    "git-subdir without a path is an error",
			content: `{"plugins":[{"name":"a","source":{"source":"git-subdir"}}]}`,
			wantSub: "has no source.path",
		},
		{
			name:    "whitespace in source.path is an error",
			content: `{"plugins":[{"name":"a","source":{"source":"git-subdir","path":"plugin/my plugin"}}]}`,
			wantSub: "whitespace in source.path",
		},
		{
			name:    "malformed JSON is an error",
			content: `{"plugins":[`,
			wantSub: "parsing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := gitSubdirPluginDirs(writeManifest(t, tt.content))
			if err == nil {
				t.Fatal("gitSubdirPluginDirs() error = nil, want error")
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantSub)
			}
		})
	}
}

func TestGitSubdirPluginDirsMissingFile(t *testing.T) {
	_, err := gitSubdirPluginDirs(filepath.Join(t.TempDir(), "absent.json"))
	if err == nil {
		t.Fatal("gitSubdirPluginDirs() error = nil, want error for a missing file")
	}
	if !strings.Contains(err.Error(), "reading") {
		t.Errorf("error = %q, want it to mention reading the file", err)
	}
}

// TestRealMarketplaceManifest guards the actual file the release script reads:
// if it stops listing git-subdir plugins, or gains a malformed entry, the
// plugin-bump step would silently skip every plugin.
func TestRealMarketplaceManifest(t *testing.T) {
	got, err := gitSubdirPluginDirs("../../.claude-plugin/marketplace.json")
	if err != nil {
		t.Fatalf("gitSubdirPluginDirs() on the real manifest: %v", err)
	}
	for _, dir := range got {
		manifest := filepath.Join("../..", dir, ".claude-plugin", "plugin.json")
		if _, err := os.Stat(manifest); err != nil {
			t.Errorf("plugin %q is listed in marketplace.json but %s is missing: %v", dir, manifest, err)
		}
	}
}

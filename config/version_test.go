// Copyright (c) 2026 Fabrik Contributors. All rights reserved.

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInferVersion(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string // filename → content
		want  string
	}{
		{
			name: "package.json version",
			files: map[string]string{
				"package.json": `{"name": "myapp", "version": "1.2.3"}`,
			},
			want: "1.2.3",
		},
		{
			name: "go.mod module name (no version)",
			files: map[string]string{
				"go.mod": "module github.com/verveguy/fabrik\n\ngo 1.21\n",
			},
			want: "github.com/verveguy/fabrik",
		},
		{
			name: "go.mod returns module name not a semver",
			files: map[string]string{
				"go.mod": "module example.com/myproject\n\ngo 1.22\n",
			},
			want: "example.com/myproject",
		},
		{
			name: "Cargo.toml version",
			files: map[string]string{
				"Cargo.toml": "[package]\nname = \"myapp\"\nversion = \"0.5.0\"\n",
			},
			want: "0.5.0",
		},
		{
			name: "pyproject.toml version",
			files: map[string]string{
				"pyproject.toml": "[build-system]\nrequires = [\"setuptools\"]\n\n[project]\nname = \"myapp\"\nversion = \"2.0.0\"\n",
			},
			want: "2.0.0",
		},
		{
			name:  "no metadata files — returns empty",
			files: map[string]string{},
			want:  "",
		},
		{
			name: "malformed package.json — returns empty",
			files: map[string]string{
				"package.json": `not json`,
			},
			want: "",
		},
		{
			name: "package.json without version field — falls through to go.mod",
			files: map[string]string{
				"package.json": `{"name": "myapp"}`,
				"go.mod":       "module example.com/fallback\n",
			},
			want: "example.com/fallback",
		},
		{
			name: "Cargo.toml version with single quotes",
			files: map[string]string{
				"Cargo.toml": "[package]\nversion = '3.1.4'\n",
			},
			want: "3.1.4",
		},
		{
			name: "pyproject.toml version not under [project] section is ignored",
			files: map[string]string{
				"pyproject.toml": "[tool.poetry]\nversion = \"9.9.9\"\n\n[project]\nname = \"myapp\"\nversion = \"1.0.0\"\n",
			},
			want: "1.0.0",
		},
		{
			name: "package.json priority over go.mod",
			files: map[string]string{
				"package.json": `{"version": "4.0.0"}`,
				"go.mod":       "module example.com/foo\n",
			},
			want: "4.0.0",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
					t.Fatalf("writing %s: %v", name, err)
				}
			}
			got := InferVersion(dir)
			if got != tc.want {
				t.Errorf("InferVersion() = %q, want %q", got, tc.want)
			}
		})
	}
}

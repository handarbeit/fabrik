// Package reviewplugin embeds Pruefer's base review plugin, pruefer-review, and
// writes it to a directory (#2306, ADR 2306). The plugin has one skill,
// review, whose instructions are Pruefer's default review guidance. It lives
// under pruefer/ rather than plugin/ because it is private to Pruefer: the
// shipped-files table, the PM plugin and `fabrik upgrade` do not apply to it.
package reviewplugin

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/handarbeit/fabrik/plugin"
)

const (
	// PluginName is the manifest name of the embedded plugin.
	PluginName = "pruefer-review"
	// SkillName is the default review skill inside it.
	SkillName = "review"
)

// DefaultRef is the qualified reference to the default review skill.
var DefaultRef = plugin.Qualify(PluginName, SkillName)

// "all:" is required: the manifest lives in the dot-directory .claude-plugin.
//
//go:embed all:plugin
var tree embed.FS

const root = "plugin"

// Write writes the plugin tree into dir (which must already exist). Every
// review gets a fresh directory, so Write never merges with or prunes prior
// content.
func Write(dir string) error {
	return fs.WalkDir(tree, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, root), "/")
		if rel == "" {
			return nil
		}
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		data, err := tree.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", rel, err)
		}
		return nil
	})
}

// SkillBody returns the review skill's instructions: SKILL.md without its
// frontmatter and trailing newline. Tests use it to pin the default guidance.
func SkillBody() (string, error) {
	data, err := tree.ReadFile(root + "/skills/" + SkillName + "/SKILL.md")
	if err != nil {
		return "", err
	}
	s := string(data)
	if rest, ok := strings.CutPrefix(s, "---\n"); ok {
		if _, body, found := strings.Cut(rest, "\n---\n"); found {
			s = strings.TrimPrefix(body, "\n")
		}
	}
	return strings.TrimSuffix(s, "\n"), nil
}

// Version returns the manifest version, for the review log line.
func Version() string {
	data, err := tree.ReadFile(root + "/.claude-plugin/plugin.json")
	if err != nil {
		return "unknown"
	}
	const key = `"version"`
	s := string(data)
	i := strings.Index(s, key)
	if i < 0 {
		return "unknown"
	}
	rest := s[i+len(key):]
	a := strings.Index(rest, `"`)
	if a < 0 {
		return "unknown"
	}
	b := strings.Index(rest[a+1:], `"`)
	if b < 0 {
		return "unknown"
	}
	return rest[a+1 : a+1+b]
}

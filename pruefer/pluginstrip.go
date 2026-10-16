package pruefer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/handarbeit/fabrik/internal/skillref"
)

// Plugins without hooks or MCP servers (#2308, R3, ADR 2308 decision 5).
//
// Claude Code starts a --plugin-dir plugin's hooks and MCP servers whatever
// the permission rules say, and has no switch that turns them off for one
// plugin. For an author without write access Pruefer therefore loads each
// operator plugin from a per-review COPY built from an allowlist: skills,
// agents and commands, with the manifest cut down to its descriptive keys and
// the hooks and MCP keys removed from every markdown frontmatter. Anything the
// allowlist does not name — hooks/, .mcp.json, scripts, symlinks, unknown
// manifest keys — is simply not copied, so a plugin layout this code has never
// seen fails closed instead of open. The embedded pruefer-review plugin declares
// no hooks or MCP servers and is never copied.

// pluginManifestKeepKeys are the manifest keys the copy keeps. Everything else
// (notably hooks and mcpServers) is dropped.
var pluginManifestKeepKeys = map[string]bool{
	"name": true, "version": true, "description": true, "author": true,
	"license": true, "homepage": true, "repository": true, "keywords": true,
	"skills": true, "agents": true, "commands": true,
}

// pluginContentDirs are the default directories whose content is copied.
var pluginContentDirs = []string{"skills", "agents", "commands"}

// hooksOrMCPKey reports whether a manifest or frontmatter key declares hooks or
// MCP servers.
func hooksOrMCPKey(k string) bool {
	switch strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(k)) {
	case "hooks", "mcpservers", "mcp":
		return true
	}
	return false
}

// strippedPlugin is the report of one copy.
type strippedPlugin struct {
	// Removed lists the hooks and MCP servers the copy dropped (empty when the
	// plugin declared none), for the log and the posted notice.
	Removed []string
	// Omitted lists other content the allowlist left out, for the log.
	Omitted []string
}

// stripPluginHooks copies the plugin at src into dst (which must not exist),
// following the allowlist above. src is a resolved directory.
func stripPluginHooks(src, dst string) (strippedPlugin, error) {
	var rep strippedPlugin
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return rep, err
	}
	copyRels := append([]string(nil), pluginContentDirs...)

	// The manifest: keep the descriptive keys, report the hooks/MCP keys.
	manifestSrc := filepath.Join(src, skillref.ManifestRel)
	if data, err := os.ReadFile(manifestSrc); err == nil {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(data, &m); err != nil {
			return rep, fmt.Errorf("manifest is not valid JSON: %w", err)
		}
		kept := map[string]json.RawMessage{}
		for k, v := range m {
			switch {
			case hooksOrMCPKey(k):
				if string(bytes.TrimSpace(v)) != "null" {
					rep.Removed = append(rep.Removed, "manifest key "+k)
				}
			case pluginManifestKeepKeys[k]:
				kept[k] = v
			default:
				rep.Omitted = append(rep.Omitted, "manifest key "+k)
			}
		}
		for _, k := range []string{"skills", "agents", "commands"} {
			copyRels = append(copyRels, manifestPaths(kept[k])...)
		}
		out, err := json.MarshalIndent(kept, "", "  ")
		if err != nil {
			return rep, err
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dst, skillref.ManifestRel)), 0o755); err != nil {
			return rep, err
		}
		if err := os.WriteFile(filepath.Join(dst, skillref.ManifestRel), out, 0o644); err != nil {
			return rep, err
		}
	}

	if _, err := os.Lstat(filepath.Join(src, "hooks")); err == nil {
		rep.Removed = append(rep.Removed, "hooks/")
	}
	if _, err := os.Lstat(filepath.Join(src, ".mcp.json")); err == nil {
		rep.Removed = append(rep.Removed, ".mcp.json")
	}

	// A root single-skill plugin.
	if err := copyPluginFile(src, dst, "SKILL.md", &rep); err != nil {
		return rep, err
	}
	seen := map[string]bool{}
	for _, rel := range copyRels {
		rel = cleanPluginRel(rel)
		if rel == "" || seen[rel] {
			continue
		}
		seen[rel] = true
		if err := copyPluginTree(src, dst, rel, &rep); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// manifestPaths decodes a manifest skills/agents/commands value: one path or a
// list of paths. Anything else yields none.
func manifestPaths(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		return many
	}
	return nil
}

// cleanPluginRel returns a slash-separated relative path that stays inside the
// plugin root and names neither the root itself nor an excluded top-level
// entry, or "" if rel is not one.
func cleanPluginRel(rel string) string {
	rel = filepath.ToSlash(filepath.Clean(strings.TrimPrefix(rel, "./")))
	if rel == "." || rel == "" || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../") {
		return ""
	}
	switch strings.SplitN(rel, "/", 2)[0] {
	case "hooks", ".claude-plugin", ".mcp.json":
		return ""
	}
	return rel
}

func copyPluginTree(src, dst, rel string, rep *strippedPlugin) error {
	root := filepath.Join(src, filepath.FromSlash(rel))
	fi, err := os.Lstat(root)
	if err != nil {
		return nil // an absent default directory or dangling manifest path
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		rep.Omitted = append(rep.Omitted, "symlink "+rel)
		return nil
	}
	if !fi.IsDir() {
		return copyPluginFile(src, dst, rel, rep)
	}
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		sub, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		sub = filepath.ToSlash(sub)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			rep.Omitted = append(rep.Omitted, "symlink "+sub)
			return nil
		case d.IsDir():
			return os.MkdirAll(filepath.Join(dst, filepath.FromSlash(sub)), 0o755)
		case !d.Type().IsRegular():
			rep.Omitted = append(rep.Omitted, "special file "+sub)
			return nil
		}
		return copyPluginFile(src, dst, sub, rep)
	})
}

// copyPluginFile copies one regular file (a missing one is not an error). The
// copy is never executable. A markdown file has its hooks and MCP frontmatter
// keys removed; one whose frontmatter cannot be parsed is not copied at all.
func copyPluginFile(src, dst, rel string, rep *strippedPlugin) error {
	from := filepath.Join(src, filepath.FromSlash(rel))
	fi, err := os.Lstat(from)
	if err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	base := filepath.Base(rel)
	if base == ".mcp.json" || base == "hooks.json" {
		rep.Omitted = append(rep.Omitted, rel)
		return nil
	}
	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	if strings.EqualFold(filepath.Ext(rel), ".md") {
		out, removed, ok := stripFrontmatter(data)
		if !ok {
			rep.Omitted = append(rep.Omitted, rel+" (unparseable frontmatter)")
			return nil
		}
		for _, k := range removed {
			rep.Removed = append(rep.Removed, fmt.Sprintf("frontmatter key %s in %s", k, rel))
		}
		data = out
	}
	to := filepath.Join(dst, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	return os.WriteFile(to, data, 0o644)
}

// stripFrontmatter removes the hooks and MCP keys from a markdown file's YAML
// frontmatter. ok is false when the file opens a frontmatter block that cannot
// be parsed as a YAML mapping or is not closed: the caller then drops the file.
// A file with no frontmatter is returned unchanged.
func stripFrontmatter(data []byte) (out []byte, removed []string, ok bool) {
	text := string(data)
	if !strings.HasPrefix(text, "---\n") && !strings.HasPrefix(text, "---\r\n") {
		if strings.HasPrefix(strings.TrimLeft(text, "\ufeff"), "---") {
			return nil, nil, false // BOM or odd opener: do not guess
		}
		return data, nil, true
	}
	rest := text[strings.Index(text, "\n")+1:]
	// The block ends at the first line that is exactly "---" (trailing blanks
	// and CR tolerated); an unclosed block is not guessed at.
	var fm, body string
	closed := false
	for off := 0; off < len(rest); {
		end := strings.Index(rest[off:], "\n")
		line, next := rest[off:], len(rest)
		if end >= 0 {
			line, next = rest[off:off+end], off+end+1
		}
		if strings.TrimRight(line, " \t\r") == "---" {
			fm, body, closed = rest[:off], rest[next:], true
			break
		}
		off = next
	}
	if !closed {
		return nil, nil, false
	}
	if strings.TrimSpace(fm) == "" {
		return data, nil, true
	}
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(fm), &node); err != nil || node.Kind != yaml.DocumentNode || len(node.Content) != 1 {
		return nil, nil, false
	}
	m := node.Content[0]
	if m.Kind != yaml.MappingNode {
		return nil, nil, false
	}
	kept := m.Content[:0:0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		k, v := m.Content[i], m.Content[i+1]
		if k.Kind == yaml.ScalarNode && hooksOrMCPKey(k.Value) {
			removed = append(removed, k.Value)
			continue
		}
		kept = append(kept, k, v)
	}
	if len(removed) == 0 {
		return data, nil, true
	}
	m.Content = kept
	enc, err := yaml.Marshal(&node)
	if err != nil {
		return nil, nil, false
	}
	var b bytes.Buffer
	b.WriteString("---\n")
	if len(kept) > 0 {
		b.Write(enc)
	}
	b.WriteString("---\n")
	b.WriteString(body)
	return b.Bytes(), removed, true
}

// stripOperatorPlugins builds the read-only copies of dirs under root (a
// directory the caller removes at the end of the review) and returns the
// directories to pass with --plugin-dir plus the manifest names of the plugins
// that actually lost hooks or MCP servers. A plugin that cannot be copied is
// left out and logged — failing closed — rather than loaded unstripped.
func stripOperatorPlugins(root string, dirs []string, prNumber int) (out, stripped []string) {
	for i, src := range dirs {
		// The copy keeps the original base name: a plugin without a manifest
		// is named after its directory.
		dst := filepath.Join(root, fmt.Sprintf("operator-readonly-%d", i), filepath.Base(src))
		rep, err := stripPluginHooks(src, dst)
		if err != nil {
			logf(prNumber, "warn", "operator review plugin %q skipped for a read-only review: %v\n", filepath.Base(src), err)
			_ = os.RemoveAll(filepath.Dir(dst))
			continue
		}
		if len(rep.Omitted) > 0 {
			logf(prNumber, "claude", "read-only copy of review plugin %q omitted: %s\n", filepath.Base(src), strings.Join(rep.Omitted, "; "))
		}
		if len(rep.Removed) > 0 {
			name := skillref.Discover(dst).Name
			logf(prNumber, "claude", "read-only copy of review plugin %q removed hooks/MCP: %s\n", name, strings.Join(rep.Removed, "; "))
			stripped = append(stripped, name)
		}
		out = append(out, dst)
	}
	return out, stripped
}

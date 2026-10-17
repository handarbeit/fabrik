package pruefer

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Tool pre-approvals in plugin frontmatter (#2308, ADR 2308 decision 13).
//
// A skill, agent or command can pre-approve tools while it is active with an
// allowed-tools frontmatter key. An operator plugin that declares
// "allowed-tools: Bash(go test:*)" would therefore run repo code on any
// author's PR, past the operator ceiling and the test-runner refusal. For an
// outside author the read-only copy removes the key (stripFrontmatter). For a
// trusted author the operator plugin is checked here: every entry must already
// be in the effective grant (the base rules or the validated review_tools
// extras), otherwise the plugin is loaded from a copy without the key and the
// dropped entries are logged. The grant is never widened by a plugin.

// splitToolEntries splits an allowed-tools string into its rules. Entries are
// separated by commas or whitespace outside parentheses, so "Bash(git log:*),
// Read" and "Bash(git log:*) Read" both give two entries.
func splitToolEntries(s string) []string {
	var out []string
	var cur strings.Builder
	depth := 0
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, normalizeToolRule(cur.String()))
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case r == '(':
			depth++
			cur.WriteRune(r)
		case r == ')':
			if depth > 0 {
				depth--
			}
			cur.WriteRune(r)
		case depth == 0 && (r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'):
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// allowedToolEntries reads the value of an allowed-tools key: a string or a
// list of strings. ok is false for any other shape, which the caller treats as
// unverifiable.
func allowedToolEntries(v *yaml.Node) (entries []string, ok bool) {
	switch v.Kind {
	case yaml.ScalarNode:
		return splitToolEntries(v.Value), true
	case yaml.SequenceNode:
		for _, item := range v.Content {
			if item.Kind != yaml.ScalarNode {
				return nil, false
			}
			entries = append(entries, splitToolEntries(item.Value)...)
		}
		return entries, true
	}
	return nil, false
}

// dropUnapprovedPreapprovals returns a rewriteFrontmatter predicate that drops
// an allowed-tools key unless every one of its entries is in grant, and records
// the entries that caused a drop.
func dropUnapprovedPreapprovals(grant []string, dropped *[]string) func(string, *yaml.Node) bool {
	return func(k string, v *yaml.Node) bool {
		if !allowedToolsKey(k) {
			return false
		}
		entries, ok := allowedToolEntries(v)
		if !ok {
			*dropped = append(*dropped, "(unreadable allowed-tools value)")
			return true
		}
		var bad []string
		for _, e := range entries {
			if !slices.Contains(grant, e) {
				bad = append(bad, e)
			}
		}
		if len(bad) == 0 {
			return false
		}
		*dropped = append(*dropped, bad...)
		return true
	}
}

// rewriteMarkdownPreapprovals returns data with any allowed-tools key that
// names a tool outside grant removed, the entries it dropped, and whether the
// file is usable. A file whose frontmatter this code cannot parse is unusable
// only when it mentions an allowed-tools key at all: the CLI's parser may read
// what ours cannot, so it is left out of the copy rather than guessed at.
func rewriteMarkdownPreapprovals(data []byte, grant []string) (out []byte, dropped []string, ok bool) {
	out, _, ok = rewriteFrontmatter(data, dropUnapprovedPreapprovals(grant, &dropped))
	if ok {
		return out, dropped, true
	}
	low := strings.ToLower(string(data))
	if strings.Contains(low, "allowed-tools") || strings.Contains(low, "allowed_tools") || strings.Contains(low, "allowedtools") {
		return nil, []string{"(frontmatter that could not be parsed)"}, false
	}
	return data, nil, true
}

// sanitizeOperatorPreapprovals returns dirs with each operator plugin that
// pre-approves a tool outside grant replaced by a copy under root without the
// offending allowed-tools keys. A plugin that pre-approves nothing outside the
// grant is returned as it is (used in place). A plugin that cannot be copied is
// left out and its directory name returned in skipped, failing closed.
func sanitizeOperatorPreapprovals(root string, dirs []string, grant []string, prNumber int) (out, skipped []string) {
	for i, src := range dirs {
		name := filepath.Base(src)
		dropped, err := scanPreapprovals(src, grant)
		if err != nil {
			logf(prNumber, "warn", "operator review plugin %q skipped: checking its allowed-tools: %v\n", name, err)
			skipped = append(skipped, name)
			continue
		}
		if len(dropped) == 0 {
			out = append(out, src)
			continue
		}
		dst := filepath.Join(root, fmt.Sprintf("operator-preapproved-%d", i), name)
		if err := copyWithoutPreapprovals(src, dst, grant); err != nil {
			logf(prNumber, "warn", "operator review plugin %q skipped: copying it without out-of-grant allowed-tools: %v\n", name, err)
			_ = os.RemoveAll(filepath.Dir(dst))
			skipped = append(skipped, name)
			continue
		}
		logf(prNumber, "warn", "operator review plugin %q: allowed-tools outside the review grant dropped: %s\n", name, strings.Join(dropped, "; "))
		out = append(out, dst)
	}
	return out, skipped
}

// scanPreapprovals lists, as "<file>: <entry>", the allowed-tools entries under
// src that are outside grant. It reads markdown files only and never follows
// symlinks.
func scanPreapprovals(src string, grant []string) ([]string, error) {
	var dropped []string
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() || !strings.EqualFold(filepath.Ext(p), ".md") {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		_, bad, _ := rewriteMarkdownPreapprovals(data, grant)
		for _, e := range bad {
			dropped = append(dropped, filepath.ToSlash(rel)+": "+sanitizeNoticeText(e))
		}
		return nil
	})
	return dropped, err
}

// copyWithoutPreapprovals copies the whole plugin tree (hooks, scripts and all:
// a trusted author's review loads the operator's plugin as it is) with the
// out-of-grant allowed-tools keys removed from every markdown file. Symlinks are
// recreated as symlinks and file modes kept, so executables stay executable. A
// markdown file whose frontmatter cannot be read is left out.
func copyWithoutPreapprovals(src, dst string, grant []string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		to := filepath.Join(dst, rel)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
				return err
			}
			return os.Symlink(target, to)
		case d.IsDir():
			return os.MkdirAll(to, 0o755)
		case !d.Type().IsRegular():
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.EqualFold(filepath.Ext(p), ".md") {
			var ok bool
			if data, _, ok = rewriteMarkdownPreapprovals(data, grant); !ok {
				return nil
			}
		}
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			return err
		}
		return os.WriteFile(to, data, info.Mode().Perm())
	})
}

// gen-shipped-files maintains plugin/shipped_files.json: for every file the
// embedded workflow plugin has ever shipped, the set of sha256 sums seen in any
// release (#2139, ADR 2139). `fabrik upgrade` uses it to tell a file an older
// release shipped from one an operator edited — the aggregate fingerprints in
// plugin.KnownEmbeddedVersions cannot answer that per-file question.
//
// Usage (from the repository root):
//
//	go run ./tools/gen-shipped-files             regenerate from every v* tag, plus the current embed
//	go run ./tools/gen-shipped-files -add-current merge the current embed's sums into the table (release step)
//	go run ./tools/gen-shipped-files -check       exit non-zero when a tag's file or the current embed is missing from the table
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"

	fabrikplugin "github.com/handarbeit/fabrik/plugin"
)

const tableFile = "plugin/shipped_files.json"

// tagPluginDirs are the directories the plugin lived in over the releases.
var tagPluginDirs = []string{"plugin/fabrik-workflows", "plugin/fabrik-plugin"}

func main() {
	addCurrent := flag.Bool("add-current", false, "merge the current embed into the existing table")
	check := flag.Bool("check", false, "verify every tag and the current embed are covered by the table")
	flag.Parse()
	if err := run(*addCurrent, *check); err != nil {
		fmt.Fprintln(os.Stderr, "gen-shipped-files:", err)
		os.Exit(1)
	}
}

func run(addCurrent, check bool) error {
	switch {
	case check:
		return runCheck()
	case addCurrent:
		table, err := readTable()
		if err != nil {
			return err
		}
		addEmbed(table)
		return writeTable(table)
	default:
		table := map[string]map[string]bool{}
		tags, err := listTags()
		if err != nil {
			return err
		}
		for _, tag := range tags {
			files, err := tagFiles(tag)
			if err != nil {
				return fmt.Errorf("tag %s: %w", tag, err)
			}
			for rel, sum := range files {
				add(table, rel, sum)
			}
		}
		addEmbed(table)
		return writeTable(table)
	}
}

func runCheck() error {
	table, err := readTable()
	if err != nil {
		return err
	}
	var missing []string
	tags, err := listTags()
	if err != nil {
		return err
	}
	for _, tag := range tags {
		files, err := tagFiles(tag)
		if err != nil {
			return fmt.Errorf("tag %s: %w", tag, err)
		}
		for rel, sum := range files {
			if !table[rel][sum] {
				missing = append(missing, fmt.Sprintf("%s: %s (%s)", tag, rel, sum[:12]))
			}
		}
	}
	cur := map[string]map[string]bool{}
	addEmbed(cur)
	for rel, sums := range cur {
		for sum := range sums {
			if !table[rel][sum] {
				missing = append(missing, fmt.Sprintf("current embed: %s (%s)", rel, sum[:12]))
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("%s is missing %d shipped file version(s); run `go run ./tools/gen-shipped-files -add-current`:\n  %s",
			tableFile, len(missing), strings.Join(missing, "\n  "))
	}
	fmt.Println("shipped-files table covers every tag and the current embed")
	return nil
}

func listTags() ([]string, error) {
	out, err := exec.Command("git", "tag", "--list", "v*").Output()
	if err != nil {
		return nil, fmt.Errorf("listing tags: %w", err)
	}
	return strings.Fields(string(out)), nil
}

// shippable reports whether rel is a file `fabrik init` extracts from the embed.
func shippable(rel string) bool {
	switch rel {
	case ".claude-plugin/plugin.json", "README.md", "LABELS.md":
		return true
	}
	parts := strings.Split(rel, "/")
	return len(parts) == 3 && parts[0] == "skills" && parts[2] == "SKILL.md"
}

// tagFiles returns relative path -> sha256 for the shippable plugin files at tag.
func tagFiles(tag string) (map[string]string, error) {
	for _, dir := range tagPluginDirs {
		out, err := exec.Command("git", "ls-tree", "-r", "--name-only", tag, dir+"/").Output()
		if err != nil || len(strings.TrimSpace(string(out))) == 0 {
			continue
		}
		files := map[string]string{}
		for _, full := range strings.Fields(string(out)) {
			rel := strings.TrimPrefix(full, dir+"/")
			if !shippable(rel) {
				continue
			}
			data, err := exec.Command("git", "show", tag+":"+full).Output()
			if err != nil {
				return nil, fmt.Errorf("reading %s: %w", full, err)
			}
			files[rel] = sum(data)
		}
		return files, nil
	}
	return map[string]string{}, nil // tag predates the plugin
}

func addEmbed(table map[string]map[string]bool) {
	_ = fs.WalkDir(fabrikplugin.FabrikPlugin, "fabrik-workflows", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, rerr := fabrikplugin.FabrikPlugin.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		add(table, strings.TrimPrefix(path.Clean(p), "fabrik-workflows/"), sum(data))
		return nil
	})
}

func add(table map[string]map[string]bool, rel, s string) {
	if table[rel] == nil {
		table[rel] = map[string]bool{}
	}
	table[rel][s] = true
}

func sum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func readTable() (map[string]map[string]bool, error) {
	data, err := os.ReadFile(tableFile)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", tableFile, err)
	}
	var raw map[string][]string
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", tableFile, err)
	}
	table := map[string]map[string]bool{}
	for rel, sums := range raw {
		for _, s := range sums {
			add(table, rel, s)
		}
	}
	return table, nil
}

func writeTable(table map[string]map[string]bool) error {
	raw := map[string][]string{}
	for rel, sums := range table {
		list := make([]string, 0, len(sums))
		for s := range sums {
			list = append(list, s)
		}
		sort.Strings(list)
		raw[rel] = list
	}
	data, err := json.MarshalIndent(raw, "", "  ") // map keys are marshalled sorted
	if err != nil {
		return err
	}
	return os.WriteFile(tableFile, append(data, '\n'), 0644)
}

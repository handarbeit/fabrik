// list-marketplace-plugins prints the repo-relative directory of every plugin
// distributed from this repository via .claude-plugin/marketplace.json, one
// per line.
//
// Only entries whose source.source is "git-subdir" are listed: those carry a
// source.path pointing at a directory in this repo and are therefore ours to
// version-bump at release time. Any other source type is distributed from
// somewhere else and must be left alone.
//
// This exists so scripts/cut-release.sh can derive its plugin-bump list from
// marketplace.json instead of hardcoding it — marketplace.json membership is
// exactly what makes /plugin update's version comparison apply to a plugin, so
// deriving the list keeps the two from drifting apart. It is a Go program
// rather than a jq one-liner to avoid adding a jq dependency to the release
// path; the release script already requires the Go toolchain.
//
// Usage: go run ./tools/list-marketplace-plugins/ [marketplace-json-path]
//
// Defaults to .claude-plugin/marketplace.json relative to the working
// directory. Exits non-zero if the file is missing, malformed, or lists no
// git-subdir plugins at all — each of which means the release script's
// assumptions no longer hold and a silent empty list would skip every bump.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const defaultManifest = ".claude-plugin/marketplace.json"

type marketplace struct {
	Plugins []struct {
		Name   string `json:"name"`
		Source struct {
			Source string `json:"source"`
			Path   string `json:"path"`
		} `json:"source"`
	} `json:"plugins"`
}

func main() {
	path := defaultManifest
	if len(os.Args) > 2 {
		fmt.Fprintf(os.Stderr, "usage: %s [marketplace-json-path]\n", os.Args[0])
		os.Exit(2)
	}
	if len(os.Args) == 2 {
		path = os.Args[1]
	}

	dirs, err := gitSubdirPluginDirs(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "list-marketplace-plugins: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(strings.Join(dirs, "\n"))
}

// gitSubdirPluginDirs returns the source.path of every git-subdir plugin
// listed in the marketplace manifest at path, in file order.
func gitSubdirPluginDirs(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	var m marketplace
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	var dirs []string
	for _, p := range m.Plugins {
		if p.Source.Source != "git-subdir" {
			continue
		}
		if p.Source.Path == "" {
			return nil, fmt.Errorf("%s: plugin %q is git-subdir but has no source.path", path, p.Name)
		}
		// The release script consumes this output with shell word-splitting
		// (for dir in $PLUGIN_DIRS). A path containing whitespace would split
		// into two nonexistent paths and fail with a confusing missing-file
		// error several steps later. Reject it here, where the message can say
		// what is actually wrong.
		if strings.ContainsAny(p.Source.Path, " \t\n") {
			return nil, fmt.Errorf("%s: plugin %q has whitespace in source.path (%q); "+
				"the release script splits this list on whitespace", path, p.Name, p.Source.Path)
		}
		dirs = append(dirs, p.Source.Path)
	}
	if len(dirs) == 0 {
		return nil, fmt.Errorf("%s: no git-subdir plugins listed", path)
	}
	return dirs, nil
}

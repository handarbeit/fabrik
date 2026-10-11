package plugin

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
)

// shippedFilesJSON is plugin/shipped_files.json: for every relative plugin file
// path, the sorted sha256 sums that any release has shipped (#2139, ADR 2139).
// KnownEmbeddedVersions holds only aggregate fingerprints, which cannot say
// whether one file differs from every shipped version; this table can.
//
// It is generated from the release tags by tools/gen-shipped-files and extended
// at each release by scripts/cut-release.sh (-add-current). The current embed is
// always treated as shipped at runtime (see IsShippedFile), so a table that is
// one release behind never makes a pristine install look customised.
//
//go:embed shipped_files.json
var shippedFilesJSON []byte

var (
	shippedOnce  sync.Once
	shippedTable map[string]map[string]bool
	embedOnce    sync.Once
	embedTable   map[string]map[string]bool
)

func loadShippedTable() map[string]map[string]bool {
	shippedOnce.Do(func() {
		shippedTable = map[string]map[string]bool{}
		var raw map[string][]string
		if err := json.Unmarshal(shippedFilesJSON, &raw); err != nil {
			return // a corrupt table degrades to "only the current embed is shipped"
		}
		for rel, sums := range raw {
			m := make(map[string]bool, len(sums))
			for _, s := range sums {
				m[s] = true
			}
			shippedTable[rel] = m
		}
	})
	return shippedTable
}

func loadEmbedTable() map[string]map[string]bool {
	embedOnce.Do(func() {
		embedTable = map[string]map[string]bool{}
		_ = fs.WalkDir(FabrikPlugin, "fabrik-workflows", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			data, rerr := FabrikPlugin.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			sum := sha256.Sum256(data)
			rel := strings.TrimPrefix(path.Clean(p), "fabrik-workflows/")
			embedTable[rel] = map[string]bool{hex.EncodeToString(sum[:]): true}
			return nil
		})
	})
	return embedTable
}

// IsShippedFile reports whether the file at rel (slash-separated, relative to
// the plugin root) with the given sha256 hex sum is byte-identical to a version
// some release shipped, or to the current embed.
func IsShippedFile(rel, sum string) bool {
	return loadShippedTable()[rel][sum] || loadEmbedTable()[rel][sum]
}

// ShippedSkillNames returns the names of the skills the current embed ships,
// sorted.
func ShippedSkillNames() []string {
	var names []string
	entries, err := fs.ReadDir(FabrikPlugin, "fabrik-workflows/skills")
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

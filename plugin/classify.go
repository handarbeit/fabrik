package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Classification is the result of comparing every file of an on-disk plugin
// tree with the files releases have shipped (#2139).
type Classification struct {
	// Files lists every file in the tree (slash-separated, relative to the
	// plugin root, .installed-version excluded), sorted.
	Files []string
	// Unknown lists the files whose content no release shipped — operator
	// edits, plus files the embed does not know at all — sorted.
	Unknown []string
	// Skills lists every skill directory in the tree, sorted.
	Skills []string
	// EditedSkills lists the skills with at least one unknown file under
	// skills/<name>/, sorted. A skill present only in the tree is edited.
	EditedSkills []string
}

// Customised reports whether any file in the tree is unknown to every shipped
// version.
func (c Classification) Customised() bool { return len(c.Unknown) > 0 }

// IsEdited reports whether skill is among EditedSkills.
func (c Classification) IsEdited(skill string) bool { return containsString(c.EditedSkills, skill) }

// HasSkill reports whether skill exists in the tree.
func (c Classification) HasSkill(skill string) bool { return containsString(c.Skills, skill) }

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ClassifyDir walks dir and classifies each file against IsShippedFile. A
// missing directory classifies as empty.
func ClassifyDir(dir string) (Classification, error) {
	sums, err := hashTree(dir)
	if err != nil {
		return Classification{}, err
	}
	return ClassifySums(sums), nil
}

// ClassifySums classifies a map of relative path -> sha256 hex sum. It is pure,
// so the rules can be tested without a disk.
func ClassifySums(sums map[string]string) Classification {
	var c Classification
	skills := map[string]bool{}
	edited := map[string]bool{}
	for rel, sum := range sums {
		c.Files = append(c.Files, rel)
		skill, inSkill := skillOf(rel)
		if inSkill {
			skills[skill] = true
		}
		if !IsShippedFile(rel, sum) {
			c.Unknown = append(c.Unknown, rel)
			if inSkill {
				edited[skill] = true
			}
		}
	}
	sort.Strings(c.Files)
	sort.Strings(c.Unknown)
	c.Skills = sortedKeys(skills)
	c.EditedSkills = sortedKeys(edited)
	return c
}

// skillOf returns the skill a path under skills/<name>/ belongs to.
func skillOf(rel string) (string, bool) {
	parts := strings.Split(rel, "/")
	if len(parts) >= 3 && parts[0] == "skills" {
		return parts[1], true
	}
	return "", false
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// hashTree returns relative path -> sha256 hex for every regular file under
// dir, skipping .installed-version. A missing dir yields an empty map.
func hashTree(dir string) (map[string]string, error) {
	sums := map[string]string{}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return sums, nil
	}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(dir, p)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if rel == installedVersionFile {
			return nil
		}
		data, readErr := os.ReadFile(p)
		if readErr != nil {
			return fmt.Errorf("reading %s: %w", p, readErr)
		}
		sum := sha256.Sum256(data)
		sums[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		return nil, err
	}
	return sums, nil
}

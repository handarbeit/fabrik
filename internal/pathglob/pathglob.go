// Package pathglob is the repository's one path-glob matcher, shared by
// Pruefer's excluded_paths and the engine's merge-train overlap_ignore so the
// two can never disagree on what a pattern means (#2047).
//
// Semantics: the pattern and the path are split on "/" and compared segment by
// segment with filepath.Match, so "*" and "?" never cross a "/". A segment
// that is exactly "**" matches zero or more path segments, so "vendor/**"
// matches everything under vendor/ and "**/*.lock" matches a lockfile at any
// depth. Note that "*.lock" matches only a top-level file: it does not match
// "a/b/yarn.lock".
package pathglob

import (
	"path/filepath"
	"strings"
)

// Match reports whether path matches pattern.
func Match(pattern, path string) bool {
	return matchParts(strings.Split(pattern, "/"), strings.Split(path, "/"))
}

// MatchAny reports whether path matches at least one of patterns.
func MatchAny(path string, patterns []string) bool {
	for _, pat := range patterns {
		if Match(pat, path) {
			return true
		}
	}
	return false
}

func matchParts(pat, name []string) bool {
	if len(pat) == 0 {
		return len(name) == 0
	}
	if pat[0] == "**" {
		if matchParts(pat[1:], name) {
			return true
		}
		return len(name) > 0 && matchParts(pat, name[1:])
	}
	if len(name) == 0 {
		return false
	}
	ok, err := filepath.Match(pat[0], name[0])
	if err != nil || !ok {
		return false
	}
	return matchParts(pat[1:], name[1:])
}

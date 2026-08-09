package simgh

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// sortedKeys returns a map's keys in sorted order, so projections built from
// maps are deterministic across reads.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// contains reports whether s appears in list.
func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// removeString returns list with the first occurrence of s removed, and
// whether anything was removed.
func removeString(list []string, s string) ([]string, bool) {
	for i, v := range list {
		if v == s {
			return append(append([]string{}, list[:i]...), list[i+1:]...), true
		}
	}
	return list, false
}

// splitOwnerRepo splits "owner/repo" into its parts.
//
// Both segments are validated as single path-safe names, because repoDir turns
// them straight into a directory under baseDir: without this, "../evil/pwned"
// splits into owner "../evil" and lands the bare repo outside the t.TempDir()
// the package's doc comment promises to keep everything inside. Real GitHub
// owner and repo names cannot contain a separator either, so nothing valid is
// rejected here.
func splitOwnerRepo(ownerRepo string) (owner, repo string, err error) {
	i := strings.LastIndex(ownerRepo, "/")
	if i <= 0 || i == len(ownerRepo)-1 {
		return "", "", fmt.Errorf("simgh: invalid owner/repo %q", ownerRepo)
	}
	owner, repo = ownerRepo[:i], ownerRepo[i+1:]
	if err := validatePathSegment("owner", owner, ownerRepo); err != nil {
		return "", "", err
	}
	if err := validatePathSegment("repo", repo, ownerRepo); err != nil {
		return "", "", err
	}
	return owner, repo, nil
}

// validatePathSegment rejects a name that would escape or restructure the
// baseDir sandbox once joined into a directory path.
func validatePathSegment(kind, seg, ownerRepo string) error {
	if seg == "." || seg == ".." {
		return fmt.Errorf("simgh: invalid owner/repo %q: %s %q is a path-traversal segment", ownerRepo, kind, seg)
	}
	if strings.ContainsAny(seg, `/\`) || strings.ContainsRune(seg, os.PathSeparator) {
		return fmt.Errorf("simgh: invalid owner/repo %q: %s %q contains a path separator", ownerRepo, kind, seg)
	}
	return nil
}

// cloneStrings returns a defensive copy, so a caller mutating a returned slice
// cannot reach back into model state.
func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

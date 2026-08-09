package simgh

import (
	"fmt"
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
func splitOwnerRepo(ownerRepo string) (owner, repo string, err error) {
	i := strings.LastIndex(ownerRepo, "/")
	if i <= 0 || i == len(ownerRepo)-1 {
		return "", "", fmt.Errorf("simgh: invalid owner/repo %q", ownerRepo)
	}
	return ownerRepo[:i], ownerRepo[i+1:], nil
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

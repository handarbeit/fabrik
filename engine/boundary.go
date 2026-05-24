package engine

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// applyWorktreeBoundary replaces bare "Edit" and "Write" entries in tools with
// path-scoped variants ("Edit(<workDir>/**)" and "Write(<workDir>/**)").
// All other entries are left unchanged. Returns a new slice (does not mutate input).
// When workDir is empty, returns a copy of tools unchanged.
func applyWorktreeBoundary(tools []string, workDir string) []string {
	if workDir == "" {
		result := make([]string, len(tools))
		copy(result, tools)
		return result
	}
	result := make([]string, 0, len(tools))
	for _, t := range tools {
		switch t {
		case "Edit":
			result = append(result, fmt.Sprintf("Edit(%s/**)", workDir))
		case "Write":
			result = append(result, fmt.Sprintf("Write(%s/**)", workDir))
		default:
			result = append(result, t)
		}
	}
	return result
}

// snapshotRepoRefs runs "git for-each-ref --format=%(refname) %(objectname)" in
// bareDir and returns a map of refname → SHA. Returns an empty map when bareDir is
// empty or the command fails (non-fatal: single-repo projects have no other repos to
// audit and the caller skips the audit when the map is empty).
func snapshotRepoRefs(bareDir string) (map[string]string, error) {
	if bareDir == "" {
		return map[string]string{}, nil
	}
	cmd := exec.Command("git", "for-each-ref", "--format=%(refname) %(objectname)")
	cmd.Dir = bareDir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git for-each-ref in %s: %w", bareDir, err)
	}
	refs := make(map[string]string)
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		parts := strings.SplitN(scanner.Text(), " ", 2)
		if len(parts) == 2 {
			refs[parts[0]] = parts[1]
		}
	}
	return refs, nil
}

// crossRepoViolations compares before and after ref snapshots (maps of repo →
// map[refname]SHA) and returns a sorted slice of human-readable violation strings
// for every ref that is new, changed, or deleted in any repo other than activeRepo.
// Only repos present in both before and after are checked; repos absent from before
// (lazily registered or snapshot-failed) are skipped to avoid false positives.
func crossRepoViolations(before, after map[string]map[string]string, activeRepo string) []string {
	var violations []string
	for repo, afterRefs := range after {
		if repo == activeRepo {
			continue
		}
		beforeRefs, ok := before[repo]
		if !ok {
			continue
		}
		for ref, newSHA := range afterRefs {
			oldSHA, existed := beforeRefs[ref]
			if !existed {
				violations = append(violations, fmt.Sprintf("%s: %s (new ref %s)", repo, ref, newSHA))
			} else if oldSHA != newSHA {
				violations = append(violations, fmt.Sprintf("%s: %s (%s → %s)", repo, ref, oldSHA, newSHA))
			}
		}
		for ref, oldSHA := range beforeRefs {
			if _, existed := afterRefs[ref]; !existed {
				violations = append(violations, fmt.Sprintf("%s: %s (deleted, was %s)", repo, ref, oldSHA))
			}
		}
	}
	sort.Strings(violations)
	return violations
}

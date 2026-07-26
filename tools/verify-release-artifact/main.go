// verify-release-artifact asserts that a built binary's embedded VCS build
// info reports a clean tree at the expected commit. It reads the binary's
// buildinfo directly (via debug/buildinfo, which works cross-arch without
// executing the binary) and fails loudly, naming the offending binary and
// its actual vs. expected values, if the tree was dirty or the revision
// doesn't match the tag's target commit.
//
// Usage: verify-release-artifact <binary-path> <expected-revision>
//
// Invoked once per goreleaser build target via builds[].hooks.post, so that
// a dirty or mismatched binary aborts the release before it is archived,
// checksummed, or published.
package main

import (
	"debug/buildinfo"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintf(os.Stderr, "usage: %s <binary-path> <expected-revision>\n", os.Args[0])
		os.Exit(2)
	}
	path := os.Args[1]
	expectedRevision := os.Args[2]

	if err := verify(path, expectedRevision); err != nil {
		fmt.Fprintf(os.Stderr, "verify-release-artifact: %s: %v\n", path, err)
		os.Exit(1)
	}
	fmt.Printf("verify-release-artifact: %s: clean at %s\n", path, expectedRevision)
}

// verify reads path's embedded VCS build info and asserts it reports a clean
// tree (vcs.modified != "true") at expectedRevision (vcs.revision).
func verify(path, expectedRevision string) error {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading build info: %w", err)
	}

	var revision, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}

	if modified == "true" {
		return fmt.Errorf("built from a dirty working tree (vcs.modified=%s, vcs.revision=%s, expected clean at %s)", modified, revision, expectedRevision)
	}
	if revision != expectedRevision {
		return fmt.Errorf("built from the wrong commit (vcs.revision=%s, expected=%s)", revision, expectedRevision)
	}
	return nil
}

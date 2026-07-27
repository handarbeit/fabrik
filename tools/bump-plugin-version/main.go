// bump-plugin-version detects whether a marketplace-distributed plugin's
// source changed since the previous release tag and, if so, patch-bumps the
// "version" field in its .claude-plugin/plugin.json manifest.
//
// This exists because Claude Code's `/plugin update` compares the cached
// plugin.json version against the version at ref:main in marketplace.json —
// same version number means no refresh fires, even if the plugin's source
// (skills, prompts, etc.) changed. Bumping the patch version on every
// source-changing release forces the cache to refresh.
//
// Usage: go run ./tools/bump-plugin-version/ <plugin-dir> <manifest-path> <prev-tag>
//
// Prints "<old-version> <new-version>" to stdout and exits 0 if a bump
// fired. Prints nothing and exits 0 if plugin-dir has no changes since
// prev-tag (excluding manifest-path itself). Exits non-zero on git or
// manifest-parsing errors.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintf(os.Stderr, "usage: %s <plugin-dir> <manifest-path> <prev-tag>\n", os.Args[0])
		os.Exit(2)
	}
	pluginDir := os.Args[1]
	manifestPath := os.Args[2]
	prevTag := os.Args[3]

	changed, err := sourceChanged(".", pluginDir, manifestPath, prevTag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bump-plugin-version: %v\n", err)
		os.Exit(1)
	}
	if !changed {
		os.Exit(0)
	}

	oldVer, newVer, err := bumpPatchVersion(manifestPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bump-plugin-version: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("%s %s\n", oldVer, newVer)
}

// sourceChanged reports whether any file under pluginDir (excluding
// manifestPath) differs between prevTag and HEAD, in the git repo rooted at
// repoDir.
func sourceChanged(repoDir, pluginDir, manifestPath, prevTag string) (bool, error) {
	if prevTag == "" {
		return false, fmt.Errorf("prevTag must not be empty")
	}
	cmd := exec.Command("git", "-C", repoDir, "diff", "--name-only",
		prevTag+"..HEAD", "--", pluginDir, ":(exclude)"+manifestPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("git diff %s..HEAD -- %s: %w: %s", prevTag, pluginDir, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)) != "", nil
}

var versionFieldRe = regexp.MustCompile(`"version":\s*"([0-9]+)\.([0-9]+)\.([0-9]+)"`)

// bumpPatchVersion patch-bumps the "version" field in the JSON manifest at
// manifestPath (e.g. "0.2.0" -> "0.2.1") via a targeted string replace, so
// the rest of the file's formatting is left untouched. It errors if the
// field is missing, malformed, or appears more than once.
func bumpPatchVersion(manifestPath string) (oldVer, newVer string, err error) {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", "", fmt.Errorf("reading %s: %w", manifestPath, err)
	}
	content := string(data)

	matches := versionFieldRe.FindAllStringSubmatchIndex(content, -1)
	if len(matches) == 0 {
		return "", "", fmt.Errorf("%s: no \"version\": \"MAJOR.MINOR.PATCH\" field found", manifestPath)
	}
	if len(matches) > 1 {
		return "", "", fmt.Errorf("%s: multiple \"version\" fields found, expected exactly one", manifestPath)
	}

	m := matches[0]
	major, minor, patch := content[m[2]:m[3]], content[m[4]:m[5]], content[m[6]:m[7]]
	patchN, err := strconv.Atoi(patch)
	if err != nil {
		return "", "", fmt.Errorf("%s: malformed patch version %q: %w", manifestPath, patch, err)
	}

	oldVer = fmt.Sprintf("%s.%s.%s", major, minor, patch)
	newVer = fmt.Sprintf("%s.%s.%d", major, minor, patchN+1)

	// Splice in the new patch digits at the exact byte range the regex
	// matched, rather than reconstructing the field as a literal with
	// hardcoded spacing — that would desync from versionFieldRe's \s*
	// tolerance if the manifest's actual spacing ever differed.
	updated := content[:m[6]] + strconv.Itoa(patchN+1) + content[m[7]:]

	if err := os.WriteFile(manifestPath, []byte(updated), 0o644); err != nil {
		return "", "", fmt.Errorf("writing %s: %w", manifestPath, err)
	}
	return oldVer, newVer, nil
}

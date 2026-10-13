// pin-plugin-release pins what a release ships to users of the plugin
// marketplace (#2141, ADR 2141): it sets a plugin manifest's version to the
// release version and points every git-subdir entry of
// .claude-plugin/marketplace.json at the release tag.
//
// Usage:
//
//	go run ./tools/pin-plugin-release/ version <manifest-path> <version>
//	go run ./tools/pin-plugin-release/ refs <marketplace-json-path> <tag>
//
// "version" prints "<old> <new>" (the version has no leading "v": it must
// equal the binary's own version, which goreleaser injects without one).
// "refs" prints "<count>" of the entries rewritten. Both edit by targeted
// text replacement so the rest of the file's formatting is untouched, and
// both are idempotent. Like the sibling release tools this is Go rather than
// jq so the release path needs no extra dependency.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
)

func main() {
	if len(os.Args) != 4 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "version":
		var oldVer string
		oldVer, err = setManifestVersion(os.Args[2], os.Args[3])
		if err == nil {
			fmt.Printf("%s %s\n", oldVer, os.Args[3])
		}
	case "refs":
		var n int
		n, err = pinMarketplaceRefs(os.Args[2], os.Args[3])
		if err == nil {
			fmt.Println(n)
		}
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "pin-plugin-release: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "usage: %s version <manifest-path> <version>\n       %s refs <marketplace-json-path> <tag>\n", os.Args[0], os.Args[0])
	os.Exit(2)
}

var (
	versionFieldRe = regexp.MustCompile(`("version":\s*")([^"]*)(")`)
	semverRe       = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]+)?$`)
	refFieldRe     = regexp.MustCompile(`("ref":\s*")([^"]*)(")`)
	tagRe          = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]+)?$`)
)

// setManifestVersion sets the "version" field of the JSON manifest at path to
// version and returns the previous value. It errors if the field is missing or
// appears more than once, or if version is not a semver without a leading "v".
func setManifestVersion(path, version string) (string, error) {
	if !semverRe.MatchString(version) {
		return "", fmt.Errorf("version %q is not MAJOR.MINOR.PATCH (no leading \"v\")", version)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	content := string(data)
	matches := versionFieldRe.FindAllStringSubmatchIndex(content, -1)
	switch {
	case len(matches) == 0:
		return "", fmt.Errorf("%s: no \"version\" field found", path)
	case len(matches) > 1:
		return "", fmt.Errorf("%s: multiple \"version\" fields found, expected exactly one", path)
	}
	m := matches[0]
	oldVer := content[m[4]:m[5]]
	updated := content[:m[4]] + version + content[m[5]:]
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return "", fmt.Errorf("writing %s: %w", path, err)
	}
	return oldVer, nil
}

// pinMarketplaceRefs rewrites the "ref" of every git-subdir plugin entry in the
// marketplace file at path to tag and returns how many entries it rewrote. The
// number of "ref" fields in the text must equal the number of git-subdir
// entries (an entry without one, or a stray extra, is an error), so a changed
// file shape cannot silently leave an entry unpinned.
func pinMarketplaceRefs(path, tag string) (int, error) {
	if !tagRe.MatchString(tag) {
		return 0, fmt.Errorf("tag %q is not vMAJOR.MINOR.PATCH", tag)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", path, err)
	}
	var mp struct {
		Plugins []struct {
			Source struct {
				Source string `json:"source"`
			} `json:"source"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(data, &mp); err != nil {
		return 0, fmt.Errorf("parsing %s: %w", path, err)
	}
	want := 0
	for _, p := range mp.Plugins {
		if p.Source.Source == "git-subdir" {
			want++
		}
	}
	if want == 0 {
		return 0, fmt.Errorf("%s: no git-subdir plugin entries", path)
	}
	content := string(data)
	got := len(refFieldRe.FindAllStringIndex(content, -1))
	if got != want {
		return 0, fmt.Errorf("%s: %d \"ref\" fields for %d git-subdir entries; refusing to guess", path, got, want)
	}
	updated := refFieldRe.ReplaceAllString(content, "${1}"+tag+"${3}")
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return 0, fmt.Errorf("writing %s: %w", path, err)
	}
	return want, nil
}

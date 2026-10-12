package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f.json")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const manifest = "{\n  \"name\": \"fabrik\",\n  \"version\": \"0.2.3\",\n  \"description\": \"x\"\n}\n"

func TestSetManifestVersion(t *testing.T) {
	p := write(t, manifest)
	old, err := setManifestVersion(p, "0.0.91")
	if err != nil || old != "0.2.3" {
		t.Fatalf("got %q, %v", old, err)
	}
	want := strings.Replace(manifest, "0.2.3", "0.0.91", 1)
	if got := read(t, p); got != want {
		t.Errorf("formatting not preserved:\n%s", got)
	}
	// idempotent
	if old, err := setManifestVersion(p, "0.0.91"); err != nil || old != "0.0.91" {
		t.Errorf("second run: %q, %v", old, err)
	}
	if read(t, p) != want {
		t.Error("second run changed the file")
	}
}

func TestSetManifestVersionErrors(t *testing.T) {
	for name, tc := range map[string]struct{ content, version string }{
		"leading v":        {manifest, "v0.0.91"},
		"not semver":       {manifest, "latest"},
		"missing field":    {"{\n  \"name\": \"x\"\n}\n", "1.0.0"},
		"duplicate fields": {"{\"version\": \"1.0.0\", \"a\": {\"version\": \"2.0.0\"}}", "1.0.0"},
	} {
		p := write(t, tc.content)
		if _, err := setManifestVersion(p, tc.version); err == nil {
			t.Errorf("%s: want an error", name)
		}
		if read(t, p) != tc.content {
			t.Errorf("%s: file modified on error", name)
		}
	}
	if _, err := setManifestVersion(filepath.Join(t.TempDir(), "nope.json"), "1.0.0"); err == nil {
		t.Error("missing file must error")
	}
}

const marketplace = `{
  "name": "fabrik",
  "plugins": [
    {
      "name": "fabrik",
      "source": {
        "source": "git-subdir",
        "url": "https://github.com/handarbeit/fabrik.git",
        "path": "plugin/fabrik",
        "ref": "main"
      }
    },
    {
      "name": "entwurf",
      "source": {
        "source": "git-subdir",
        "path": "plugin/entwurf",
        "ref":"main"
      }
    }
  ]
}
`

func TestPinMarketplaceRefs(t *testing.T) {
	p := write(t, marketplace)
	n, err := pinMarketplaceRefs(p, "v0.0.91")
	if err != nil || n != 2 {
		t.Fatalf("got %d, %v", n, err)
	}
	got := read(t, p)
	if !reflect.DeepEqual(refsOf(got), []string{"v0.0.91", "v0.0.91"}) {
		t.Errorf("refs = %v", refsOf(got))
	}
	// Formatting (including the unspaced entry) is untouched apart from the values.
	want := strings.ReplaceAll(marketplace, `"main"`, `"v0.0.91"`)
	if got != want {
		t.Errorf("formatting changed:\n%s", got)
	}
	// idempotent
	if n, err := pinMarketplaceRefs(p, "v0.0.91"); err != nil || n != 2 || read(t, p) != want {
		t.Errorf("second run: %d, %v", n, err)
	}
}

func TestPinMarketplaceRefsErrors(t *testing.T) {
	missingRef := strings.Replace(marketplace, ",\n        \"ref\": \"main\"", "", 1)
	for name, tc := range map[string]struct{ content, tag string }{
		"count mismatch":  {missingRef, "v1.0.0"},
		"bad tag":         {marketplace, "1.0.0"},
		"not json":        {"{", "v1.0.0"},
		"no git-subdir":   {`{"plugins":[{"name":"x","source":{"source":"github"}}]}`, "v1.0.0"},
		"stray extra ref": {strings.Replace(marketplace, `"name": "fabrik",`, `"name": "fabrik", "ref": "x",`, 1), "v1.0.0"},
	} {
		p := write(t, tc.content)
		if _, err := pinMarketplaceRefs(p, tc.tag); err == nil {
			t.Errorf("%s: want an error", name)
		}
		if read(t, p) != tc.content {
			t.Errorf("%s: file modified on error", name)
		}
	}
}

// The checked-in marketplace file must be pinnable: every git-subdir entry has
// exactly one ref field.
func TestRealMarketplaceIsPinnable(t *testing.T) {
	src, err := os.ReadFile("../../.claude-plugin/marketplace.json")
	if err != nil {
		t.Fatal(err)
	}
	p := write(t, string(src))
	if n, err := pinMarketplaceRefs(p, "v9.9.9"); err != nil || n < 2 {
		t.Fatalf("real marketplace.json: %d, %v", n, err)
	}
}

// refsOf lists the "ref" values of a marketplace file, in order.
func refsOf(content string) []string {
	var out []string
	for _, m := range refFieldRe.FindAllStringSubmatch(content, -1) {
		out = append(out, m[2])
	}
	return out
}

package appwebhook

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFunnelWebhookURL(t *testing.T) {
	for _, tc := range []struct{ dns, path, want string }{
		{"fabrik-models.tail1.ts.net.", "/webhook", "https://fabrik-models.tail1.ts.net/webhook"},
		{"a.b.ts.net", "/x/y", "https://a.b.ts.net/x/y"},
		{"", "/webhook", ""},
	} {
		if got := FunnelWebhookURL(tc.dns, tc.path); got != tc.want {
			t.Errorf("FunnelWebhookURL(%q,%q) = %q, want %q", tc.dns, tc.path, got, tc.want)
		}
	}
}

func TestRedactLog(t *testing.T) {
	in := "To authenticate, visit: https://login.tailscale.com/a/abc123def\n"
	got := RedactLog(in)
	if strings.Contains(got, "abc123def") || !strings.Contains(got, "[login URL withheld]") {
		t.Fatalf("got %q", got)
	}
}

func TestRedactLog_Variants(t *testing.T) {
	for _, in := range []string{
		"visit: HTTPS://Login.Tailscale.com/a/secret1",
		"visit: http://login.tailscale.com/a/secret1",
		"noise https://example.com/?next=https://login.tailscale.com/a/secret1 more",
	} {
		if got := RedactLog(in); strings.Contains(got, "secret1") {
			t.Errorf("RedactLog(%q) = %q leaks the link", in, got)
		}
	}
}

func TestStateExists(t *testing.T) {
	dir := t.TempDir()
	if StateExists(dir) {
		t.Fatal("empty dir has no state")
	}
	p := filepath.Join(dir, NodeStateFile)
	_ = os.WriteFile(p, nil, 0o600)
	if StateExists(dir) {
		t.Fatal("an empty state file is not node state")
	}
	_ = os.WriteFile(p, []byte("x"), 0o600)
	if StateExists(dir) {
		t.Fatal("an unparseable state file is not node state")
	}
	_ = os.WriteFile(p, []byte("{}"), 0o600)
	if StateExists(dir) {
		t.Fatal("tsnet's initial empty store is not node state")
	}
	// A first start that failed on a bad key leaves the machine key behind.
	writeTailnetState(t, dir, false)
	if StateExists(dir) {
		t.Fatal("a machine key without a logged-in profile is not node state: the key is still required")
	}
	writeTailnetState(t, dir, true)
	if !StateExists(dir) {
		t.Fatal("a stored profile with a NodeID is node state")
	}
	if StateExists(filepath.Join(dir, "missing")) {
		t.Fatal("missing dir")
	}
}

// tsnet's log lines carry no line ending and the engine log adds none, so the
// adapter must, or several node lines run together on one line.
func TestWithLineEnd(t *testing.T) {
	for in, want := range map[string]string{
		"a":     "a\n",
		"a\n":   "a\n",
		"":      "\n",
		"a\nb":  "a\nb\n",
		"x\n\n": "x\n\n",
	} {
		if got := WithLineEnd(in); got != want {
			t.Errorf("WithLineEnd(%q) = %q, want %q", in, got, want)
		}
	}
}

// writeTailnetState writes a tsnet FileStore state file: a JSON object of base64
// values. With loggedIn it holds a profile whose prefs carry a NodeID (what a
// completed login stores); without it only the machine key tsnet writes at Start.
func writeTailnetState(t *testing.T, dir string, loggedIn bool) {
	t.Helper()
	entries := map[string][]byte{"_machinekey": []byte("privkey:00")}
	if loggedIn {
		entries["_current-profile"] = []byte("profile-1")
		entries["profile-1"] = []byte(`{"WantRunning":true,"Config":{"NodeID":"nABC","UserProfile":{"LoginName":"tagged-devices"}}}`)
	}
	bs, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, NodeStateFile), bs, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Exactly one non-test source file in the whole module may import tailscale.com:
// the adapter in the tsnetnode sub-package. Every other file (and its tests)
// stays on the Node seam and needs no tailnet (SC-004, FR-012). The walk starts
// at the module root, so the old engine/ location is covered too.
func TestOnlyTheAdapterFileImportsTailscale(t *testing.T) {
	root := moduleRoot(t)
	var importers []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".fabrik", "node_modules", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), `"tailscale.com`) {
			rel, _ := filepath.Rel(root, path)
			importers = append(importers, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "internal/appwebhook/tsnetnode/node.go"
	if len(importers) != 1 || importers[0] != want {
		t.Fatalf("tailscale.com importers = %v; want exactly [%s]", importers, want)
	}
}

// moduleRoot finds the directory holding go.mod above the test's package dir.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

// A consumer that supplies no Logf must not panic when the node logs or closes.
func TestNewFunnelNilLogfDoesNotPanic(t *testing.T) {
	f := NewFunnel(context.Background(), FunnelConfig{Tag: "t"})
	f.cfg.Logf(0, "t", "x\n") // would be a nil-func call without the default
	f.CloseNode()
}

package pruefer

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParsePluginID(t *testing.T) {
	for _, ok := range []string{"acme-review@acme-tools", "a@b", "x.y_z@m-1"} {
		if _, _, good := parsePluginID(ok); !good {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "plain", "@m", "p@", "a@b@c", "a b@m", "../x@m", "p@m/../x", "-p@m"} {
		if _, _, good := parsePluginID(bad); good {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParseGitHubRepo(t *testing.T) {
	good := map[string]string{
		"acme/tools":                         "acme/tools",
		"Acme/Tools.git":                     "acme/tools",
		"https://github.com/acme/tools":      "acme/tools",
		"https://github.com/acme/tools.git":  "acme/tools",
		"https://github.com/acme/tools/":     "acme/tools",
		"https://github.com/Acme/Tools.git/": "acme/tools",
	}
	for in, want := range good {
		got, err := parseGitHubRepo(in)
		if err != nil || got != want {
			t.Errorf("parseGitHubRepo(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"", "acme", "a/b/c", "https://gitlab.com/acme/tools", "https://github.com.evil.com/a/b",
		"http://github.com/a/b", "git@github.com:a/b.git", "ssh://git@github.com/a/b", "https://user@github.com/a/b",
		"https://github.com/a/b?x=1", "https://github.com/a/b#x", "https://github.com/a", "https://github.com/a/b/tree/main",
		"../x", "a/..", "-a/b", "file:///tmp/x",
	} {
		if got, err := parseGitHubRepo(bad); err == nil {
			t.Errorf("parseGitHubRepo(%q) = %q, want a refusal", bad, got)
		}
	}
}

func TestRepoAllowed(t *testing.T) {
	allow := []string{"Acme/Tools", "wild/*", "bad entry", "/x", "x/", "*/*"}
	for _, ok := range []string{"acme/tools", "ACME/TOOLS", "wild/anything", "wild/other"} {
		if !repoAllowed(allow, ok) {
			t.Errorf("%s not allowed", ok)
		}
	}
	for _, no := range []string{"acme/other", "other/tools", "wildcard/x", "x/y", "acme/tools2"} {
		if repoAllowed(allow, no) {
			t.Errorf("%s allowed", no)
		}
	}
	if repoAllowed(nil, "acme/tools") {
		t.Error("an empty allowlist must allow nothing")
	}
}

func TestParseMarketplaceManifest(t *testing.T) {
	m, err := parseMarketplaceManifest([]byte(`{"name":"acme-tools","owner":{"name":"x"},"metadata":{"pluginRoot":"./plugins"},"plugins":[{"name":"a","source":"./a"},{"name":"b","source":{"source":"github","repo":"o/r"}}]}`))
	if err != nil || m.Name != "acme-tools" || m.PluginRoot != "./plugins" || len(m.Plugins) != 2 {
		t.Fatalf("manifest = %+v, %v", m, err)
	}
	if e, ok := m.entry("b"); !ok || len(e.Source) == 0 {
		t.Errorf("entry b = %+v %v", e, ok)
	}
	if _, ok := m.entry("zzz"); ok {
		t.Error("unknown entry found")
	}
	for name, in := range map[string]string{
		"malformed":   `{`,
		"no name":     `{"plugins":[]}`,
		"bad name":    `{"name":"a/b","plugins":[]}`,
		"not utf8":    "{\"name\":\"a\xff\"}",
		"plugins str": `{"name":"a","plugins":"x"}`,
	} {
		if _, err := parseMarketplaceManifest([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := parseMarketplaceManifest([]byte(`{"name":"a","x":"` + strings.Repeat("a", maxRepoPluginDeclBytes) + `"}`)); err == nil {
		t.Error("an oversized manifest was accepted")
	}
}

func TestParseSettingsMarketplaces(t *testing.T) {
	got, err := parseSettingsMarketplaces([]byte(`{
 "enabledPlugins":{"p@m":true},
 "extraKnownMarketplaces":{
  "gh":{"source":{"source":"github","repo":"acme/tools","ref":"v1"}},
  "git":{"source":{"source":"git","url":"https://github.com/acme/other.git"}},
  "dir":{"source":{"source":"directory","path":"/etc"}}
 }}`))
	if err != nil || len(got) != 3 || got["gh"].Repo != "acme/tools" || got["gh"].Ref != "v1" || got["dir"].Kind != "directory" {
		t.Fatalf("got = %+v, %v", got, err)
	}
	if _, err := parseSettingsMarketplaces([]byte(`{`)); err == nil {
		t.Error("malformed settings accepted")
	}
	if m, err := parseSettingsMarketplaces([]byte(`{"permissions":{}}`)); err != nil || len(m) != 0 {
		t.Errorf("settings without marketplaces = %v, %v", m, err)
	}
}

func TestResolveSettingsMarketplace(t *testing.T) {
	cases := []struct {
		name     string
		in       settingsMarketplace
		wantRepo string
		same     bool
		refuse   string
	}{
		{"github", settingsMarketplace{Kind: "github", Repo: "Acme/Tools", Ref: " v1 "}, "acme/tools", false, ""},
		{"git on host", settingsMarketplace{Kind: "git", URL: "https://github.com/acme/tools.git"}, "acme/tools", false, ""},
		{"reviewed repo is in-repo", settingsMarketplace{Kind: "github", Repo: "OWNER/repo", Ref: "main"}, "", true, ""},
		{"directory", settingsMarketplace{Kind: "directory"}, "", false, "not supported"},
		{"file", settingsMarketplace{Kind: "file"}, "", false, "not supported"},
		{"url", settingsMarketplace{Kind: "url"}, "", false, "not supported"},
		{"settings", settingsMarketplace{Kind: "settings"}, "", false, "not supported"},
		{"npm", settingsMarketplace{Kind: "npm"}, "", false, "not supported"},
		{"other host", settingsMarketplace{Kind: "git", URL: "https://gitlab.com/a/b.git"}, "", false, "https://github.com"},
		{"path", settingsMarketplace{Kind: "github", Repo: "a/b", Path: "sub"}, "", false, "path"},
		{"sparse", settingsMarketplace{Kind: "github", Repo: "a/b", SparsePaths: []string{"x"}}, "", false, "sparsePaths"},
	}
	for _, tc := range cases {
		ext, same, err := resolveSettingsMarketplace(tc.in, "owner/repo")
		if tc.refuse != "" {
			if err == nil || !strings.Contains(err.Error(), tc.refuse) {
				t.Errorf("%s: err = %v, want %q", tc.name, err, tc.refuse)
			}
			continue
		}
		if err != nil || ext.Repo != tc.wantRepo || same != tc.same {
			t.Errorf("%s: %+v same=%v err=%v", tc.name, ext, same, err)
		}
	}
	ext, _, _ := resolveSettingsMarketplace(cases[0].in, "owner/repo")
	if ext.Ref != "v1" {
		t.Errorf("ref not trimmed: %q", ext.Ref)
	}
}

func TestResolvePluginSource(t *testing.T) {
	type want struct {
		rel         bool
		repo, path  string
		ref, sha    string
		refuseMatch string
	}
	sha := strings.Repeat("a", 40)
	cases := []struct {
		name string
		src  string
		root string
		want want
	}{
		{"relative", `"./plugins/x"`, "", want{rel: true, path: "plugins/x"}},
		{"root", `"."`, "", want{rel: true, path: ""}},
		{"dot slash", `"./"`, "", want{rel: true, path: ""}},
		{"bare name under pluginRoot", `"x"`, "./plugins", want{rel: true, path: "plugins/x"}},
		{"bare name without pluginRoot", `"x"`, "", want{refuseMatch: "must start with ./"}},
		{"dotdot", `"./../x"`, "", want{refuseMatch: "not usable"}},
		{"deep dotdot", `"./a/../../x"`, "", want{refuseMatch: "not usable"}},
		{"absolute", `"/etc"`, "", want{refuseMatch: "must start with ./"}},
		{"github sha", `{"source":"github","repo":"Acme/X","sha":"` + sha + `"}`, "", want{repo: "acme/x", sha: sha}},
		{"github ref", `{"source":"github","repo":"acme/x","ref":"v1"}`, "", want{repo: "acme/x", ref: "v1"}},
		{"git-subdir", `{"source":"git-subdir","url":"acme/x","path":"tools/p","ref":"v2"}`, "", want{repo: "acme/x", path: "tools/p", ref: "v2"}},
		{"git-subdir url", `{"source":"git-subdir","url":"https://github.com/acme/x.git","path":"p"}`, "", want{repo: "acme/x", path: "p"}},
		{"git-subdir dotdot", `{"source":"git-subdir","url":"acme/x","path":"../p"}`, "", want{refuseMatch: "not usable"}},
		{"url kind on host", `{"source":"url","url":"https://github.com/acme/x.git"}`, "", want{repo: "acme/x"}},
		{"url kind elsewhere", `{"source":"url","url":"https://gitlab.com/acme/x.git"}`, "", want{refuseMatch: "https://github.com"}},
		{"npm", `{"source":"npm","package":"x"}`, "", want{refuseMatch: "not supported"}},
		{"archive", `{"source":"archive","url":"https://x/y.zip"}`, "", want{refuseMatch: "not supported"}},
		{"command", `{"source":"command","command":"x"}`, "", want{refuseMatch: "not supported"}},
		{"github with path", `{"source":"github","repo":"a/b","path":"x"}`, "", want{refuseMatch: "path"}},
		{"null", `null`, "", want{refuseMatch: "no source"}},
		{"number", `5`, "", want{refuseMatch: "malformed"}},
		{"empty", ``, "", want{refuseMatch: "no source"}},
	}
	for _, tc := range cases {
		got, err := resolvePluginSource(json.RawMessage(tc.src), tc.root)
		if tc.want.refuseMatch != "" {
			if err == nil || !strings.Contains(err.Error(), tc.want.refuseMatch) {
				t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want.refuseMatch)
			}
			continue
		}
		if err != nil || got.Relative != tc.want.rel || got.Repo != tc.want.repo || got.Path != tc.want.path || got.Ref != tc.want.ref || got.SHA != tc.want.sha {
			t.Errorf("%s: %+v, %v; want %+v", tc.name, got, err, tc.want)
		}
	}
}

func TestPinFor(t *testing.T) {
	sha := strings.Repeat("AbC", 13) + "d"
	if got, err := pinFor(sha, "main"); err != nil || got.Commit != strings.ToLower(sha) || got.Resolve != "" {
		t.Errorf("sha wins: %+v, %v", got, err)
	}
	if got, err := pinFor("", sha); err != nil || got.Commit != strings.ToLower(sha) {
		t.Errorf("40-hex ref: %+v, %v", got, err)
	}
	if got, err := pinFor("", "v1.2"); err != nil || got.Resolve != "v1.2" || got.Commit != "" {
		t.Errorf("named ref: %+v, %v", got, err)
	}
	if _, err := pinFor("", ""); err == nil {
		t.Error("an unpinned source must be refused")
	}
	if _, err := pinFor("abc123", ""); err == nil {
		t.Error("a short sha must be refused")
	}
}

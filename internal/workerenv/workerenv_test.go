package workerenv

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestListsAreDisjointAndUnique(t *testing.T) {
	seen := map[string]string{}
	check := func(list, name string) {
		if prev, dup := seen[name]; dup {
			t.Errorf("%s appears in both %s and %s", name, prev, list)
		}
		seen[name] = list
	}
	for _, v := range Scrubbed {
		check("Scrubbed", v.Name)
		if v.Reason == "" || v.Group == "" {
			t.Errorf("%s: scrubbed entries need a group and a reason", v.Name)
		}
	}
	for _, v := range EngineSet {
		check("EngineSet", v.Name)
		if v.Reason == "" {
			t.Errorf("%s: forwarded entries need a stated reason", v.Name)
		}
	}
	for _, n := range NotEnv {
		check("NotEnv", n)
	}
}

func TestSanctionedAccessIsNeverScrubbed(t *testing.T) {
	for _, n := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_HOST", "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_7", "FABRIK_ISSUE", "FABRIK_ROOT"} {
		if IsScrubbed(n) {
			t.Errorf("%s must not be on the scrub list", n)
		}
		if !Protected(n) {
			t.Errorf("%s must be Protected", n)
		}
	}
	for _, s := range Sentinels(Resolve(nil, "")) {
		if Protected(s) {
			t.Errorf("sentinel for protected name %s", s)
		}
	}
}

func TestRequiredCredentialsAreScrubbed(t *testing.T) {
	for _, n := range []string{
		"FABRIK_GITHUB_APP_PRIVATE_KEY_PATH", "FABRIK_GITHUB_APP_ID", "FABRIK_GITHUB_APP_INSTALLATION_ID",
		"FABRIK_GITHUB_WEBHOOK_SECRET", "FABRIK_TOKEN", "FABRIK_REVIEWER_TOKEN",
		"FABRIK_ANTHROPIC_API_KEY", "FABRIK_ANTHROPIC_ENV_PASSTHROUGH", OptInVar,
		"FABRIK_HOOKDECK_API_KEY_ENV", "FABRIK_HOOKDECK_WEBHOOK_SECRET_ENV", "HOOKDECK_API_KEY",
	} {
		if !IsScrubbed(n) {
			t.Errorf("%s must be scrubbed", n)
		}
	}
}

func TestParseNames(t *testing.T) {
	got := ParseNames(" A, B ,,A,C, ")
	if strings.Join(got, "|") != "A|B|C" {
		t.Fatalf("got %v", got)
	}
	if ParseNames("") != nil {
		t.Fatal("empty input should yield nil")
	}
}

func TestResolve(t *testing.T) {
	r := Resolve([]string{"MY_HD_KEY", "PATH", "GH_TOKEN", " "}, "FABRIK_TOKEN, GH_TOKEN, NOT_LISTED, MY_HD_KEY, "+OptInVar+", FABRIK_ANTHROPIC_API_KEY")
	if strings.Join(r.Extra, ",") != "MY_HD_KEY" {
		t.Errorf("Extra = %v", r.Extra)
	}
	if strings.Join(r.Dropped, ",") != "PATH,GH_TOKEN" {
		t.Errorf("Dropped = %v", r.Dropped)
	}
	if strings.Join(r.Admitted, ",") != "FABRIK_TOKEN,MY_HD_KEY" {
		t.Errorf("Admitted = %v", r.Admitted)
	}
	if strings.Join(r.Ignored, ",") != "GH_TOKEN,NOT_LISTED,"+OptInVar+",FABRIK_ANTHROPIC_API_KEY" {
		t.Errorf("Ignored = %v", r.Ignored)
	}
}

func TestSentinels(t *testing.T) {
	has := func(list []string, n string) bool {
		for _, x := range list {
			if x == n {
				return true
			}
		}
		return false
	}
	s := Sentinels(Resolve([]string{"MY_HD_KEY"}, ""))
	for _, n := range []string{"FABRIK_TOKEN", "MY_HD_KEY", "HOOKDECK_API_KEY", "FABRIK_GITHUB_WEBHOOK_SECRET", OptInVar} {
		if !has(s, n) {
			t.Errorf("missing sentinel %s", n)
		}
	}
	s = Sentinels(Resolve(nil, "FABRIK_TOKEN,"+OptInVar))
	if has(s, "FABRIK_TOKEN") {
		t.Error("admitted FABRIK_TOKEN still sentineled")
	}
	if !has(s, OptInVar) {
		t.Error("the opt-in variable must always be scrubbed")
	}
	for _, kv := range s {
		if strings.Contains(kv, "=") {
			t.Errorf("sentinel %q must be a bare key", kv)
		}
	}
}

var envLiteral = regexp.MustCompile(`^FABRIK_[A-Z0-9_]+$`)

// TestEveryFabrikLiteralIsClassified is the R1a gate: every FABRIK_* string
// literal in the engine's non-test sources must be scrubbed, engine-set, or a
// declared non-env marker, so a future variable cannot reach workers by
// omission. Add new variables to Scrubbed (the default) — or to EngineSet with
// a stated reason.
func TestEveryFabrikLiteralIsClassified(t *testing.T) {
	root := filepath.Join("..", "..")
	var dirs = []string{"config", "cmd", "engine", "internal", "stages", "github", "plugin", "tui"}
	fset := token.NewFileSet()
	found := map[string]string{}
	for _, d := range dirs {
		_ = filepath.WalkDir(filepath.Join(root, d), func(path string, de fs.DirEntry, err error) error {
			if err != nil || de.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				t.Errorf("parse %s: %v", path, perr)
				return nil
			}
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				s, uerr := strconv.Unquote(lit.Value)
				if uerr == nil && envLiteral.MatchString(s) {
					found[s] = path
				}
				return true
			})
			return nil
		})
	}
	if len(found) == 0 {
		t.Fatal("scan found no FABRIK_* literals — the walk is broken")
	}
	for name, path := range found {
		if !Classified(name) {
			t.Errorf("%s (%s) is in none of Scrubbed/EngineSet/NotEnv — classify it (default: Scrubbed)", name, path)
		}
	}
}

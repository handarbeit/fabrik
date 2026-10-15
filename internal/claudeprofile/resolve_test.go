package claudeprofile

import (
	"strings"
	"testing"
)

func fullInputs() Inputs {
	return Inputs{
		Flag:        "/flag",
		ShellFabrik: "/shell",
		Dotenv:      map[string]string{DeliberateEnv: "/dotenv-fabrik", BareEnv: "/dotenv-bare"},
		YAML:        "/yaml",
		Ambient:     "/ambient",
	}
}

// SC-001: each layer wins over every layer below it.
func TestResolve_EachLayerBeatsEveryLayerBelow(t *testing.T) {
	steps := []struct {
		mutate func(*Inputs)
		want   string
		layer  Layer
	}{
		{func(in *Inputs) {}, "/flag", LayerFlag},
		{func(in *Inputs) { in.Flag = "" }, "/shell", LayerShellEnv},
		{func(in *Inputs) { in.ShellFabrik = "" }, "/dotenv-fabrik", LayerDotenv},
		{func(in *Inputs) { delete(in.Dotenv, DeliberateEnv) }, "/dotenv-bare", LayerDotenv},
		{func(in *Inputs) { delete(in.Dotenv, BareEnv) }, "/yaml", LayerYAML},
		{func(in *Inputs) { in.YAML = "" }, "/ambient", LayerAmbient},
		{func(in *Inputs) { in.Ambient = "" }, "", ""},
	}
	in := fullInputs()
	for i, st := range steps {
		st.mutate(&in)
		got := Resolve(in)
		if got.Dir != st.want || got.Winner != st.layer {
			t.Fatalf("step %d: got (%q, %q), want (%q, %q)", i, got.Dir, got.Winner, st.want, st.layer)
		}
	}
}

// SC-002: the #1807 regression and the override cases.
func TestResolve_DotenvBeatsAmbientAndWarns(t *testing.T) {
	r := Resolve(Inputs{Ambient: "/a", Dotenv: map[string]string{BareEnv: "/b"}})
	if r.Dir != "/b" || r.Winner != LayerDotenv {
		t.Fatalf("got (%q, %q), want (/b, .env)", r.Dir, r.Winner)
	}
	if !r.Disagree() || len(r.Sources) != 2 {
		t.Fatalf("want a two-source disagreement, got %+v", r)
	}
}

func TestResolve_YAMLBeatsAmbient(t *testing.T) {
	if r := Resolve(Inputs{Ambient: "/a", YAML: "/c"}); r.Dir != "/c" {
		t.Errorf("got %q, want /c", r.Dir)
	}
}

func TestResolve_ShellFabrikBeatsYAML(t *testing.T) {
	if r := Resolve(Inputs{YAML: "/c", ShellFabrik: "/d"}); r.Dir != "/d" {
		t.Errorf("got %q, want /d", r.Dir)
	}
}

func TestResolve_DotenvBothNamesReportsConflict(t *testing.T) {
	r := Resolve(Inputs{Dotenv: map[string]string{DeliberateEnv: "/f", BareEnv: "/g"}})
	if r.Dir != "/f" || !r.Disagree() {
		t.Fatalf("got %+v", r)
	}
}

func TestResolve_AmbientAloneIsResolvedWithoutDisagreement(t *testing.T) {
	r := Resolve(Inputs{Ambient: "/a"})
	if r.Dir != "/a" || r.Winner != LayerAmbient || r.Disagree() {
		t.Fatalf("got %+v", r)
	}
}

func TestResolve_NothingSet(t *testing.T) {
	r := Resolve(Inputs{Dotenv: map[string]string{"OTHER": "x"}})
	if r.Dir != "" || r.Winner != "" || len(r.Sources) != 0 || r.Disagree() {
		t.Fatalf("got %+v", r)
	}
}

func TestResolve_SameDirectoryIsNotADisagreement(t *testing.T) {
	r := Resolve(Inputs{Home: "/home/u", YAML: "/home/u/p/", Ambient: "~/p", Flag: "/home/u//p/."})
	if r.Disagree() {
		t.Fatalf("same directory spelled three ways must agree: %+v", r)
	}
	if r.Dir != "/home/u/p" {
		t.Errorf("Dir = %q, want /home/u/p", r.Dir)
	}
}

func TestResolve_TildeNotExpandedWithoutHome(t *testing.T) {
	if r := Resolve(Inputs{Ambient: "~/p"}); r.Dir != "~/p" {
		t.Errorf("got %q", r.Dir)
	}
}

func TestResolve_RelativeValueAnchoredAtCwd(t *testing.T) {
	r := Resolve(Inputs{Cwd: "/proj", YAML: ".claude-work", Ambient: "/proj/.claude-work/"})
	if r.Dir != "/proj/.claude-work" {
		t.Errorf("Dir = %q, want /proj/.claude-work", r.Dir)
	}
	if r.Disagree() {
		t.Errorf("relative and absolute spellings of one directory must agree: %+v", r)
	}
}

func TestResolve_RelativeValueKeptWithoutCwd(t *testing.T) {
	if r := Resolve(Inputs{YAML: ".claude-work"}); r.Dir != ".claude-work" {
		t.Errorf("got %q", r.Dir)
	}
}

func TestFingerprint_StableAndEmailFree(t *testing.T) {
	a := Resolve(Inputs{Ambient: "/a", Dotenv: map[string]string{BareEnv: "/b"}})
	b := a.FillAccounts(func(dir string) string { return "user" + dir + "@x.com" })
	c := a.FillAccounts(func(dir string) string { return "other@x.com" })
	if b.Fingerprint() != a.Fingerprint() || c.Fingerprint() != a.Fingerprint() {
		t.Error("fingerprint must not depend on account emails")
	}
	if strings.Contains(a.Fingerprint(), "@") {
		t.Errorf("fingerprint %q carries an email", a.Fingerprint())
	}
	if !strings.Contains(a.Fingerprint(), "/a") || !strings.Contains(a.Fingerprint(), "/b") {
		t.Errorf("fingerprint %q missing a source", a.Fingerprint())
	}
}

func TestFillAccounts_DoesNotMutateReceiver(t *testing.T) {
	a := Resolve(Inputs{Ambient: "/a"})
	b := a.FillAccounts(func(string) string { return "e@x.com" })
	if a.Sources[0].Email != "" || b.Sources[0].Email != "e@x.com" || b.WinnerEmail() != "e@x.com" {
		t.Errorf("a=%+v b=%+v", a, b)
	}
}

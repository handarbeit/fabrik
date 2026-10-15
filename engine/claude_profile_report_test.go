package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/internal/claudeprofile"
)

func profileWithAccount(t *testing.T, email string) string {
	t.Helper()
	dir := t.TempDir()
	if email != "" {
		body := `{"oauthAccount":{"emailAddress":"` + email + `"}}`
		if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func resolveWithAccounts(in claudeprofile.Inputs) claudeprofile.Resolution {
	return claudeprofile.Resolve(in).FillAccounts(claudeprofile.Account)
}

func TestReportClaudeProfile_OneLineNamesDirAccountAndSource(t *testing.T) {
	isolateWarnings(t)
	dir := profileWithAccount(t, "a@b.com")
	var buf strings.Builder
	reportClaudeProfile(resolveWithAccounts(claudeprofile.Inputs{YAML: dir}), &buf)

	want := `[startup] notice: Claude profile "` + dir + `" (a@b.com) from .fabrik/config.yaml` + "\n"
	if buf.String() != want {
		t.Errorf("got %q, want %q", buf.String(), want)
	}
	if k := warningKeys(t); len(k) != 0 {
		t.Errorf("a lone source must not warn: %v", k)
	}
}

func TestReportClaudeProfile_UnreadableAccountOmitsEmail(t *testing.T) {
	isolateWarnings(t)
	dir := profileWithAccount(t, "")
	var buf strings.Builder
	reportClaudeProfile(resolveWithAccounts(claudeprofile.Inputs{Ambient: dir}), &buf)
	out := buf.String()
	if !strings.Contains(out, dir) || strings.Contains(out, "@") || !strings.Contains(out, "CLAUDE_CONFIG_DIR (environment)") {
		t.Errorf("got %q", out)
	}
}

func TestReportClaudeProfile_NothingResolvedIsSilent(t *testing.T) {
	isolateWarnings(t)
	var buf strings.Builder
	reportClaudeProfile(claudeprofile.Resolution{}, &buf)
	if buf.Len() != 0 {
		t.Errorf("got %q, want silence", buf.String())
	}
}

// SC-007: recorded on disagreement, cleared on agreement.
func TestReportClaudeProfile_DisagreementRecordsAndClears(t *testing.T) {
	isolateWarnings(t)
	a := profileWithAccount(t, "ambient@x.com")
	b := profileWithAccount(t, "dotenv@x.com")
	var buf strings.Builder
	reportClaudeProfile(resolveWithAccounts(claudeprofile.Inputs{
		Ambient: a, Dotenv: map[string]string{claudeprofile.BareEnv: b},
	}), &buf)

	out := buf.String()
	for _, want := range []string{a, b, "ambient@x.com", "dotenv@x.com", "[startup] warning:", "Using " + `"` + b + `"`} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if k := warningKeys(t); len(k) != 1 || !k["claude_config_dir:sources"] {
		t.Fatalf("keys = %v", k)
	}

	reportClaudeProfile(resolveWithAccounts(claudeprofile.Inputs{
		Ambient: a, Dotenv: map[string]string{claudeprofile.BareEnv: a},
	}), &strings.Builder{})
	if k := warningKeys(t); len(k) != 0 {
		t.Errorf("agreement must clear the entry: %v", k)
	}
}

func TestClaudeProfileCondition_FingerprintIgnoresEmails(t *testing.T) {
	in := claudeprofile.Inputs{Ambient: "/a", YAML: "/c"}
	r1 := claudeprofile.Resolve(in).FillAccounts(func(string) string { return "one@x.com" })
	r2 := claudeprofile.Resolve(in).FillAccounts(func(string) string { return "two@x.com" })
	c1, c2 := claudeProfileCondition(r1), claudeProfileCondition(r2)
	if c1 == nil || c2 == nil || c1.Fingerprint != c2.Fingerprint || c1.Detail == c2.Detail {
		t.Errorf("fingerprint must be email-free while detail names emails: %+v / %+v", c1, c2)
	}
}

// SC-006 (engine half): the startup line is built from the same resolution the
// footer reads, so the account in both can only diverge if one stops using it.
func TestClaudeProfile_FooterAndStartupLineShareOneAccount(t *testing.T) {
	dir := profileWithAccount(t, "same@x.com")
	res := resolveWithAccounts(claudeprofile.Inputs{Flag: dir})
	t.Cleanup(claudeprofile.Reset)
	claudeprofile.Set(res)

	footer := ClaudeProfileAccount(claudeprofile.Current().Dir)
	if footer != "same@x.com" || !strings.Contains(claudeProfileStartupLine(claudeprofile.Current()), "("+footer+")") {
		t.Errorf("footer %q and startup line %q diverge", footer, claudeProfileStartupLine(claudeprofile.Current()))
	}
}

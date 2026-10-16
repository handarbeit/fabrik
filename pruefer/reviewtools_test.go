package pruefer

import (
	"reflect"
	"strings"
	"testing"
)

func TestValidateReviewToolRule_Accepts(t *testing.T) {
	for in, want := range map[string]string{
		"Bash(golangci-lint:*)":                  "Bash(golangci-lint:*)",
		"  Bash(semgrep   --config   p/ci *)  ":  "Bash(semgrep --config p/ci *)",
		"Bash(shellcheck *)":                     "Bash(shellcheck *)",
		"Bash(go vet:*)":                         "Bash(go vet:*)",
		"Bash(govulncheck ./...)":                "Bash(govulncheck ./...)",
		"Bash(staticcheck)":                      "Bash(staticcheck)",
		"Bash(/usr/local/bin/gitleaks detect *)": "Bash(/usr/local/bin/gitleaks detect *)",
		"Bash(go list -m all)":                   "Bash(go list -m all)",
		"Bash(yarn audit:*)":                     "Bash(yarn audit:*)",
		"Bash(cargo audit:*)":                    "Bash(cargo audit:*)",
		"Bash(cargo tree)":                       "Bash(cargo tree)",
		"Bash(pnpm audit --prod)":                "Bash(pnpm audit --prod)",
		"Bash(sha256sum *)":                      "Bash(sha256sum *)",
	} {
		got, err := validateReviewToolRule(in)
		if err != nil {
			t.Errorf("%q: unexpected error %v", in, err)
		} else if got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestValidateReviewToolRule_RefusesTestRunners(t *testing.T) {
	for _, in := range []string{
		"Bash(go test:*)", "Bash(go test *)", "Bash(go test)", "Bash(go   test   ./...)",
		"Bash(/usr/local/go/bin/go test:*)", "Bash(go -count=1 test:*)",
		"Bash(cargo t:*)", "Bash(cargo t)", "Bash(cargo --locked t)", "Bash(npm test)", "Bash(npm run test:*)", "Bash(yarn test *)", "Bash(pnpm test)",
		"Bash(pytest:*)", "Bash(/usr/bin/pytest *)", "Bash(cargo test:*)", "Bash(cargo nextest run)",
		"Bash(make test)", "Bash(make check:*)", "Bash(jest:*)", "Bash(vitest *)",
		"Bash(mocha)", "Bash(rspec)", "Bash(npm run-script test)", "Bash(npm it)", "Bash(yarn run test)", "Bash(tox)", "Bash(dotnet test)", "Bash(mvn -q test)",
		// Versioned launcher binaries are still the launcher.
		"Bash(go1.22 test:*)", "Bash(/usr/local/go/bin/go1.22 -count=1 test *)", "Bash(npm9 test)", "Bash(cargo1 test)",
	} {
		_, err := validateReviewToolRule(in)
		if err == nil {
			t.Errorf("%q: expected refusal", in)
			continue
		}
		if !strings.Contains(err.Error(), "test runners are not permitted") {
			t.Errorf("%q: error does not state the reason: %v", in, err)
		}
		if !strings.Contains(err.Error(), "review_tools entry") {
			t.Errorf("%q: error does not name the entry: %v", in, err)
		}
	}
}

func TestValidateReviewToolRule_RefusesBlanketMalformedAndLaunchers(t *testing.T) {
	for _, in := range []string{
		"", "   ", "Bash", "Bash()", "Bash(*)", "Bash(:*)", "Bash( * )",
		"Edit", "Write(**)", "Read(//tmp/**)", "WebFetch", "Task", "Skill(x:y)",
		"Bash(shellcheck", "Bash shellcheck)", "Bash(shell(check))",
		"Bash(shell*check)", "Bash(shellcheck;rm -rf x)", "Bash(a && b)", "Bash(a | b)",
		"Bash(a `b`)", "Bash(a $(b))", "Bash(a, b)", "Bash(a \"b\")", "Bash(a\nb)", "Bash(a\x00b)",
		"Bash(-x)", "Bash(go:*)", "Bash(make:*)", "Bash(npm *)", "Bash(cargo)",
		"Bash(bash -c *)", "Bash(sh:*)", "Bash(env *)", "Bash(xargs *)", "Bash(python3 -m foo)", "Bash(node *)",
		// Package-runner wrappers and container runners fetch and execute arbitrary code.
		"Bash(uvx ruff:*)", "Bash(bunx eslint *)", "Bash(pnpx foo)", "Bash(pipx run foo)", "Bash(pipx ruff)", "Bash(docker run *)", "Bash(podman:*)", "Bash(/usr/bin/uvx ruff)",
		"Bash(git *)", "Bash(curl *)", "Bash(" + strings.Repeat("a", 300) + ")",
		// Launchers with a generic run subcommand execute repo-controlled code.
		"Bash(npm run:*)", "Bash(yarn run:*)", "Bash(pnpm run:*)", "Bash(npm exec:*)", "Bash(npm run-script lint)",
		"Bash(go run:*)", "Bash(go generate:*)", "Bash(cargo run:*)", "Bash(pnpm dlx foo)", "Bash(bun x foo)",
		"Bash(npm --prefix web run lint)", "Bash(cargo r:*)", "Bash(cargo r --bin x)", "Bash(cargo clippy:*)", "Bash(cargo check:*)", "Bash(cargo build)", "Bash(cargo doc *)", "Bash(mvn verify:*)", "Bash(gradle build:*)", "Bash(./gradlew build)", "Bash(dotnet build:*)", "Bash(dotnet list package)", "Bash(yarn lint)", "Bash(yarn lint:*)", "Bash(pnpm lint:*)", "Bash(bun lint)", "Bash(yarn --cwd web lint)", "Bash(yarn)", "Bash(pnpm --version)", "Bash(deno task build)", "Bash(npm stop)", "Bash(/usr/bin/npm run lint)",
		// make and friends run recipes from the reviewed tree, whatever the target.
		"Bash(make lint)", "Bash(make4 lint)", "Bash(make -C sub lint *)", "Bash(gmake:*)", "Bash(just lint)",
		// Versioned interpreters match their unversioned name.
		"Bash(python3.12 -m foo)", "Bash(node20 x.js)", "Bash(ruby3.2 x.rb)", "Bash(/usr/bin/perl5 x)",
	} {
		if got, err := validateReviewToolRule(in); err == nil {
			t.Errorf("%q: expected refusal, accepted as %q", in, got)
		}
	}
}

func TestValidateReviewTools_NormalisesAndDedupes(t *testing.T) {
	got, err := validateReviewTools([]string{"Bash(shellcheck *)", " Bash(shellcheck   *) ", "Bash(semgrep:*)"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Bash(shellcheck *)", "Bash(semgrep:*)"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
	if _, err := validateReviewTools([]string{"Bash(shellcheck *)", "Bash(go test:*)"}); err == nil {
		t.Error("a list containing a refused rule must fail as a whole")
	}
	if got, err := validateReviewTools(nil); err != nil || len(got) != 0 {
		t.Errorf("empty list: %v %v", got, err)
	}
}

func ptr(s ...string) *[]string { return &s }

func TestSelectReviewTools(t *testing.T) {
	op := []string{"Bash(a *)", "Bash(b *)"}
	sel, ign := selectReviewTools(op, nil)
	if !reflect.DeepEqual(sel, op) || ign != nil {
		t.Errorf("absent repo key: sel=%v ign=%v", sel, ign)
	}
	sel, ign = selectReviewTools(op, ptr("Bash(a *)", "Bash(c *)"))
	if !reflect.DeepEqual(sel, []string{"Bash(a *)"}) || !reflect.DeepEqual(ign, []string{"Bash(c *)"}) {
		t.Errorf("subset: sel=%v ign=%v", sel, ign)
	}
	sel, ign = selectReviewTools(op, ptr())
	if len(sel) != 0 || len(ign) != 0 {
		t.Errorf("explicit empty: sel=%v ign=%v", sel, ign)
	}
	// Whitespace variants match; operator order wins; duplicates collapse.
	sel, _ = selectReviewTools(op, ptr(" Bash(b   *)", "Bash(a *)", "Bash(a *)"))
	if !reflect.DeepEqual(sel, op) {
		t.Errorf("normalised match: sel=%v", sel)
	}
	// A repo can never widen: refused, blanket and malformed rules are ignored.
	_, ign = selectReviewTools(op, ptr("Bash(go test:*)", "Bash(*)", "Bash", "Bash(a"))
	if len(ign) != 4 {
		t.Errorf("widening rules must all be ignored, got %v", ign)
	}
	sel, _ = selectReviewTools(nil, ptr("Bash(a *)"))
	if len(sel) != 0 {
		t.Errorf("no ceiling: sel=%v", sel)
	}
}

func TestTrustDecision(t *testing.T) {
	for _, a := range []string{"OWNER", "MEMBER", "COLLABORATOR"} {
		if !trustDecision("alice", a).Trusted {
			t.Errorf("%s must be trusted", a)
		}
	}
	for _, a := range []string{"CONTRIBUTOR", "FIRST_TIMER", "FIRST_TIME_CONTRIBUTOR", "NONE", "MANNEQUIN", "", "member", "Owner", "OWNER ", "ADMIN", "unknown"} {
		if trustDecision("alice", a).Trusted {
			t.Errorf("association %q must be outside", a)
		}
	}
	d := trustDecision("dependabot[bot]", "NONE")
	if !d.Trusted || !d.Dependabot {
		t.Errorf("dependabot[bot] must be trusted via the exception: %+v", d)
	}
	if d := trustDecision("dependabot[bot]", "MEMBER"); d.Dependabot {
		t.Errorf("association trust must not be reported as the exception: %+v", d)
	}
	for _, l := range []string{"dependabot", "dependabot-preview[bot]", "not-dependabot[bot]", "Dependabot[bot]", "DEPENDABOT[BOT]", "dependabot[bot] ", "dependabot[bot]x", ""} {
		if trustDecision(l, "NONE").Trusted {
			t.Errorf("lookalike login %q must not be trusted", l)
		}
	}
}

func TestResolveReviewGrant(t *testing.T) {
	op := []string{"Bash(a *)", "Bash(b *)"}
	g := resolveReviewGrant(op, ptr("Bash(a *)", "Bash(c *)"), "alice", "MEMBER")
	if !reflect.DeepEqual(g.Extras, []string{"Bash(a *)"}) || !reflect.DeepEqual(g.RepoIgnored, []string{"Bash(c *)"}) || g.StripPluginHooks {
		t.Errorf("trusted: %+v", g)
	}
	// Fork PR by a trusted member: trust is the author's, nothing else.
	g = resolveReviewGrant(op, nil, "alice", "COLLABORATOR")
	if !reflect.DeepEqual(g.Extras, op) {
		t.Errorf("trusted, no repo key: %+v", g)
	}
	// Outside author: no extras whatever the repo asked for; hooks stripped.
	g = resolveReviewGrant(op, ptr("Bash(a *)", "Bash(c *)"), "bob", "FIRST_TIME_CONTRIBUTOR")
	if len(g.Extras) != 0 || !g.StripPluginHooks || !g.DowngradedExtras || len(g.RepoIgnored) != 0 {
		t.Errorf("outside: %+v", g)
	}
	// Unknown association is outside.
	g = resolveReviewGrant(op, nil, "bob", "")
	if len(g.Extras) != 0 || !g.StripPluginHooks {
		t.Errorf("unknown: %+v", g)
	}
	// Outside author with nothing configured is not "downgraded".
	g = resolveReviewGrant(nil, nil, "bob", "NONE")
	if g.DowngradedExtras || !g.StripPluginHooks {
		t.Errorf("outside, empty ceiling: %+v", g)
	}
	// Dependabot gets the full grant.
	g = resolveReviewGrant(op, nil, "dependabot[bot]", "NONE")
	if !reflect.DeepEqual(g.Extras, op) || g.StripPluginHooks || !g.Trust.Dependabot {
		t.Errorf("dependabot: %+v", g)
	}
}

func TestGrantLogLines(t *testing.T) {
	op := []string{"Bash(a *)", "Bash(b *)"}
	g := resolveReviewGrant(op, ptr("Bash(a *)", "Bash(c *)"), "alice", "MEMBER")
	line := g.grantLogLine("alice")
	for _, want := range []string{"operator=[Bash(a *) Bash(b *)]", "repo_selected=[Bash(a *)]", "repo_ignored=[Bash(c *)]", "association=MEMBER", "dependabot_exception=false", "outside_author=false", "downgraded=false"} {
		if !strings.Contains(line, want) {
			t.Errorf("grant line missing %q: %s", want, line)
		}
	}
	if ig := g.ignoredLogLine(); !strings.Contains(ig, "Bash(c *)") || !strings.Contains(ig, "outside the operator") || strings.Contains(ig, "Bash(a *)") {
		t.Errorf("ignored line: %q", ig)
	}
	if resolveReviewGrant(op, nil, "alice", "MEMBER").ignoredLogLine() != "" {
		t.Error("no ignored line when nothing is ignored")
	}
	o := resolveReviewGrant(op, nil, "bob", "")
	line = o.grantLogLine("bob")
	for _, want := range []string{"association=unknown", "outside_author=true", "downgraded=true", "repo_selected=[]"} {
		if !strings.Contains(line, want) {
			t.Errorf("outside grant line missing %q: %s", want, line)
		}
	}
	o = resolveReviewGrant(op, ptr("Bash(c *)"), "bob", "")
	if ig := o.ignoredLogLine(); !strings.Contains(ig, "not applied: outside author") {
		t.Errorf("outside ignored line: %q", ig)
	}
	// Repo-controlled text is sanitised in logs.
	x := resolveReviewGrant(op, ptr("Bash(c\n*)\x1b[31m"), "alice", "MEMBER")
	if l := x.ignoredLogLine(); strings.ContainsAny(l, "\n\x1b") {
		t.Errorf("unsanitised log line: %q", l)
	}
}

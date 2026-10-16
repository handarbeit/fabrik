package pruefer

import (
	"slices"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/pruefer/reviewplugin"
)

func allowedRules(args []string) []string {
	var out []string
	for i, a := range args {
		if a == "--allowedTools" {
			out = append(out, args[i+1])
		}
	}
	return out
}

// SC-001: an empty grant (the zero value, an outside author, an unconfigured
// operator) builds exactly today's arguments, byte for byte.
func TestBuildReviewArgs_NoExtrasIsByteIdenticalToBaseline(t *testing.T) {
	req := reviewTestScope(t)
	baseline := mustReviewArgs(t, req)

	outside := req
	outside.Grant = resolveReviewGrant([]string{"Bash(shellcheck *)"}, nil, "mallory", "NONE")
	if got := mustReviewArgs(t, outside); !slices.Equal(got, baseline) {
		t.Errorf("outside author args differ from the baseline:\n got %v\nwant %v", got, baseline)
	}
	empty := req
	empty.Grant = resolveReviewGrant(nil, nil, "alice", "MEMBER")
	if got := mustReviewArgs(t, empty); !slices.Equal(got, baseline) {
		t.Errorf("empty ceiling args differ from the baseline:\n got %v\nwant %v", got, baseline)
	}
}

func TestBuildReviewArgs_TrustedGetsTheExtras(t *testing.T) {
	req := reviewTestScope(t)
	baseline := allowedRules(mustReviewArgs(t, req))

	extras := []string{"Bash(golangci-lint:*)", "Bash(semgrep --config p/ci *)"}
	req.Grant = resolveReviewGrant(extras, nil, "alice", "COLLABORATOR")
	args := mustReviewArgs(t, req)
	got := allowedRules(args)
	if want := append(slices.Clone(baseline), extras...); !slices.Equal(got, want) {
		t.Errorf("allow rules = %v, want the base grant plus the extras %v", got, want)
	}
	if err := checkReviewArgsScopedTools(args, req.Grant.Extras, reviewplugin.DefaultRef); err != nil {
		t.Errorf("granted args rejected: %v", err)
	}
	// Non-vacuity of FR-013: extras that were not passed in are a violation.
	if checkReviewArgsScoped(args, reviewplugin.DefaultRef) == nil {
		t.Error("an extra rule the caller did not name must be a violation")
	}
	if checkReviewArgsScopedTools(args, extras[:1], reviewplugin.DefaultRef) == nil {
		t.Error("an extra rule missing from the named set must be a violation")
	}
}

// An extra rule is accepted only when the caller names it AND it is valid: a
// test runner or blanket grant smuggled into the extras is still a violation.
func TestCheckReviewArgsScopedTools_RejectsInvalidExtrasEvenWhenNamed(t *testing.T) {
	req := reviewTestScope(t)
	good := mustReviewArgs(t, req)
	for _, widen := range []string{"Bash(go test:*)", "Bash(*)", "Bash", "Edit", "Write", "WebFetch", "Skill", "Bash(go:*)", "Bash(sh -c *)", "Read", "Bash(shellcheck  *)"} {
		args := append(slices.Clone(good), "--allowedTools", widen)
		if checkReviewArgsScopedTools(args, []string{widen}, reviewplugin.DefaultRef) == nil {
			t.Errorf("%q: naming an invalid extra must not make it legitimate", widen)
		}
	}
}

func TestReviewPrompt_NamesTheGrantedTools(t *testing.T) {
	req := reviewTestScope(t)
	var base strings.Builder
	renderReviewTools(&base, req)
	if strings.Contains(base.String(), "Coded review tools") || !strings.Contains(base.String(), "linters") {
		t.Errorf("a request with no extras must keep today's wording:\n%s", base.String())
	}

	req.Grant = resolveReviewGrant([]string{"Bash(golangci-lint:*)", "Bash(shellcheck *)"}, nil, "alice", "OWNER")
	var b strings.Builder
	renderReviewTools(&b, req)
	section := b.String()
	for _, r := range req.Grant.Extras {
		if !strings.Contains(section, "`"+r+"`") {
			t.Errorf("prompt does not name the granted rule %q", r)
		}
	}
	if strings.Contains(section, "builds, linters") {
		t.Error("the prompt still says linters are unavailable although tools were granted")
	}
	if !strings.Contains(section, "untrusted evidence") || !strings.Contains(section, "scrubbed environment") {
		t.Errorf("prompt must frame tool output as untrusted and name the scrub:\n%s", section)
	}

	// An outside author is told nothing about tools it does not hold.
	req.Grant = resolveReviewGrant([]string{"Bash(shellcheck *)"}, nil, "mallory", "NONE")
	var o strings.Builder
	renderReviewTools(&o, req)
	if o.String() != base.String() {
		t.Error("an outside author's prompt section must equal the baseline")
	}
}

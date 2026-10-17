package pruefer

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

func toolsCfg(rules ...string) Config {
	return Config{ReviewTools: rules, RepoReviewSkill: true}
}

// reviewAs runs ReviewPR for a PR by the given author with a mock invoker that
// reports the plugins it "stripped", and returns the request the invoker saw,
// the posted body and the log lines.
func reviewAs(t *testing.T, client *fakeReviewer, cfg Config, login, assoc string, stripped []string) (ReviewRequest, string, []string) {
	t.Helper()
	logs := capturePluginLogs(t)
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (ReviewResult, error) {
		return ReviewResult{Text: "mock review", NumTurns: 2, CostUSD: 0.01, PluginsStripped: stripped}, nil
	}}
	clone, _ := fakeClone(t, nil)
	pr := gh.PRDetails{Number: 1, Author: login, AuthorAssociation: assoc, HeadSHA: "head-sha", BaseRef: "main"}
	out := ReviewPR(context.Background(), client, claude, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr, nil)
	if !out.Reviewed || out.Err != nil {
		t.Fatalf("outcome = %+v", out)
	}
	if len(client.submitCalls) != 1 {
		t.Fatalf("submit calls = %d", len(client.submitCalls))
	}
	return claude.callsSnapshot()[0], client.submitCalls[0].body, logs()
}

// Acceptance: an operator rule granted to a trusted author's PR is in the
// constructed --allowedTools; the same rule is absent for an outside author,
// whose review body says it was read-only.
func TestReviewPR_OperatorRuleReachesTrustedAuthorsOnly(t *testing.T) {
	cfg := toolsCfg("Bash(golangci-lint:*)")
	for _, assoc := range []string{"OWNER", "MEMBER", "COLLABORATOR"} {
		req, body, logs := reviewAs(t, newFakeReviewer(), cfg, "alice", assoc, nil)
		args, err := buildReviewArgs(withScope(t, req))
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(allowedRules(args), "Bash(golangci-lint:*)") {
			t.Errorf("%s: rule missing from --allowedTools: %v", assoc, allowedRules(args))
		}
		if strings.Contains(body, "Read-only review") {
			t.Errorf("%s: a trusted author's review carries the read-only banner:\n%s", assoc, body)
		}
		if countContaining(logs, "review grant: ") != 1 || countContaining(logs, "association="+assoc) != 1 {
			t.Errorf("%s: grant record missing: %q", assoc, logs)
		}
	}
	for _, assoc := range []string{"CONTRIBUTOR", "FIRST_TIMER", "FIRST_TIME_CONTRIBUTOR", "NONE", "MANNEQUIN", ""} {
		req, body, logs := reviewAs(t, newFakeReviewer(), cfg, "mallory", assoc, nil)
		args, err := buildReviewArgs(withScope(t, req))
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(allowedRules(args), "Bash(golangci-lint:*)") {
			t.Errorf("%q: outside author got the rule: %v", assoc, allowedRules(args))
		}
		for _, want := range []string{"Read-only review", "not owner, member or collaborator", "coded review tools"} {
			if !strings.Contains(body, want) {
				t.Errorf("%q: body lacks %q:\n%s", assoc, want, body)
			}
		}
		if countContaining(logs, "outside_author=true") != 1 || countContaining(logs, "downgraded=true") != 1 {
			t.Errorf("%q: downgrade not recorded: %q", assoc, logs)
		}
	}
}

// withScope gives a request recorded by the mock invoker a real clone and staged
// directory so buildReviewArgs can construct the arguments it would have run.
func withScope(t *testing.T, req ReviewRequest) ReviewRequest {
	t.Helper()
	scope := reviewTestScope(t)
	req.WorkDir, req.Staged = scope.WorkDir, scope.Staged
	return req
}

// Acceptance: a dependabot[bot] PR receives the full grant; lookalikes do not.
func TestReviewPR_DependabotGetsTheFullGrant(t *testing.T) {
	cfg := toolsCfg("Bash(govulncheck ./...)")
	req, body, logs := reviewAs(t, newFakeReviewer(), cfg, "dependabot[bot]", "NONE", nil)
	if !slices.Equal(req.Grant.Extras, cfg.ReviewTools) || req.Grant.StripPluginHooks {
		t.Errorf("grant = %+v", req.Grant)
	}
	if strings.Contains(body, "Read-only review") {
		t.Errorf("body carries the banner:\n%s", body)
	}
	if countContaining(logs, "dependabot_exception=true") != 1 {
		t.Errorf("exception not recorded: %q", logs)
	}
	for _, login := range []string{"dependabot", "dependabot-preview[bot]", "not-dependabot[bot]"} {
		req, _, _ := reviewAs(t, newFakeReviewer(), cfg, login, "NONE", nil)
		if len(req.Grant.Extras) != 0 {
			t.Errorf("%s got the grant", login)
		}
	}
}

// Acceptance: a repo rule outside the operator ceiling is not granted, and the
// log names it exactly once.
func TestReviewPR_RepoRuleOutsideTheCeilingIsIgnoredAndLogged(t *testing.T) {
	client := newFakeReviewer()
	client.repoConfigData = []byte("review_tools:\n  - \"Bash(shellcheck *)\"\n  - \"Bash(semgrep:*)\"\n  - \"Bash(go test:*)\"\n")
	cfg := toolsCfg("Bash(shellcheck *)", "Bash(actionlint)")
	req, _, logs := reviewAs(t, client, cfg, "alice", "MEMBER", nil)
	if !slices.Equal(req.Grant.Extras, []string{"Bash(shellcheck *)"}) {
		t.Errorf("Extras = %v, want only the rule in both lists", req.Grant.Extras)
	}
	if n := countContaining(logs, "ignored: outside the operator's review_tools"); n != 1 {
		t.Fatalf("ignored line logged %d times: %q", n, logs)
	}
	var line string
	for _, l := range logs {
		if strings.Contains(l, "ignored: outside the operator's") {
			line = l
		}
	}
	for _, want := range []string{"Bash(semgrep:*)", "Bash(go test:*)"} {
		if !strings.Contains(line, want) {
			t.Errorf("log line does not name %s: %s", want, line)
		}
	}
	if strings.Contains(line, "Bash(shellcheck *)") {
		t.Errorf("log line names a rule that was granted: %s", line)
	}
}

func TestReviewPR_RepoSelectionAbsentEmptyAndOutsideAuthor(t *testing.T) {
	cfg := toolsCfg("Bash(a *)", "Bash(b *)")
	// absent key: the repo did not narrow.
	req, _, _ := reviewAs(t, newFakeReviewer(), cfg, "alice", "OWNER", nil)
	if !slices.Equal(req.Grant.Extras, cfg.ReviewTools) {
		t.Errorf("absent: %v", req.Grant.Extras)
	}
	// explicit empty list: the repo selected none.
	c := newFakeReviewer()
	c.repoConfigData = []byte("review_tools: []\n")
	req, _, _ = reviewAs(t, c, cfg, "alice", "OWNER", nil)
	if len(req.Grant.Extras) != 0 {
		t.Errorf("explicit empty: %v", req.Grant.Extras)
	}
	// an unreadable repo config means no selection: the full ceiling applies.
	c = newFakeReviewer()
	c.repoConfigData = []byte("review_tools: [unterminated\n")
	req, _, _ = reviewAs(t, c, cfg, "alice", "OWNER", nil)
	if !slices.Equal(req.Grant.Extras, cfg.ReviewTools) {
		t.Errorf("invalid repo config: %v", req.Grant.Extras)
	}
	// outside author: the repo key is logged "not applied" and has no effect.
	c = newFakeReviewer()
	c.repoConfigData = []byte("review_tools: [\"Bash(a *)\"]\n")
	req, _, logs := reviewAs(t, c, cfg, "mallory", "NONE", nil)
	if len(req.Grant.Extras) != 0 || countContaining(logs, "not applied: outside author") != 1 {
		t.Errorf("outside: extras=%v logs=%q", req.Grant.Extras, logs)
	}
}

// FR-009: the notice appears only when something was removed, names the
// stripped plugins, and sits ahead of the skill-fallback notice.
func TestReviewPR_ReadOnlyNotice(t *testing.T) {
	// Outside author, nothing configured, no operator plugin hooks: no banner.
	_, body, _ := reviewAs(t, newFakeReviewer(), toolsCfg(), "mallory", "NONE", nil)
	if strings.Contains(body, "Read-only review") {
		t.Errorf("banner without anything removed:\n%s", body)
	}
	// Plugins stripped, no tools configured.
	_, body, _ = reviewAs(t, newFakeReviewer(), toolsCfg(), "mallory", "NONE", []string{"ops`@x"})
	if !strings.Contains(body, "hooks, MCP servers and tool pre-approvals of operator plugin `ops??x` were not loaded") || strings.Contains(body, "coded review tools") {
		t.Errorf("plugin notice wrong (name must be sanitised):\n%s", body)
	}
	// Association is sanitised and never a repo-controlled string.
	_, body, _ = reviewAs(t, newFakeReviewer(), toolsCfg("Bash(a *)"), "mallory", "NONE`@bob\n", nil)
	if strings.Contains(body, "@bob") || strings.Contains(body, "`@") {
		t.Errorf("unsanitised association in body:\n%s", body)
	}
	// Both notices together: read-only first, then the fallback, then the review.
	logs := capturePluginLogs(t)
	_ = logs
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (ReviewResult, error) {
		return ReviewResult{Text: "mock review", FallbackFrom: "mine:deep", FallbackReason: "it did not resolve", PluginsStripped: []string{"ops"}}, nil
	}}
	client := newFakeReviewer()
	clone, _ := fakeClone(t, nil)
	pr := gh.PRDetails{Number: 1, Author: "mallory", AuthorAssociation: "NONE", HeadSHA: "h", BaseRef: "main"}
	if out := ReviewPR(context.Background(), client, claude, clone, toolsCfg("Bash(a *)"), "bot", "o", "r", pr, nil); !out.Reviewed {
		t.Fatalf("outcome = %+v", out)
	}
	b := client.submitCalls[0].body
	ro, fb, rv := strings.Index(b, "Read-only review"), strings.Index(b, "Review skill fallback"), strings.Index(b, "mock review")
	if ro < 0 || fb < 0 || rv < 0 || !(ro < fb && fb < rv) {
		t.Errorf("notices out of order (ro=%d fb=%d review=%d):\n%s", ro, fb, rv, b)
	}
	// No repo text in the posted notice.
	c := newFakeReviewer()
	c.repoConfigData = []byte("review_tools: [\"Bash(secret-tool-name *)\"]\n")
	_, body, _ = reviewAs(t, c, toolsCfg("Bash(a *)"), "mallory", "NONE", nil)
	if strings.Contains(body, "secret-tool-name") {
		t.Errorf("repo rule text echoed into the posted review:\n%s", body)
	}
}

// A plugin that could not be copied for a read-only review is not loaded at all;
// the posted notice must say so even when no tools were configured and no other
// plugin lost hooks, and a trusted author's review (which loads it) gets none.
func TestReadOnlyNotice_NamesSkippedOperatorPlugins(t *testing.T) {
	outside := reviewGrant{Trust: authorTrust{Association: "NONE"}}
	n := readOnlyNotice(outside, nil, []string{"broken`x"}, nil)
	if !strings.Contains(n, "Read-only review") || !strings.Contains(n, "`broken?x`") || !strings.Contains(n, "not loaded") {
		t.Errorf("notice does not name the skipped plugin: %q", n)
	}
	if got := readOnlyNotice(outside, nil, nil, nil); got != "" {
		t.Errorf("notice with nothing removed = %q, want empty", got)
	}
	trusted := reviewGrant{Trust: authorTrust{Trusted: true, Association: "MEMBER"}}
	if got := readOnlyNotice(trusted, nil, []string{"broken"}, nil); got != "" {
		t.Errorf("trusted author got a read-only notice: %q", got)
	}
}

// A plugin whose copy lost no hooks or MCP servers but did lose other files
// (scripts, other directories) still behaves differently from the trusted
// author's review, so the notice names it.
func TestReadOnlyNotice_NamesPartialOperatorPlugins(t *testing.T) {
	outside := reviewGrant{Trust: authorTrust{Association: "NONE"}}
	n := readOnlyNotice(outside, nil, nil, []string{"scripted"})
	if !strings.Contains(n, "`scripted`") || !strings.Contains(n, "omits") {
		t.Errorf("notice does not name the partially loaded plugin: %q", n)
	}
	trusted := reviewGrant{Trust: authorTrust{Trusted: true, Association: "MEMBER"}}
	if got := readOnlyNotice(trusted, nil, nil, []string{"scripted"}); got != "" {
		t.Errorf("trusted author got a read-only notice: %q", got)
	}
}

func TestStripOperatorPlugins_ReportsPluginsThatLostOnlyScripts(t *testing.T) {
	scripted := filepath.Join(t.TempDir(), "scripted")
	writeTree(t, scripted, map[string]string{
		"skills/s/SKILL.md": "---\nname: s\ndescription: d\n---\nrun ${CLAUDE_PLUGIN_ROOT}/scripts/check.sh\n",
		"scripts/check.sh":  "#!/bin/sh\n",
	})
	clean := filepath.Join(t.TempDir(), "clean")
	writeTree(t, clean, map[string]string{"skills/s/SKILL.md": "---\nname: s\ndescription: d\n---\nx\n"})
	_, rep := stripOperatorPlugins(t.TempDir(), []string{scripted, clean}, 1)
	if len(rep.Partial) != 1 || rep.Partial[0] != "scripted" || len(rep.Stripped) != 0 || len(rep.Skipped) != 0 {
		t.Errorf("report = %+v, want only scripted partial", rep)
	}
}

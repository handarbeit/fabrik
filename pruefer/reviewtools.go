package pruefer

import (
	"fmt"
	"path"
	"slices"
	"strings"
)

// Coded review tools (#2308, ADR 2308). The reviewer's base grant is fixed
// and read-only (#2192). An operator may add Bash(<tool> …) rules for linters
// and scanners to it (Config.ReviewTools, the ceiling); a repo's base-ref
// config may select a subset; and only an author with write access gets the
// result. The functions in this file are pure so each rule of that policy is
// decided, and tested, without a CLI.

// reviewToolsTestRunners are the test runners an operator may not grant
// (running tests is the worker's job, not Pruefer's). Each entry is a command
// whose first word is the executable and whose remaining words must all appear,
// in order, among the rule's words: so "go -count=1 test ./..." and "go test"
// are both caught, and respelling (extra spaces, a path to the binary, the
// ":*" form) cannot bypass it. The set is a maintained denylist, not a
// semantic analysis: a runner that is not listed is the operator's
// responsibility.
var reviewToolsTestRunners = [][]string{
	{"go", "test"},
	{"npm", "test"}, {"npm", "run", "test"}, {"npm", "t"},
	{"yarn", "test"}, {"pnpm", "test"}, {"bun", "test"}, {"deno", "test"},
	{"pytest"}, {"py.test"}, {"python", "-m", "pytest"}, {"python3", "-m", "pytest"},
	{"cargo", "test"}, {"cargo", "nextest"},
	{"make", "test"}, {"make", "check"},
	{"jest"}, {"vitest"}, {"mocha"}, {"rspec"}, {"phpunit"},
	{"tox"}, {"nox"}, {"ctest"},
	{"mvn", "test"}, {"gradle", "test"}, {"gradlew", "test"}, {"dotnet", "test"},
}

// reviewToolsLaunchers are executables that run arbitrary code or arbitrary
// subcommands, so a rule naming one with no further narrowing subsumes the test
// runners above. They are refused as a bare rule ("Bash(go:*)"); a narrowed
// rule such as "Bash(go vet:*)" stays allowed.
var reviewToolsLaunchers = []string{
	"go", "make", "npm", "yarn", "pnpm", "cargo", "pip", "pip3", "uv", "bun", "deno",
	"gradle", "gradlew", "mvn", "dotnet", "git", "gh", "curl", "wget",
}

// reviewToolsInterpreters are refused in every form: a rule that starts an
// interpreter, shell or command wrapper grants whatever it is then told to run.
var reviewToolsInterpreters = []string{
	"sh", "bash", "zsh", "dash", "ksh", "fish", "env", "xargs", "sudo", "doas", "nice",
	"time", "timeout", "nohup", "exec", "eval",
	"node", "npx", "python", "python3", "ruby", "perl", "php", "lua", "awk", "sed",
	"find", "rm", "cp", "mv", "tee", "dd", "chmod", "chown", "ssh", "scp", "nc",
}

const maxReviewToolRuleLen = 200

// normalizeToolRule trims a rule and collapses internal runs of spaces to one
// space. It is the single normalisation behind both validation and the
// repo-versus-operator comparison, so the two can never disagree about whether
// two spellings are the same rule.
//
// Only spaces and tabs are whitespace here: a newline or other control
// character stays in the rule, where validation refuses it, instead of being
// folded into a space that would make a hostile spelling equal to a clean one.
func normalizeToolRule(rule string) string {
	return strings.Join(strings.FieldsFunc(rule, func(r rune) bool { return r == ' ' || r == '\t' }), " ")
}

// toolRuleWordOK reports whether w is a plain command word: letters, digits and
// . _ / + = @ % -, with no glob, quote, shell metacharacter, comma or control
// character.
func toolRuleWordOK(w string) bool {
	if w == "" {
		return false
	}
	for _, r := range w {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("._/+=@%-", r):
		default:
			return false
		}
	}
	return true
}

// validateReviewToolRule checks one operator review_tools entry and returns its
// normalised form. Only Bash command rules are accepted, in the forms
// Bash(<words>:*), Bash(<words> *) and Bash(<words>). Blanket grants, malformed
// rules, interpreters, bare build-tool launchers and test runners are refused
// with an error naming the entry.
func validateReviewToolRule(raw string) (string, error) {
	rule := normalizeToolRule(raw)
	bad := func(format string, args ...any) (string, error) {
		return "", fmt.Errorf("review_tools entry %q: %s", sanitizeNoticeRunes(raw, 120, "()*,;=@+%'\""), fmt.Sprintf(format, args...))
	}
	if rule == "" {
		return bad("empty rule")
	}
	if len(rule) > maxReviewToolRuleLen {
		return bad("longer than %d characters", maxReviewToolRuleLen)
	}
	if !strings.HasPrefix(rule, "Bash(") || !strings.HasSuffix(rule, ")") {
		if rule == "Bash" {
			return bad("a bare Bash grant is not permitted; name the command, for example Bash(shellcheck *)")
		}
		return bad("only Bash(<command> …) rules are supported")
	}
	inner := rule[len("Bash(") : len(rule)-1]
	switch inner {
	case "", "*", ":*":
		return bad("a blanket Bash grant is not permitted; name the command, for example Bash(shellcheck *)")
	}
	var cmd string
	switch {
	case strings.HasSuffix(inner, ":*"):
		cmd = strings.TrimSuffix(inner, ":*")
	case strings.HasSuffix(inner, " *"):
		cmd = strings.TrimSuffix(inner, " *")
	default:
		cmd = inner
	}
	words := strings.Split(cmd, " ")
	for _, w := range words {
		if !toolRuleWordOK(w) {
			return bad("the command %q contains a character that is not permitted (glob, quote, shell metacharacter, comma, parenthesis or control character)", w)
		}
	}
	if strings.HasPrefix(words[0], "-") {
		return bad("the command must start with an executable name")
	}
	exe := path.Base(words[0])
	if slices.Contains(reviewToolsInterpreters, exe) {
		return bad("%q runs arbitrary code and is not permitted", exe)
	}
	if len(words) == 1 && slices.Contains(reviewToolsLaunchers, exe) {
		return bad("%q with no narrowing subcommand subsumes test runners and arbitrary code; name the subcommand, for example Bash(%s vet:*)", exe, exe)
	}
	norm := append([]string{exe}, words[1:]...)
	for _, runner := range reviewToolsTestRunners {
		if runner[0] != exe {
			continue
		}
		if isSubsequence(runner[1:], norm[1:]) {
			return bad("test runners are not permitted (%q): running tests is the worker's job, not Pruefer's", strings.Join(runner, " "))
		}
	}
	return rule, nil
}

// isSubsequence reports whether every word of want appears in have, in order.
func isSubsequence(want, have []string) bool {
	i := 0
	for _, w := range have {
		if i < len(want) && w == want[i] {
			i++
		}
	}
	return i == len(want)
}

// validateReviewTools validates and normalises an operator review_tools list,
// collapsing duplicates. The first invalid entry fails the whole list.
func validateReviewTools(rules []string) ([]string, error) {
	var out []string
	for _, r := range rules {
		n, err := validateReviewToolRule(r)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out, nil
}

// selectReviewTools applies a repo's review_tools selection to the operator
// ceiling. operator must already be validated and normalised. A nil repo list
// (the key was absent) leaves the full operator list in effect; a non-nil list
// selects the entries that are also in the operator list, in operator order,
// even when it is empty. Entries not in the ceiling are returned as ignored
// (normalised, de-duplicated): a repo can never widen the grant.
func selectReviewTools(operator []string, repo *[]string) (selected, ignored []string) {
	if repo == nil {
		return slices.Clone(operator), nil
	}
	requested := map[string]bool{}
	for _, r := range *repo {
		n := normalizeToolRule(r)
		if slices.Contains(operator, n) {
			requested[n] = true
		} else if !slices.Contains(ignored, n) {
			ignored = append(ignored, n)
		}
	}
	for _, o := range operator {
		if requested[o] {
			selected = append(selected, o)
		}
	}
	return selected, ignored
}

// Author associations GitHub reports for a PR author with write access.
var trustedAssociations = []string{"OWNER", "MEMBER", "COLLABORATOR"}

// dependabotLogin is the one bot whose PRs get the trusted grant (R4). It is
// compared exactly: no case folding, no prefix or suffix match.
const dependabotLogin = "dependabot[bot]"

// authorTrust is the trust decision for a PR author.
type authorTrust struct {
	Trusted bool
	// Association is the author_association as GitHub reported it, "" when
	// absent.
	Association string
	// Dependabot is true when the Dependabot exception (and nothing else) made
	// the author trusted.
	Dependabot bool
}

// trustDecision decides whether the PR author has write access. It fails closed:
// anything that is not positively one of OWNER, MEMBER or COLLABORATOR (exact,
// case-sensitive), or the exact Dependabot login, is outside — an empty,
// unrecognised or missing association included. Trust follows the PR author, not
// the head repository, so a trusted member's fork PR is trusted.
func trustDecision(login, association string) authorTrust {
	t := authorTrust{Association: association}
	if slices.Contains(trustedAssociations, association) {
		t.Trusted = true
		return t
	}
	if login == dependabotLogin {
		t.Trusted = true
		t.Dependabot = true
	}
	return t
}

// reviewGrant is the resolved per-review policy: the extra rules this review
// gets, whether operator plugin hooks and MCP servers are stripped, and the
// facts logged for the record. The zero value is today's behaviour: no extra
// rules and no stripping.
type reviewGrant struct {
	// Extras are the validated review_tools rules appended to the base grant.
	Extras []string
	// StripPluginHooks is true for an outside author: operator plugins are
	// loaded without their hooks and MCP servers.
	StripPluginHooks bool

	// Audit fields (R6).
	Trust            authorTrust
	Operator         []string // the ceiling
	RepoRequested    []string // what the repo's config listed, normalised
	RepoSpecified    bool     // the repo config carried a review_tools key
	RepoIgnored      []string // listed by the repo but outside the ceiling
	WouldHaveExtras  []string // what a trusted author would have been granted
	DowngradedExtras bool     // an outside author lost WouldHaveExtras
}

// resolveReviewGrant combines the operator ceiling, the repo's selection and
// the author decision. operator must be validated (Config.ReviewTools is).
func resolveReviewGrant(operator []string, repo *[]string, login, association string) reviewGrant {
	g := reviewGrant{Trust: trustDecision(login, association), Operator: slices.Clone(operator)}
	selected, ignored := selectReviewTools(operator, repo)
	if repo != nil {
		g.RepoSpecified = true
		for _, r := range *repo {
			if n := normalizeToolRule(r); !slices.Contains(g.RepoRequested, n) {
				g.RepoRequested = append(g.RepoRequested, n)
			}
		}
	}
	g.WouldHaveExtras = selected
	if g.Trust.Trusted {
		g.Extras = selected
		g.RepoIgnored = ignored
		return g
	}
	g.StripPluginHooks = true
	g.DowngradedExtras = len(selected) > 0
	return g
}

func logRules(rules []string) string {
	if len(rules) == 0 {
		return "[]"
	}
	parts := make([]string, len(rules))
	for i, r := range rules {
		parts[i] = sanitizeNoticeRunes(r, 120, "()*=@+%'\"")
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// grantLogLine is the single record of the resolved grant (R6/FR-011).
func (g reviewGrant) grantLogLine(login string) string {
	assoc := g.Trust.Association
	if assoc == "" {
		assoc = "unknown"
	}
	return fmt.Sprintf("review grant: base=read-only operator=%s repo_selected=%s repo_ignored=%s author=%s association=%s dependabot_exception=%t outside_author=%t downgraded=%t",
		logRules(g.Operator), logRules(g.Extras), logRules(g.RepoIgnored),
		sanitizeNoticeText(login), sanitizeNoticeText(assoc),
		g.Trust.Dependabot, !g.Trust.Trusted, g.DowngradedExtras)
}

// ignoredLogLine is the one line naming repo rules that were not applied, or ""
// when there are none. For an outside author the repo's rules have no effect at
// all, which is a different reason from being outside the operator ceiling.
func (g reviewGrant) ignoredLogLine() string {
	if !g.Trust.Trusted {
		if len(g.RepoRequested) == 0 {
			return ""
		}
		return fmt.Sprintf("review tools: repo rules %s not applied: outside author", logRules(g.RepoRequested))
	}
	if len(g.RepoIgnored) == 0 {
		return ""
	}
	return fmt.Sprintf("review tools: repo rules %s ignored: outside the operator's review_tools", logRules(g.RepoIgnored))
}

// toolRuleValid reports whether rule is already in the normalised form
// validateReviewToolRule produces and passes it. It is the structural check
// checkReviewArgsScopedTools applies to every extra rule it is told about.
func toolRuleValid(rule string) bool {
	n, err := validateReviewToolRule(rule)
	return err == nil && n == rule
}

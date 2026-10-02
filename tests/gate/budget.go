package gate

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// GraphQLBudgetRemaining is run.sh's graphql_budget_remaining: the
// caller-scoped token's actual remaining GraphQL budget, via GraphQL's own
// inline `rateLimit` field.
//
// It deliberately does NOT use the REST `rate_limit` endpoint, which reports a
// permanently full bucket for the bed's token (measured 2026-09-06: inline
// rateLimit 4248 remaining while REST said 5000 — the REST figure never moved,
// so every "5000 -> 5000 (consumed 0 pts)" line the script ever printed before
// that was a dead gauge). The inline query costs 1 point per call (2 per leg).
//
// The token is scoped via GH_TOKEN, never whatever identity the ambient `gh`
// happens to be logged in as. The call is bounded by Cfg.GHAPITimeout and its
// whole session is killed past it — THE REQUIRED ROUTING POINT for any network
// call the gate adds (#1676: the v0.0.81 cut hung 17 hours in an unbounded
// probe). A failure or timeout is never a gate: the caller degrades to a
// skipped report (R4, #1676).
//
// stderr carries gh's own error text and, when the deadline was enforced,
// with_timeout's "command exceeded Ns, killed" diagnostic, so a caught hang is
// distinguishable from an ordinary gh error in the log.
func (g *Gate) GraphQLBudgetRemaining(ctx context.Context, token string) (remaining int, stderr string, err error) {
	var so, se syncBuf
	c := Cmd{
		Name: "gh", Args: []string{"api", "graphql", "-f", "query=query { rateLimit { remaining } }", "--jq", ".data.rateLimit.remaining"},
		Env:    withEnv(g.Env, "GH_TOKEN="+token),
		Stdout: &so, Stderr: &se,
		Session: true, Timeout: g.Cfg.GHAPITimeout, Grace: g.Cfg.KillGrace,
	}
	res := g.Exec.Run(ctx, c)
	stderr = se.String()
	if res.TimedOut {
		stderr += fmt.Sprintf("with_timeout: command exceeded %ds, killed: %s %s\n", int(g.Cfg.GHAPITimeout.Seconds()), c.Name, strings.Join(c.Args, " "))
		return 0, stderr, fmt.Errorf("timed out")
	}
	if res.ExitCode != 0 || res.Err != nil {
		return 0, stderr, fmt.Errorf("gh exited %d", res.ExitCode)
	}
	n, perr := strconv.Atoi(strings.TrimSpace(so.String()))
	if perr != nil {
		return 0, stderr, fmt.Errorf("unparseable budget %q", strings.TrimSpace(so.String()))
	}
	return n, stderr, nil
}

// reportProbeFailure is run.sh's _report_gh_probe_failure: relay a failed
// probe's captured stderr as warnings, one line at a time — including the
// timeout diagnostic. Visibility only: it never touches the leg's exit code.
func (g *Gate) reportProbeFailure(stderr, label string) {
	if stderr == "" {
		return
	}
	for _, line := range strings.Split(strings.TrimRight(stderr, "\n"), "\n") {
		g.errf("warning: %s probe: %s\n", label, line)
	}
}

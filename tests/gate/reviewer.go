package gate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The reviewer-token budget guard (#2536, ADR-2536).
//
// FABRIK_REVIEWER_TOKEN is the second, non-author identity the live tests use to
// request reviews, submit verdicts and post "human" comments. It belongs to a
// personal account whose budget other local sessions share. On the 0.0.91 gate its
// REST core budget was already 0/5000 when two legs started, every test touching
// it failed on its first call, and the four cells behind the failed leg never
// started: 65 of 366 pairs after 1.5 hours of live spend.
//
// The reviewer is deliberately NOT a member of identitySet: that set drives the
// scheduler's exclusive/shared acquisition and the budget floor, and the reviewer
// has no engine or harness role. This is a read-only facility beside it:
//
//   - CheckReviewerBudget: a live preflight, ahead of every other live read, that
//     refuses (ExitPreconditionFailed) a gate that cannot succeed;
//   - reviewerCellGate: a check before each cell starts that holds the cell while
//     the budget resets soon and otherwise declines to start it, before anything
//     is recorded, so the cell's pairs stay missing and --resume picks them up;
//   - logReviewerBudget: the reviewer's REST core and GraphQL budget at each leg's
//     start and end.
//
// Every read goes through IdentityOps (the bounded, token-scoped `gh` path). A
// read that cannot be completed is "unavailable" and never blocks (#1676).

// Names of the knobs, for messages.
const (
	envReviewerMinCore    = "E2E_REVIEWER_MIN_CORE"
	envReviewerMinGraphQL = "E2E_REVIEWER_MIN_GRAPHQL"
	envReviewerHoldMax    = "E2E_REVIEWER_HOLD_MAX"
)

// reviewerBucket is one rate-limit bucket of the reviewer account.
type reviewerBucket struct {
	Remaining int
	Limit     int    // 0: not reported (an exhausted GraphQL bucket answers with an error)
	Reset     string // RFC3339, "" when unknown
	// Err is why the bucket could not be read; a bucket with an Err is
	// "unavailable" and never counts as short.
	Err error
}

func (b reviewerBucket) figures() string {
	if b.Err != nil {
		return "unavailable"
	}
	lim := "?"
	if b.Limit > 0 {
		lim = fmt.Sprint(b.Limit)
	}
	return fmt.Sprintf("%d/%s resets %s", b.Remaining, lim, orUnknown(b.Reset))
}

// reviewerReading is one read of the reviewer account.
type reviewerReading struct {
	Login    string // "" when it could not be resolved
	LoginErr string
	Core     reviewerBucket
	GraphQL  reviewerBucket
}

// reviewerShort is a bucket below its minimum.
type reviewerShort struct {
	Bucket string // "REST core" or "GraphQL"
	Env    string // the override variable
	Min    int
	B      reviewerBucket
}

// redact keeps a token out of any text derived from a failed call.
func redact(token string, err error) error {
	if err == nil || token == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), token, "<redacted>"))
}

// classifyRateLimitErr reports whether a failed budget read failed BECAUSE the
// bucket is exhausted: GitHub answers the inline GraphQL rateLimit query of an
// exhausted bucket with a rate-limit error rather than "remaining: 0". Matching is
// deliberately narrow and case-insensitive; any other failure (a network error, a
// 502, a bad credential) is "unavailable", not exhausted.
func classifyRateLimitErr(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "rate limit exceeded") ||
		strings.Contains(m, "rate_limited") ||
		strings.Contains(m, "secondary rate limit")
}

// readReviewerBudget reads the reviewer account's REST core and GraphQL budgets,
// then — best effort, last, because an exhausted token cannot answer GET /user —
// its login. Nothing here blocks beyond the bounded call path.
func (g *Gate) readReviewerBudget(ctx context.Context, token string) reviewerReading {
	var rd reviewerReading
	id := g.root().Identity
	rem, lim, reset, err := id.CoreBudget(ctx, token)
	if err != nil {
		rd.Core.Err = redact(token, err)
	} else {
		rd.Core = reviewerBucket{Remaining: rem, Limit: lim, Reset: reset}
	}
	rem, lim, reset, err = id.Budget(ctx, token)
	switch {
	case err == nil:
		rd.GraphQL = reviewerBucket{Remaining: rem, Limit: lim, Reset: reset}
	case classifyRateLimitErr(err):
		// Exhausted: the read itself was refused. Remaining 0, reset unknown.
		rd.GraphQL = reviewerBucket{Remaining: 0}
	default:
		rd.GraphQL.Err = redact(token, err)
	}
	if ctx.Err() == nil {
		if login, lerr := g.resolveLogin(ctx, token); lerr == nil {
			rd.Login = login
		} else {
			rd.LoginErr = redact(token, lerr).Error()
		}
	}
	return rd
}

// shortfalls are the buckets of rd below their minimum. An unavailable bucket and
// a bucket whose minimum is 0 (disabled) are never short.
func (g *Gate) shortfalls(rd reviewerReading) []reviewerShort {
	var out []reviewerShort
	if m := g.Cfg.ReviewerMinCore; m > 0 && rd.Core.Err == nil && rd.Core.Remaining < m {
		out = append(out, reviewerShort{"REST core", envReviewerMinCore, m, rd.Core})
	}
	if m := g.Cfg.ReviewerMinGraphQL; m > 0 && rd.GraphQL.Err == nil && rd.GraphQL.Remaining < m {
		out = append(out, reviewerShort{"GraphQL", envReviewerMinGraphQL, m, rd.GraphQL})
	}
	return out
}

// reviewerGuardOff reports whether the guard does nothing for this gate: the
// non-vacuity seam, no identity ops, or both buckets' minimums at 0.
func (g *Gate) reviewerGuardOff() bool {
	return g.root().reviewerBudgetCheckDisabledForTest || g.root().Identity == nil ||
		(g.Cfg.ReviewerMinCore <= 0 && g.Cfg.ReviewerMinGraphQL <= 0)
}

// who names the reviewer account in a message: its login when known, else the bed
// and variable (an exhausted token cannot resolve its own login).
func (rd reviewerReading) who(bed string) string {
	if rd.Login != "" {
		return rd.Login
	}
	s := bed + "'s FABRIK_REVIEWER_TOKEN (login unavailable"
	if rd.LoginErr != "" {
		s += ": " + rd.LoginErr
	}
	return s + ")"
}

// reviewerRef is one distinct reviewer token and the beds that carry it.
type reviewerRef struct {
	token string
	beds  []string // bed labels
}

// reviewerTokens is every distinct reviewer token across the beds, in bed order:
// beds sharing a token are read once (FR-004). A bed with none contributes nothing.
func (g *Gate) reviewerTokens() []reviewerRef {
	var out []reviewerRef
	idx := map[string]int{}
	for _, b := range g.beds() {
		t := b.Cfg.ReviewerToken
		if t == "" {
			continue
		}
		if i, ok := idx[t]; ok {
			out[i].beds = append(out[i].beds, b.bedLabel())
			continue
		}
		idx[t] = len(out)
		out = append(out, reviewerRef{token: t, beds: []string{b.bedLabel()}})
	}
	return out
}

// CheckReviewerBudget is R1: before any other live read spends, refuse
// (ExitPreconditionFailed) when a bed's reviewer token has less REST core or
// GraphQL budget than its minimum. An unreadable probe warns and proceeds.
func (g *Gate) CheckReviewerBudget(ctx context.Context) error {
	if g.reviewerGuardOff() {
		return nil
	}
	refs := g.reviewerTokens()
	if len(refs) == 0 {
		return nil
	}
	var problems []string
	for _, ref := range refs {
		rd := g.readReviewerBudget(ctx, ref.token)
		bed := strings.Join(ref.beds, " and ")
		for _, b := range []struct {
			name string
			bk   reviewerBucket
			min  int
		}{{"REST core", rd.Core, g.Cfg.ReviewerMinCore}, {"GraphQL", rd.GraphQL, g.Cfg.ReviewerMinGraphQL}} {
			if b.min > 0 && b.bk.Err != nil {
				g.errf("warning: reviewer budget: cannot read %s's %s budget (%v) — proceeding without that check (unavailable, #2536)\n", rd.who(bed), b.name, b.bk.Err)
			}
		}
		short := g.shortfalls(rd)
		if len(short) == 0 {
			g.outf("== reviewer budget ok: %s — core %s; graphql %s ==\n", rd.who(bed), rd.Core.figures(), rd.GraphQL.figures())
			continue
		}
		for _, s := range short {
			problems = append(problems, fmt.Sprintf("%s (%s): %s budget %s — below the minimum of %d (%s)",
				rd.who(bed), bed, s.Bucket, s.B.figures(), s.Min, s.Env))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	var sb strings.Builder
	sb.WriteString("\n############################################################\n")
	sb.WriteString("## PRECONDITION FAILED: FABRIK_REVIEWER_TOKEN's rate-limit budget is\n")
	sb.WriteString("## too low for a gate run (#2536).\n")
	sb.WriteString("##\n")
	for _, p := range problems {
		fmt.Fprintf(&sb, "##   %s\n", p)
	}
	sb.WriteString("##\n")
	sb.WriteString("## Every live test that uses the reviewer identity fails on its first\n")
	sb.WriteString("## call once the budget is gone, and the legs behind it never start.\n")
	sb.WriteString("## Wait for the reset above, stop whatever else uses that account\n")
	sb.WriteString("## (or give the reviewer a dedicated account), then re-run. The minimums\n")
	fmt.Fprintf(&sb, "## are %s and %s (0 disables a bucket).\n", envReviewerMinCore, envReviewerMinGraphQL)
	sb.WriteString("############################################################")
	return &ExitError{Code: ExitPreconditionFailed, Msg: sb.String()}
}

// ReviewerBudgetError is a cell that was NOT started because the reviewer token's
// budget is below its minimum and does not recover within the bounded wait. It is
// the existing "budget exhausted" outcome (ExitBudgetExhausted) — but nothing ran,
// so nothing is recorded or voided and the cell's pairs stay missing — and it
// carries its own type so the multi-bed scheduler does not mistake it for the
// ENGINE identity's budget being spent.
type ReviewerBudgetError struct{ *ExitError }

func (e *ReviewerBudgetError) Unwrap() error { return e.ExitError }

func reviewerBudgetErr(format string, args ...any) *ReviewerBudgetError {
	return &ReviewerBudgetError{exitErr(ExitBudgetExhausted, format, args...)}
}

// reviewerCellGate is R2: called by Gate.runLeg before a cell starts anything (no
// invocation noted, no bed restart, no ledger record). It re-reads this bed's
// reviewer token. A healthy or unreadable budget lets the cell start. A budget
// below its minimum that resets within ReviewerHoldMax holds the cell — logged,
// re-checked every ProbeInterval — and one that does not (no reset, a reset
// already past, or still short at the end of the wait) refuses it with a
// *ReviewerBudgetError. A running leg is never touched: this runs before one.
func (g *Gate) reviewerCellGate(ctx context.Context, c Cell) error {
	token := g.Cfg.ReviewerToken
	if token == "" || g.reviewerGuardOff() {
		return nil
	}
	bed := g.bedLabel()
	deadline := g.Now().Add(g.Cfg.ReviewerHoldMax)
	held := false
	for {
		rd := g.readReviewerBudget(ctx, token)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		for _, b := range []reviewerBucket{rd.Core, rd.GraphQL} {
			if b.Err != nil {
				g.errf("warning: reviewer budget (cell %s): a read failed (%v) — unavailable, not blocking (#2536)\n", c.Label(), b.Err)
				break
			}
		}
		short := g.shortfalls(rd)
		if len(short) == 0 {
			if held {
				g.outf("== reviewer budget recovered (core %s; graphql %s) — starting %s ==\n", rd.Core.figures(), rd.GraphQL.figures(), c.Label())
			}
			return nil
		}
		desc := make([]string, len(short))
		for i, s := range short {
			desc[i] = fmt.Sprintf("%s %s (minimum %d, %s)", s.Bucket, s.B.figures(), s.Min, s.Env)
		}
		// The cell can start only once EVERY short bucket has reset.
		var latest time.Time
		recovers := true
		for _, s := range short {
			t, err := time.Parse(time.RFC3339, s.B.Reset)
			if err != nil || !t.After(g.Now()) {
				recovers = false // no reset, or one already past while still short
				break
			}
			if t.After(latest) {
				latest = t
			}
		}
		if !recovers || latest.After(deadline) || !g.Now().Before(deadline) {
			return reviewerBudgetErr(
				"reviewer token budget exhausted: %s — %s; not starting cell %s (it does not reset within the %ds bound, %s). Nothing was run or recorded: its pairs stay missing — re-run with --resume after the reset (#2536)",
				rd.who(bed), strings.Join(desc, "; "), c.Label(), int(g.Cfg.ReviewerHoldMax.Seconds()), envReviewerHoldMax)
		}
		held = true
		g.outf("== holding %s: reviewer %s %s; re-checking in %ds (bound %ds) ==\n",
			c.Label(), rd.who(bed), strings.Join(desc, "; "), int(g.probeInterval().Seconds()), int(g.Cfg.ReviewerHoldMax.Seconds()))
		g.Sleep(g.probeInterval())
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

// probeInterval is the pause between re-checks, 30s when the config sets none.
func (g *Gate) probeInterval() time.Duration {
	if g.Cfg.ProbeInterval > 0 {
		return g.Cfg.ProbeInterval
	}
	return 30 * time.Second
}

// logReviewerBudget is R3: the reviewer account's REST core and GraphQL budget at
// a leg's start and end, so the archive shows what the leg spent and what someone
// else spent meanwhile. A failed read warns and never gates; no token text reaches
// the output.
func (g *Gate) logReviewerBudget(ctx context.Context, label, which string) {
	token := g.Cfg.ReviewerToken
	if token == "" || g.root().reviewerBudgetCheckDisabledForTest || g.root().Identity == nil {
		return
	}
	if ctx.Err() != nil {
		g.errf("warning: reviewer budget (leg: %s, %s): probes stopped\n", label, which)
		return
	}
	rd := g.readReviewerBudget(ctx, token)
	for _, b := range []struct {
		name string
		bk   reviewerBucket
	}{{"core", rd.Core}, {"graphql", rd.GraphQL}} {
		if b.bk.Err != nil {
			g.errf("warning: reviewer budget (leg: %s, %s): reading %s failed: %v\n", label, which, b.name, b.bk.Err)
		}
	}
	g.outf("== reviewer budget (leg: %s, %s): %s — core %s; graphql %s ==\n", label, which, rd.who(g.bedLabel()), rd.Core.figures(), rd.GraphQL.figures())
}

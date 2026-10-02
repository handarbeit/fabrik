package gate

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// SkipKind is how the coverage summary treats a recorded SKIP (R6).
type SkipKind int

const (
	// SkipMissing: the test did not run, for no reason the ledger accepts. It
	// blocks the gate exactly like a test with no record.
	SkipMissing SkipKind = iota
	// SkipKnown: the skip cites an issue that is still OPEN. Listed separately;
	// does not block the gate and is never counted as covered.
	SkipKnown
	// SkipStructural: the registry declares (skip_ok_legs) that this test
	// self-skips on this leg by design — a merge-train scenario under train
	// "off". Listed separately; does not block and is not counted as covered.
	SkipStructural
)

func (k SkipKind) String() string {
	switch k {
	case SkipKnown:
		return "known"
	case SkipStructural:
		return "structural"
	}
	return "missing"
}

// SkipClass is the verdict on one recorded skip.
type SkipClass struct {
	Kind   SkipKind
	Reason string // why it is missing, or what it is known/structural because of
}

// IssueStates resolves a cited issue number to "OPEN" or "CLOSED". The real
// implementation shells out to gh through the Commander; tests substitute a map.
type IssueStates interface {
	State(ctx context.Context, n int) (string, error)
}

// ghIssueStates reads issue state with `gh issue view`, bounded by the gate's
// GHAPITimeout and cached per run (the summary asks about the same issue many
// times). It is NEVER called from the post-suite tail (which has a watchdog); it
// runs only when a summary is rendered, off the leg's critical path.
type ghIssueStates struct {
	g     *Gate
	cache map[int]issueState
}

type issueState struct {
	state string
	err   error
}

func (g *Gate) newIssueStates() IssueStates {
	return &ghIssueStates{g: g, cache: map[int]issueState{}}
}

func (s *ghIssueStates) State(ctx context.Context, n int) (string, error) {
	if c, ok := s.cache[n]; ok {
		return c.state, c.err
	}
	var so, se syncBuf
	repo := s.g.Cfg.IssueRepo
	if repo == "" {
		repo = "handarbeit/fabrik"
	}
	res := s.g.Exec.Run(ctx, Cmd{
		Name: "gh", Args: []string{"issue", "view", strconv.Itoa(n), "--repo", repo, "--json", "state", "--jq", ".state"},
		Env: s.g.Env, Stdout: &so, Stderr: &se,
		Session: true, Timeout: s.g.Cfg.GHAPITimeout, Grace: s.g.Cfg.KillGrace,
	})
	var c issueState
	switch {
	case res.TimedOut:
		c.err = fmt.Errorf("gh issue view #%d timed out after %ds", n, int(s.g.Cfg.GHAPITimeout.Seconds()))
	case res.ExitCode != 0 || res.Err != nil:
		c.err = fmt.Errorf("gh issue view #%d exited %d: %s", n, res.ExitCode, strings.TrimSpace(se.String()))
	default:
		c.state = strings.ToUpper(strings.TrimSpace(so.String()))
		if c.state != "OPEN" && c.state != "CLOSED" {
			c.err = fmt.Errorf("gh issue view #%d: unexpected state %q", n, c.state)
			c.state = ""
		}
	}
	s.cache[n] = c
	return c.state, c.err
}

// ClassifySkip applies R6 to one recorded SKIP:
//
//   - a registry-declared structural skip on this leg is SkipStructural;
//   - a skip citing no issue is SkipMissing (it is not a declared skip);
//   - a skip citing issues is SkipKnown iff AT LEAST ONE cited issue is OPEN;
//   - all cited issues closed, or the state of every non-closed one unreadable
//     (no gh access), is SkipMissing — never a silent known skip.
func ClassifySkip(ctx context.Context, rec Record, structural bool, states IssueStates) SkipClass {
	if structural {
		return SkipClass{Kind: SkipStructural, Reason: "registry skip_ok_legs"}
	}
	if len(rec.Issues) == 0 {
		return SkipClass{Kind: SkipMissing, Reason: "skip cites no issue (not a declared skip)"}
	}
	var closed, unknown []string
	for _, n := range rec.Issues {
		st, err := states.State(ctx, n)
		switch {
		case err != nil || st == "":
			unknown = append(unknown, fmt.Sprintf("#%d", n))
		case st == "OPEN":
			return SkipClass{Kind: SkipKnown, Reason: fmt.Sprintf("blocked on open #%d", n)}
		default:
			closed = append(closed, fmt.Sprintf("#%d", n))
		}
	}
	if len(unknown) > 0 {
		return SkipClass{Kind: SkipMissing, Reason: "cited issue state unknown (" + strings.Join(unknown, ", ") + ") — treated as missing"}
	}
	return SkipClass{Kind: SkipMissing, Reason: "cited issue closed (" + strings.Join(closed, ", ") + ") — the skip is stale"}
}

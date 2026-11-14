package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/handarbeit/fabrik/internal/closingrefs"
)

// commitCap is the most commits GET /pulls/{n}/commits will ever return. A result of exactly
// this size may be truncated, so the check fails closed instead of scanning a partial list.
const commitCap = 250

// PRSource reads the text GitHub would scan for closing keywords.
type PRSource interface {
	// Title returns the live PR title: it becomes the subject of a merge or squash commit.
	Title(ctx context.Context) (string, error)
	// Body returns the live PR description.
	Body(ctx context.Context) (string, error)
	// Commits returns every commit message on the PR, in order.
	Commits(ctx context.Context) ([]closingrefs.Commit, error)
}

// BoardReader answers whether an issue is a member of the community-report board.
type BoardReader interface {
	// Verify proves the credential can see the board at all. A token that cannot see a
	// Projects v2 board gets an empty (not an erroring) project list back, which would pass
	// every PR, so OnBoard is only trusted after Verify succeeds.
	Verify(ctx context.Context) error
	// OnBoard reports whether issue repo#number (repo is "owner/repo") is on the board.
	// An issue that does not exist, or is a pull request, is not on the board.
	OnBoard(ctx context.Context, repo string, number int) (bool, error)
}

type config struct {
	Repo        string // owner/repo of the PR
	BoardLabel  string // human name of the board, e.g. "board #3"
	Secrets     []string
	Annotate    bool // emit a GitHub ::error annotation on failure
	CodeExempt  bool
	CommitLimit int // defaults to commitCap
}

// Exit codes.
const (
	exitOK   = 0
	exitFail = 1
)

// run executes the check and returns the process exit code. out receives the log; summary
// (may be nil) receives a Markdown copy of a failure for the step summary. Secrets are
// redacted from everything written.
func run(ctx context.Context, cfg config, src PRSource, board BoardReader, out, summary io.Writer) int {
	var buf, sum bytes.Buffer
	code := runInner(ctx, cfg, src, board, &buf, &sum)
	redactTo(out, buf.String(), cfg.Secrets)
	if summary != nil && sum.Len() > 0 {
		redactTo(summary, sum.String(), cfg.Secrets)
	}
	return code
}

func redactTo(w io.Writer, s string, secrets []string) {
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "***")
		}
	}
	_, _ = io.WriteString(w, s)
}

func runInner(ctx context.Context, cfg config, src PRSource, board BoardReader, out, sum *bytes.Buffer) int {
	if cfg.BoardLabel == "" {
		cfg.BoardLabel = "the community board"
	}
	limit := cfg.CommitLimit
	if limit == 0 {
		limit = commitCap
	}
	unverified := func(why string) int {
		msg := fmt.Sprintf("could not verify %s: %s", cfg.BoardLabel, why)
		fail(cfg, out, sum, "Could not verify closing references", msg, msg+"\n")
		return exitFail
	}

	title, err := src.Title(ctx)
	if err != nil {
		return unverified("cannot read the PR: " + err.Error())
	}
	body, err := src.Body(ctx)
	if err != nil {
		return unverified("cannot read the PR: " + err.Error())
	}
	commits, err := src.Commits(ctx)
	if err != nil {
		return unverified("cannot read the PR commits: " + err.Error())
	}
	if len(commits) >= limit {
		return unverified(fmt.Sprintf("the PR has %d or more commits, which is the most GitHub's API returns, so the list may be incomplete", limit))
	}

	texts := []closingrefs.Text{{Where: "PR title", Body: title}, {Where: "PR body", Body: body}}
	for _, c := range commits {
		texts = append(texts, closingrefs.Text{Where: "commit " + c.SHA, Body: c.Message})
	}
	refs := closingrefs.Sorted(closingrefs.FindTexts(cfg.Repo, closingrefs.Options{ExemptCode: cfg.CodeExempt}, texts...))
	if len(refs) == 0 {
		fmt.Fprintf(out, "OK: no closing keyword in the PR title, body or its %d commit message(s); %s was not consulted.\n", len(commits), cfg.BoardLabel)
		return exitOK
	}

	if err := board.Verify(ctx); err != nil {
		return unverified(err.Error())
	}

	var offenders []closingrefs.Ref
	for _, r := range refs {
		on, err := board.OnBoard(ctx, r.Repo, r.Number)
		if err != nil {
			return unverified(fmt.Sprintf("reading membership of %s: %v", r, err))
		}
		if on {
			offenders = append(offenders, r)
		} else {
			fmt.Fprintf(out, "ok: %s would be closed by this PR and is not on %s.\n", r, cfg.BoardLabel)
		}
	}
	if len(offenders) == 0 {
		fmt.Fprintf(out, "OK: %d issue(s) would be closed and none is on %s.\n", len(refs), cfg.BoardLabel)
		return exitOK
	}

	var b strings.Builder
	fmt.Fprintf(&b, "This PR would close %d community report(s) on %s when it merges:\n", len(offenders), cfg.BoardLabel)
	for _, r := range offenders {
		fmt.Fprintf(&b, "\n  %s\n", r)
		for _, l := range r.Locations {
			fmt.Fprintf(&b, "    - %q in %s\n", l.Phrase, l.Where)
		}
	}
	b.WriteString("\nGitHub matches a closing keyword followed by an issue reference by word adjacency, so a sentence\n" +
		"that says the PR does NOT close the issue still closes it. Community reports are closed by hand after a\n" +
		"release containing the fix ships.\n" +
		"\nReword so no closing keyword (close/closes/closed, fix/fixes/fixed, resolve/resolves/resolved) sits next\n" +
		"to the number. Safe wordings:\n" +
		"  - mentioned for context: #N\n" +
		"  - #N is out of scope here\n" +
		"If the keyword is in a commit message, the commit has to be reworded (or squashed away) and force-pushed.\n")
	msg := b.String()
	fail(cfg, out, sum, "PR would close a community report", msg, "### "+"PR would close a community report\n\n```\n"+msg+"```\n")
	return exitFail
}

func fail(cfg config, out, sum *bytes.Buffer, title, msg, summary string) {
	if cfg.Annotate {
		fmt.Fprintf(out, "::error title=%s::%s\n", escapeProp(title), escapeData(msg))
	}
	fmt.Fprintf(out, "FAIL: %s\n", msg)
	sum.WriteString(summary)
}

// Workflow-command escaping (https://docs.github.com/actions/reference/workflow-commands-for-github-actions).
func escapeData(s string) string {
	s = strings.ReplaceAll(s, "%", "%25")
	s = strings.ReplaceAll(s, "\r", "%0D")
	return strings.ReplaceAll(s, "\n", "%0A")
}

func escapeProp(s string) string {
	s = escapeData(s)
	s = strings.ReplaceAll(s, ":", "%3A")
	return strings.ReplaceAll(s, ",", "%2C")
}

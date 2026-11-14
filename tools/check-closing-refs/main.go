// Command check-closing-refs fails a pull request that would auto-close an issue on the
// community-report board when it merges (#2576, ADR 2576).
//
// It reads the PR description and every commit message through the GitHub API, finds the
// issues they would close (internal/closingrefs), and fails if any is on the board. If the
// board cannot be read it fails too, saying it could not verify. It never checks out or
// executes PR content: the workflow runs it from the default branch under pull_request_target.
//
// Environment: REPO (owner/repo), PR_NUMBER, GITHUB_TOKEN (reads the PR), BOARD_TOKEN
// (reads the org Projects v2 board), optional BOARD_OWNER (default handarbeit) and
// BOARD_NUMBER (default 3), GITHUB_API_URL, GITHUB_GRAPHQL_URL, GITHUB_STEP_SUMMARY.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"
)

const (
	defaultBoardOwner  = "handarbeit"
	defaultBoardNumber = 3
)

func main() {
	os.Exit(realMain(os.Getenv, os.Stdout))
}

func realMain(getenv func(string) string, stdout io.Writer) int {
	repo := getenv("REPO")
	pr, err := strconv.Atoi(getenv("PR_NUMBER"))
	if repo == "" || err != nil || pr <= 0 {
		fmt.Fprintln(stdout, "check-closing-refs: REPO and a numeric PR_NUMBER are required")
		return 2
	}
	owner := getenv("BOARD_OWNER")
	if owner == "" {
		owner = defaultBoardOwner
	}
	num := defaultBoardNumber
	if s := getenv("BOARD_NUMBER"); s != "" {
		if num, err = strconv.Atoi(s); err != nil || num <= 0 {
			fmt.Fprintln(stdout, "check-closing-refs: BOARD_NUMBER must be a positive integer")
			return 2
		}
	}
	boardToken, restToken := getenv("BOARD_TOKEN"), getenv("GITHUB_TOKEN")
	cl := newGHClient(getenv("GITHUB_API_URL"), getenv("GITHUB_GRAPHQL_URL"), restToken, boardToken, repo, pr, owner, num)

	var summary io.Writer
	if p := getenv("GITHUB_STEP_SUMMARY"); p != "" {
		if f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600); err == nil {
			defer func() {
				// The step summary is optional, so a failed close is reported, not fatal.
				if err := f.Close(); err != nil {
					fmt.Fprintf(stdout, "check-closing-refs: warning: could not close the step summary: %v\n", err)
				}
			}()
			summary = f
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cfg := config{
		Repo:       repo,
		BoardLabel: fmt.Sprintf("board #%d", num),
		Secrets:    []string{boardToken, restToken},
		Annotate:   getenv("GITHUB_ACTIONS") == "true",
	}
	return run(ctx, cfg, cl, cl, stdout, summary)
}

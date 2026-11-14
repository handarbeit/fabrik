# ADR 2576: CI guard against a PR that would auto-close a community report

## Status

Accepted.

## Context

GitHub closes an issue when a PR whose body, or a commit message, puts a closing keyword
(`close`, `fix`, `resolve` and their forms) next to the issue's number merges. It matches word
adjacency, not grammar, so a sentence that says the PR does *not* close the issue still closes
it. Community reports on the public triage board (project #3) are closed by hand after a release
containing the fix ships (CLAUDE.md, "Handling Community Bug Reports"), but only spec wording
enforced that, and wording failed: PR #2556 carried a sentence disclaiming a close of report 2497
and its merge on 2026-11-06 closed the report, with no comment and before any release. Spec
wording is not enough; the guard has to be mechanical.

ADR 1962 records that GitHub's `Closes #N` auto-close silently stopped working on 2026-09-30 and
the engine backstops it. The 2556 incident shows the behaviour is at least intermittent, so this
check assumes nothing about whether GitHub currently applies keywords.

## Decision

1. **A pure matcher, `internal/closingrefs`.** `Find(body, commits, selfRepo, opts)` returns one
   `Ref{Repo, Number, Locations}` per issue. It recognises the nine keywords case-insensitively,
   separated from the target by whitespace (including newlines), a colon and Markdown emphasis or
   link openers, with a target of `#N`, `owner/repo#N` or an issue URL. Negation is irrelevant by
   construction. A keyword with no number (the PR template's bare `Closes #`) never matches;
   only the first target after a keyword matches (`Closes #1, #2` yields #1, as GitHub requires a
   keyword per issue); `fix(engine): ... (#2498)` never matches. The matcher is biased to
   over-match: a missed match reopens the incident, an extra one costs a reworded sentence.
   `deps_test.go` pins it free of network and process imports.
2. **A runner, `tools/check-closing-refs`,** reads the live PR body and every commit message
   through the API (stdlib `net/http`), calls the matcher, and resolves board membership behind a
   `BoardReader` interface, so every test runs offline.
3. **Board membership is the only signal** (not author, not `author_association`): an issue is a
   community report iff it is an item of project #3 of `handarbeit`, read live by GraphQL
   `projectItems`. Identity is `(owner/repo, number)`, and foreign-repo targets are checked too.
4. **Fail closed.** If the board cannot be read the check fails with
   `could not verify board #3: <reason>`. `projectItems` silently omits projects the token cannot
   see, so a token without project access would return an empty list and pass every PR; the runner
   therefore first proves visibility of the board itself (`organization.projectV2` must be
   non-null) and a `NOT_FOUND` on the *issue* inside a readable repository is the only error read as
   "not a report". A `NOT_FOUND` on the *repository* fails closed: GraphQL answers it identically
   for a repository the token cannot read, which could hold a board #3 issue. `hasNextPage` on an
   issue's projects, a missing token, a PR read error and a commit list at the API's 250-commit
   cap also fail closed. With no closing reference at all the check passes without consulting the
   board.
5. **Trigger: `pull_request_target`** (`opened`, `edited`, `synchronize`, `reopened`) plus a
   manual `workflow_dispatch(pr_number)`. Under `pull_request` a fork PR gets no secrets and could
   only fail closed. It is safe only because the workflow checks out the default branch, never the
   PR head, executes no PR code, and reads PR text only through the API in the tool, never
   interpolated into a shell string or expression. A consequence: it does not run on the PR that
   adds or edits it, so `workflow_dispatch` is the validation path after merge.
6. **Token: `CLOSING_REFS_BOARD_TOKEN || ADD_TO_PROJECT_PAT`.** `GITHUB_TOKEN` cannot read an
   org Projects v2 board, so a PAT is needed. The existing `ADD_TO_PROJECT_PAT` is reused so the
   check works at once, but it has write scope; a fine-grained token with organization Projects
   read-only stored as `CLOSING_REFS_BOARD_TOKEN` is preferred and takes precedence
   automatically. The token reaches only the step that runs the trusted tool, as an environment
   variable, and the tool redacts it from everything it prints.
7. **Every PR, whatever its base.** GitHub applies keywords only to default-branch merges, but a
   base can be retargeted, and the requirement is every PR.
8. **No exemption for an issue that is on both board #3 and project #1.** CLAUDE.md says a report
   is never pipelined directly; work belongs on a separate work issue. A failing `Closes` line on
   such an issue is the intended signal. Ordinary work issues sit on project #1 only, so Fabrik's
   `Closes #<work-issue>` lines, including merge-train integration and catch-up PRs (whose commit
   scan covers the member commits), pass; tests assert this.
9. **Code spans and fenced blocks are matches until verified.** GitHub's documentation is silent
   on whether it ignores keywords inside code, and a safe probe needs a merge in a scratch
   repository. `Options.ExemptCode` exists, off by default and unused by the tool. The fixtures
   assert match behaviour, marked `UNVERIFIED against GitHub`, and
   `internal/closingrefs/testdata/README.md` holds the probe recipe and the PENDING status.

## Consequences

- The workflow ships as `tools/check-closing-refs/closing-refs.workflow.yml`: the Fabrik GitHub App cannot push
  under `.github/workflows/` (no `workflows` permission). A maintainer copies it to
  `.github/workflows/closing-refs.yml` (a plain `cp` and commit); nothing else changes.

- A PR that would close a community report fails with the issue, the matched phrase, where it was
  found (PR body or commit SHA) and the safe wordings `mentioned for context: #N` and
  `#N is out of scope here`.
- A maintainer must mark the check "Guard community reports" required in branch protection (a
  repository setting) and, ideally, add the read-only `CLOSING_REFS_BOARD_TOKEN` secret.
- After merge, validate with `workflow_dispatch` against one PR that should fail and one clean
  PR.
- A commit-message match requires rewording and force-pushing the commit (or squashing it away).
- Rewriting existing PR bodies and automatically reopening a report that was already closed are
  out of scope.

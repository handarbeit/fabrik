# github/testdata — wire-contract schema validation and recorded fixtures

This directory backs the wire-contract test layer added by #1453
(`adrs/1453-wire-contract-graphql-schema-validation.md`). It has two
independent halves that share only this directory and this README:

- **`schema/`** — a vendored copy of GitHub's real GraphQL SDL schema, used
  by `github/wire_contract_test.go` to statically validate every GraphQL
  query/mutation string `github/` sends (R1).
- **`recordings/`** — real, provenance-tagged GitHub responses that
  `httptest`-based tests serve in place of hand-authored JSON literals (R2).

Neither half fixes any API misuse it might uncover — see "Findings" below
and the PR that introduced this directory for anything filed as a result.

## Schema validation (`schema/`)

`schema/github.graphql` is GitHub's publicly documented GraphQL SDL for the
free/pro/team tier, downloaded from
`https://docs.github.com/public/fpt/schema.docs.graphql` (no auth required).
`schema/SCHEMA_META.json` records `source_url` and `fetched_at`.

`github/wire_contract_test.go` loads this schema and validates a registry of
every GraphQL query/mutation const in `github/` against it — operation
names, field selections, argument names, and variable types. The registry
references each const **by Go identifier**, not by re-parsing source text,
so what's validated is provably the exact string the production code sends.
A completeness guard (`TestWireContract_RegistryIsComplete`) counts
`c.graphqlRequest(` call sites in non-test source and fails if that count
ever diverges from the registry, so a new query added without a matching
registry entry can't silently escape validation.

**Staleness fails closed.** `TestWireContract_SchemaNotStale` fails
`go test` (not a warning) once the schema is more than 180 days old — see
the ADR for why a warning was rejected. Refresh with:

```
scripts/wire-contract/refresh-schema.sh
```

This re-downloads the public SDL and updates `SCHEMA_META.json`'s
`fetched_at`. It needs no credentials and is safe to run anytime, including
from a laptop with no GitHub token configured. Run it whenever
`TestWireContract_SchemaNotStale` fails, or proactively every few months —
there's no fixed cadence beyond "before the 180-day fail-closed limit."
After refreshing, run `go test ./github/...`: a schema change that breaks
validation means a production query now targets a field or argument GitHub
has renamed or removed, and needs a real code fix, not just a re-vendor.

## Recorded fixtures (`recordings/`)

Each file is `{"provenance": {...}, "response": {...}}` — see
`github/testdata_helpers_test.go`'s `loadRecording` for the loader every
migrated test uses. `provenance` records `operation`, `endpoint`,
`recorded_at`, `source_repo`, and `scrubbed: true`; `response` is the exact
byte-for-byte GitHub response (after scrubbing).

### Refreshing

```
scripts/wire-contract/record-fixtures.sh              # record everything
scripts/wire-contract/record-fixtures.sh reads-only    # skip mutation ops
```

Refuses to run under `CI`/`GITHUB_ACTIONS` — see R4/AC7. Requires a `gh` CLI
authenticated with read access to `handarbeit/fabrik` and read/write +
sandbox-admin access to `handarbeit/fabrik-test-alpha` (the same account
`scripts/e2e/reset.sh` already uses; the script prefers a token from
`$FABRIK_TEST_DIR/.env`, falling back to the ambient `gh auth` session).
Run it whenever `refresh-schema.sh` surfaces a schema change that touches
one of the recorded operations, or whenever a live incident traced to
`github/` suggests this suite should have caught something it didn't — i.e.
treat a production wire-format surprise as a trigger to re-record, not just
to patch the code around it.

### R3a: mutation recordings never touch production

Read operations (board fetch, probe fetch, PR review fetch, check-run
fetch) are recorded against the live private `handarbeit/fabrik` board —
read-only, no side effects.

Mutation operations (`addBlockedBy`, project item status change, PR
create/mark-ready/merge, label add) are recorded **only** against the
disposable `handarbeit/fabrik-test-alpha` sandbox repo and its "Fabrik
Test" project board — never against `handarbeit/fabrik`. Each run creates
throwaway issues/branches/PRs for the sole purpose of capturing a real
response, then cleans up via `scripts/e2e/reset.sh`. This PR's own
recordings were captured this way: two disposable issues
(`fabrik-test-alpha#4380`/`#4381`) for `addBlockedBy` and the label
mutation, one disposable draft PR (`#4382`) actually created, marked
ready, and merged for the PR-lifecycle recordings, and one disposable
project item for the status mutation — all reset afterward.

### Scrubbing

Every recording is piped through `scripts/wire-contract/scrubcmd` (a thin
CLI over `internal/wirescrub`) before being written — `wirescrub.Redact`
replaces GitHub-token-shaped, generic-bearer-token-shaped, and
email-shaped substrings with a named `[SCRUBBED:...]` placeholder.
`github/scrub_recordings_test.go` independently re-scans every committed
file under `recordings/` with `wirescrub.Findings` and fails `go test` on
any match, regardless of what a fixture's own `provenance.scrubbed` field
claims — see `internal/wirescrub/scrub_test.go` for the seeded-secret proof
that `Findings` itself works (AC6).

**This scrubbing step was not just a hypothetical exercise.** The
`create_draft_pr` recording, captured from a real `POST .../pulls` response
against the sandbox, initially contained real committer email addresses
(GitHub's PR payload embeds them in nested user objects) — `scrubcmd -check`
caught this before the fixture was committed, and the committed file has
`[SCRUBBED:email address]` in their place.

## AC5: handwritten fixtures vs. recordings

One concrete discrepancy was found and is **not** silently normalized away:

**`fetch_project_board` (`TestFetchProjectBoard_Success`)** — the old
hand-authored fixture included `body`, `url`, `author`, and `assignees`
keys on issue content. The real `fetchProjectBoardQueryTemplate` query
(`github/project.go`) never requests those fields, and `itemNode` (also
`github/project.go`) has no struct fields to receive them — GitHub would
never actually return those keys for this query. The fixture's own
assertions already documented "shallow query does not populate body/author/
assignees," but the JSON it served claimed otherwise. The recording only
contains what the query actually asks for; this was harmless in practice
(unmarshal silently ignores unknown keys) but is exactly the class of
fixture/reality drift this issue exists to catch.

Every other migrated fixture (`add_blocked_by`, `update_project_item_status`,
`add_label_to_issue`, `create_draft_pr`, `mark_pr_ready`, `merge_pr`,
`fetch_pr_reviews`, `fetch_check_runs`, `probe_project_board`) matched its
handwritten predecessor's *shape* closely enough that no second discrepancy
was found — the differences were only ever in specific field values (real
issue numbers/titles/IDs vs. placeholder ones), which is expected and not a
finding.

## Coverage statement (R5/AC9)

**Recorded** (10 files under `recordings/`, all in R5's priority set):

| Operation | Kind | Source |
|---|---|---|
| `fetch_project_board` | GraphQL read | live `handarbeit/fabrik` board |
| `probe_project_board` | GraphQL read | live `handarbeit/fabrik` board |
| `fetch_pr_reviews` | REST read | live `handarbeit/fabrik` PR |
| `fetch_check_runs` | REST read | live `handarbeit/fabrik` PR |
| `add_blocked_by` | GraphQL mutation | sandbox (`fabrik-test-alpha`) |
| `update_project_item_status` | GraphQL mutation | sandbox ("Fabrik Test" board) |
| `add_label_to_issue` | REST mutation | sandbox (`fabrik-test-alpha`) |
| `create_draft_pr` | REST mutation | sandbox (`fabrik-test-alpha`) |
| `mark_pr_ready` | GraphQL mutation | sandbox (`fabrik-test-alpha`) |
| `merge_pr` | REST mutation | sandbox (`fabrik-test-alpha`) |

**Schema-validated (R1) but fixture-free (R2).** These are enumerated in
`wire_contract_test.go`'s registry and validated against the real schema on
every `go test` run, but their `httptest` tests (where they still exist)
continue to serve hand-authored literals — migrating the full 173-site
`httptest` surface was explicitly out of scope (only R5's named priority
set was migrated):

- `fetchItemDetailsQuery` (deep item fetch — labels/comments/blockedBy/linked PRs)
- `fetchNodeCommentsQuery`, `fetchNodeLabelsQuery` (pagination continuations)
- `fetchProjectUpdatedAtQuery`, `fetchProjectItemStatusQuery`, `fetchProjectItemStatusBatchQuery`
- `lookupIssueProjectItemQuery`, `addProjectV2ItemByIdMutation`
- `fetchPRReviewDecisionQuery`, `fetchPRReviewThreadsQuery` (the latter added
  2026-08-09 for Pruefer's prior-thread context, after this issue's Specify —
  in scope for R1 by "every query string," out of scope for R2's named
  priority list)
- `prNodeIDQuery` (used internally by `MarkPRReady`/auto-merge/merge-queue mutations)
- `enablePullRequestAutoMergeMutation`, `disablePullRequestAutoMergeMutation`
- `enqueuePullRequestMutation`, `dequeuePullRequestMutation`
- `archiveProjectItemMutation`, `fetchStatusFieldQuery`
- `resolveReviewThreadMutation`

**REST surface not in R5's named list**: `FetchLatestRelease`,
`FetchRepoAccess`, `FetchInstalledVersion` (`github/client.go`),
`GetIssueBody`, and the various label/comment CRUD REST calls in
`github/labels.go`/`github/comments.go` beyond `AddLabelToIssue` — none of
these have a single chokepoint to enumerate from mechanically (see the ADR's
discussion of R6), and remain covered only by their existing hand-authored
`httptest` tests.

An incomplete contract suite that presents as complete is the same
false-confidence failure R5 warns about for an over-simplified fake — this
section exists so nobody has to guess at the boundary.

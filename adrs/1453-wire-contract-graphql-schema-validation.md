# ADR 1453: Wire-Contract GraphQL Schema Validation

**Date**: 2026-08-09
**Status**: Accepted
**Issue**: #1453 — `github/`'s 173 `httptest` fixtures are handwritten by the same reading of the
API as the code that parses them, so nothing ever independently checked a query/mutation string
against GitHub's real schema; the `addBlockedByIssue`/`addBlockedBy` bug (v0.0.66) shipped broken
through exactly this gap.

## Context

Every GraphQL query and mutation `github/` sends funnels through a single chokepoint,
`Client.graphqlRequest` (`github/client.go`). Before this issue, each query/mutation was an inline
Go string literal (or, for the board-fetch queries, an `fmt.Sprintf`-templated one) built directly
at its call site — there was no way to reference "the query FetchPRReviewDecision sends" from
outside that function, so nothing could enumerate them for validation.

Two design questions had to be answered: how to make the 23 call sites enumerable without
touching production behavior, and what to validate them against.

## Decision

### Hoist queries to named package-level consts, referenced by identifier

Per the issue's own allowance ("if a query must move to a named constant to be enumerable, that is
acceptable and should be mechanical"), every inline query/mutation string was lifted to a
package-level `const` at its existing call site — same file, same string, no behavior change
(`fetchItemDetailsQuery` already followed this pattern before this issue; the rest now match its
style). `github/wire_contract_test.go` is a white-box test (`package github`, not `github_test`)
whose registry (`wireContractRegistry`) references each const **by Go identifier**, not by
re-scraping source text with a parser or regex. This was chosen over AST-scanning
`c.graphqlRequest(` call sites for the query text: AST-scanning would have to re-derive Go's own
string-concatenation and `fmt.Sprintf` semantics inside the test (several queries — the
board-fetch pair, and several others that splice in `commentSelectionFragment` via `+` — are built
from more than one literal) to get the actual wire text, whereas referencing the same named const
the production code calls is correct by construction: whatever the compiler resolves the const to
is exactly what the test validates and exactly what ships.

The two board-fetch queries are `fmt.Sprintf` templates parameterized by `ownerType`
(`"organization"`/`"user"`); their registry entries render both variants so both are validated,
while still counting as a single call site each for the completeness guard below.

### Add `github.com/vektah/gqlparser/v2` rather than hand-rolling a validator

CLAUDE.md's Go conventions say to minimize external dependencies (the project had none beyond
`yaml.v3` at the time). This issue overrides that convention deliberately: AC2 and AC3 require
operation-name validation *and* field/argument-level validation to be proven independently, and a
hand-rolled checker is exactly the kind of thing that's tempting to stop at operation-name matching
and under-cover the field/argument class — which is the actual bug class this issue exists to
catch (`blockingIssueId` vs `blockedById` is an argument-name bug, not an operation-name bug).
`gqlparser/v2` is the parser gqlgen itself is built on: pure Go, MIT-licensed, SDL parsing plus
static query validation only (no network or runtime footprint), and it validates GitHub's schema
cleanly, including the interfaces/unions/inline fragments (`... on Issue`, `... on PullRequest`)
several of `github/`'s queries use.

### Vendor GitHub's public GraphQL SDL, not live introspection

GitHub publishes its schema at `https://docs.github.com/public/fpt/schema.docs.graphql` — no auth
required. `scripts/wire-contract/refresh-schema.sh` re-downloads it and rewrites
`github/testdata/schema/SCHEMA_META.json` with `source_url` and `fetched_at`. Live introspection
against `api.github.com/graphql` was considered (it's what originally caught the real
`addBlockedBy` bug, per the code comment in `project.go`) and rejected for the *vendored copy*: it
would make every `go test` run require a live token, defeating R1's "static, offline once the
schema is vendored" requirement. Nothing rules out using introspection *as the refresh mechanism*
later if the public SDL download ever stops matching what a real token sees; that would be a
same-shaped change to `refresh-schema.sh` only, not to the validation test.

### Schema staleness fails closed

Per R1 ("a stale schema failing open is the one outcome to avoid"), `wire_contract_test.go` reads
`SCHEMA_META.json`'s `fetched_at` and fails `go test` (not a warning) once the schema is older than
180 days. 180 days is a judgment call, not derived from a GitHub deprecation-cycle document — if it
proves too aggressive or too lax in practice, it's a one-line constant
(`wireSchemaMaxAge` in `wire_contract_test.go`) to adjust.

### A completeness guard against registry drift

`wireContractRegistry` is manually maintained, which means a new GraphQL call added to production
code without a matching registry entry would silently escape schema validation — the same
"presents as complete but isn't" failure R5 warns about, applied to R1 itself.
`TestWireContract_RegistryIsComplete` counts `c.graphqlRequest(` occurrences across `github/`'s
non-test source files and asserts it equals `len(wireContractRegistry)`. It cannot say *which*
query is missing, only that the counts diverged — but that turns a silent gap into a failing test,
which is the property that matters.

### Two independent neutralization tests, not one

AC2 and AC3 are deliberately separate tests (`TestWireContract_NeutralizationCatchesBadOperationName`,
`TestWireContract_NeutralizationCatchesBadArgumentName`), each mutating an **in-test copy** of
`addBlockedByMutation` (via `strings.Replace` on the const's value — the real const is never
touched, and is independently proven valid by `TestWireContract_AllQueriesValidateAgainstSchema` in
the same file) and asserting the resulting validation error names the specific bad identifier. An
operation-name-only check would pass the first test while missing most of the bug class the second
one exists to catch — this is exactly R1/AC3's stated concern, made concrete as two permanent
regression tests rather than one.

## Consequences

- Every new GraphQL call added to `github/` must add a matching `wireContractRegistry` entry in
  `wire_contract_test.go`, or `TestWireContract_RegistryIsComplete` fails `go test`. This is the
  intended friction — it's the mechanism that keeps R1 from silently decaying as the package grows.
- The vendored schema needs a human to run `scripts/wire-contract/refresh-schema.sh` and commit the
  result periodically; letting it sit past 180 days fails the whole suite by design (see
  "Schema staleness fails closed"), not just this test file.
- `github.com/vektah/gqlparser/v2` is now a direct dependency (previously indirect, pulled in
  transitively). No other part of the codebase should need to import it beyond
  `wire_contract_test.go`.
- This ADR covers R1 (schema validation) only. R2/R3 (recorded fixtures, scrubbing) are a
  structurally separate mechanism sharing only the `github/testdata/` directory and README — see
  `github/testdata/README.md` for their design.

## Related

- #1453 — the issue and requirements this ADR implements.
- `github/testdata/README.md` — schema refresh procedure, fixture recording procedure, scrubbing,
  and the coverage statement (R5/AC9).
- `github/wire_contract_test.go` — the implementation.

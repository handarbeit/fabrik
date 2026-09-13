# ADR 1714: Board Administration (Create & Repair) via Projects v2

**Date**: 2026-09-13
**Status**: Accepted
**Issue**: #1714 — feat(init): create, validate and repair the project board from stage configs

## Context

Fabrik's most common onboarding failure has always been a hand-built project board whose
column names don't exactly match the stage names in `.fabrik/stages/*.yaml` — a human
retyping nine names into GitHub's UI, one typo away from a startup failure. `CLAUDE.md`'s
Common Issues section has led with this since early in the project's life. Fabrik already
knows the correct names: it wrote them, to `.fabrik/stages/` at `init` time.

GitHub's Projects v2 GraphQL API can create and fully configure a board
(`createProjectV2`, `updateProjectV2`, `updateProjectV2Field`), verified live against the
API on 2026-09-13. Critically, **this works with an ordinary personal access token as well
as a GitHub App token** — it does not depend on App-auth support (#770), so the onboarding
win is available to every user today, not gated behind that separate chain.

Separately, an existing board that predates a new stage (or predates `fabrik init` writing
every stage's column) has no path back to alignment except manual editing. `checkStageColumnAlignment`
(`engine/startup.go`) already diagnoses the mismatch by name — this issue's R2 confirmed
that reporting was already adequate (missing stages listed with order/present/MISSING,
extra columns listed by name) and required no new work — but nothing could *fix* it short
of a human retyping into GitHub's UI, the exact failure mode this issue exists to remove.

## The R4 hazard: `updateProjectV2Field` replaces the entire option list

The single highest-risk fact this issue is built around: GitHub's `updateProjectV2Field`
mutation does not patch a Status field's options — it **replaces the whole list** in one
call. `ProjectV2SingleSelectFieldOptionInput.id` is optional. Measured live on 2026-09-13:

- Omit `id` on an option → the server mints a **fresh** id for every option in the call,
  and **every item on the board silently loses its Status** (`fieldValueByName` returns
  `null` for it afterward — there is no error, no warning, just silent data loss).
- Echo back each existing option's current `id` → ids are preserved, item Status
  assignments survive, and new options can be added by supplying `id: null` on just those
  entries.

Status **is** Fabrik's own pipeline state (`fabrik/engine` drives entirely off which column
an item sits in). An id-losing call against a live board is a correctness incident, not a
cosmetic one: every in-flight issue would fall out of its column simultaneously.

A second, easy-to-miss wrinkle: `color: ProjectV2SingleSelectFieldOptionColor!` and
`description: String!` are **both required, non-null** on every option in the call —
including ones whose `id` is being echoed back unchanged. `FetchStatusField`'s
pre-existing query only fetched `{id, name}`, which is not enough to safely reconstruct
this payload. Getting `id` right but leaving `color`/`description` unfetched does not
silently lose data — it fails the mutation outright (a required-field validation error) —
but it does mean the read side had to grow before the write side could exist at all.

## Decision

### Read side: extend `FetchStatusField`, additively

`github/status.go`'s `fetchStatusFieldQuery` now also selects `color` and `description`
per option. `StatusField` gained a new `OptionDetails map[string]StatusOption` field
(`StatusOption{ID, Name, Color, Description}`) alongside the pre-existing
`Options`/`OrderedOptionNames` — those are untouched, so every existing caller
(`checkStageColumnAlignment`, `UpdateProjectItemStatus`'s id lookups) needs no change.
Only the new repair path consumes `OptionDetails`.

### Write side: a new file, `github/project_admin.go`

This is the first "administer the board's *structure*" write path in the codebase —
`project.go`/`status.go` only ever fetch board state or move/add individual items. Five
new GraphQL operations, all registered in `github/wire_contract_test.go`'s wire-contract
registry and validated against the vendored schema:

- `ResolveOwner(login) (ownerID, ownerType string, err error)` — a single
  `repositoryOwner(login:)` query resolves both the owner's node ID (for
  `createProjectV2`'s `ownerId`) and its `__typename` ("organization"/"user", for R7's
  refusal gate) in one round trip. This is the only way to learn ownerType for a board
  that doesn't exist yet — `FetchProjectBoard`'s org-then-user fallback has nothing to
  probe until a project is present.
- `FetchRepositoryID(owner, name) (string, error)` — resolves the repo's node ID for
  `createProjectV2Input.repositoryId`.
- `CreateProjectV2(ownerID, title, repositoryID) (projectID string, number int, err error)`
  — **`repositoryId` is supplied inline**, in the same call that creates the project.
  `linkProjectV2ToRepository` (a separate mutation Research initially scoped) is
  deliberately **not implemented**: nothing would call it, there is no window where the
  project exists unlinked, and an untested, uncalled mutation is dead weight the
  wire-contract completeness check would otherwise have to carry forever.
- `SetProjectDescription(projectID, shortDescription) error` — sets R1's "set a
  description" requirement via `updateProjectV2`'s `shortDescription` field.
- `SetStatusFieldOptions(fieldID string, options []StatusOptionInput) error` — the R4
  mutation itself. `StatusOptionInput.ID *string`: non-nil to preserve an existing option
  (id/color/description all required to be the option's *current* values, unchanged),
  nil to create a new one. This function has no knowledge of "existing" vs. "new" beyond
  what the caller passes in — the safety property lives entirely in what callers build,
  which is why both callers below are described explicitly.

### `fabrik init --create-board` (R1)

A brand-new board has **no items yet** — there is nothing for R4's id-preservation
contract to protect. `runCreateBoard`/`createBoardCore` (`cmd/init.go`) therefore replaces
the fresh project's default Status options (GitHub's own template: Todo/In Progress/Done)
outright, building every `StatusOptionInput` with `ID: nil` — deliberately not reusing the
id-echo contract, since there is nothing to echo. Options are set from
`requiredStageColumnNames`, in stage `Order`, including holding stages (e.g. `Queued`)
unconditionally — matching `fabrik init`'s own unconditional extraction of
`queued.yaml` regardless of `merge_train`, so a fresh board and a fresh stage set can
never start out of sync (the exact gap ADR-1421 had to patch around after the fact).

`--create-board` requires `--owner` and `--repo` and is mutually exclusive with the
existing positional `<project-url>` argument (one links to a board that already exists;
this creates one). `fabrik init` had no GitHub client/token dependency before this issue;
`--create-board` gives it its first one, resolved the same way the daemon does
(`--token` > `FABRIK_TOKEN` > `GITHUB_TOKEN`).

### `fabrik repair-board [--apply]` (R3)

A **separate subcommand**, not an `init` flag or a daemon flag — repair targets a board
that, by definition, already has live items with Status assigned, on a completely
different cadence than onboarding. It follows the `refresh-stages` precedent exactly:
default is a dry-run diff (missing columns, by name), `--apply` performs the mutation.
There is no code path in `repairBoardCore` that mutates without `apply == true` — AC4
("repair is never performed without explicit opt-in") holds by construction, not by a
runtime flag check alone.

The apply path builds its option list by walking `sf.OrderedOptionNames` and echoing back
every existing option's `id`/`color`/`description` from `OptionDetails`, **unchanged**,
then appending the missing stage columns with `ID: nil`. There is no code path that omits
an existing entry — R6 ("never delete a column that has items in it") is therefore also
true by construction: deletion was never implemented, not merely guarded against.

### R7: organization-only, shared refusal check

Both entry points funnel through one function, `refuseIfUserOwnedBoard(ownerType string) error`
(`cmd/board_admin.go`), so the refusal wording and behavior can never diverge between
creation and repair. The two paths resolve `ownerType` differently because they have
different sources of truth available:

- **Repair** already has `board.OwnerType`, live-resolved by the existing
  `FetchProjectBoard` call — this is authoritative, never the advisory
  `.fabrik/config.yaml` `owner_type` hint (which could be stale or hand-edited wrong; a
  mutating operation must not trust it).
- **Creation** has no existing board to resolve `OwnerType` from, so it calls
  `ResolveOwner` fresh, purely to get this answer before attempting any mutation.

A user-owned target is refused **explicitly, naming the reason**, before any GraphQL
mutation is attempted — never discovered as an opaque API failure. Under GitHub App
auth this operation is unavailable for a user-owned board (see #770); under a personal
access token it may work, but Fabrik does not attempt it automatically either way.

## R5: the required safeguard

`github/project_admin_test.go`'s `TestSetStatusFieldOptions_PreservesExistingIDsAndEchoesFields`
and `cmd/repair_board_test.go`'s `TestRepairBoardCore_ApplyPreservesExistingIDsAndEchoesFields`
assert the exact outbound GraphQL payload: every existing option's `id`/`color`/`description`
sent unchanged, and only the missing columns appended with `id` omitted entirely (not an
empty string — genuinely absent from the JSON map). This is a payload-shape proof, not a
live-server behavior proof: there is no wire-contract fixture-recording precedent for
Projects v2 admin mutations yet (unlike `github/testdata/recordings/`'s REST-call
precedent), and R7 scopes this org-only in a way that makes a disposable sandbox harder to
justify for this issue. What the tests prove is that Fabrik sends the wire shape the
issue's own empirical 2026-09-13 finding established as safe — the GitHub API's own
server-side id-preservation behavior is the thing that finding already established,
not something re-verified here.

## Consequences

- Fresh onboarding on an organization-owned repo needs no manual board editing at all
  (AC1); the board and stage configs are constructed from the same source, in the same
  command, so they cannot disagree from minute one.
- An existing board falling behind newly added stages has a safe, opt-in, one-command fix
  (`fabrik repair-board --apply`) instead of manual UI editing — closing the loop CLAUDE.md's
  very first Common Issue entry describes.
- `updateProjectV2Field`'s id-preservation contract is now load-bearing production code,
  not merely a documented hazard — any future call site touching this mutation must read
  this ADR and `StatusOptionInput`'s doc comment before changing the payload-construction
  logic.
- User-owned repos are unchanged: manual board setup remains the documented path, and
  both new subcommands refuse loudly rather than attempting an operation that may not be
  available under App auth (#770) and that Fabrik does not want to guess about under a PAT.

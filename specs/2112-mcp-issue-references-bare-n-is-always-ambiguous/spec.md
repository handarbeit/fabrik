# Feature Specification: MCP issue references — bare `N` works in multi-repo mode, and a numeric `issue` is accepted

**Feature Branch**: `fabrik/issue-2112`
**Created**: 2026-10-11
**Status**: Specified
**Input**: User description: "## Problem

Every MCP tool that takes an `issue` (`fabrik_status`, `fabrik_promote`, `fabrik_set_autonomy`,
`fabrik_revalidate`) fails on the two forms an overseer session reaches for first. Both were
observed against the live dev daemon (`dev(5f61019)`, board handarbeit #1, `owner: handarbeit`,
no `repo:` key, so multi-repo mode), driving `fabrik mcp` over stdio. Neither shows up in a
single-repo test fixture.

**1. The bare `N` form is always \"ambiguous\" in multi-repo mode, even with only one repo.**

```
fabrik_status {\"issue\":\"2050\"}
→ fabrik daemon error (ambiguous): the daemon manages 2 repos (handarbeit/, handarbeit/fabrik); use owner/repo#N
```

`handarbeit/` is not a repo. `localAPIBackend.managedRepos` (`engine/localapi.go`) seeds its set
with `e.defaultRepo()`. In multi-repo mode `defaultRepo()` (`engine/engine.go`) returns
`e.cfg.Owner + \"/\" + \"\"`, which is non-empty, so the phantom key is added beside every real repo
taken from the cached items. As a result:
- `resolveIssue`'s single-repo shortcut can never fire on a multi-repo daemon;
- the tool descriptions' promise (\"or just N when the daemon manages exactly one repo\") is false
  for exactly the configuration this instance uses;
- the error message names a repo that does not exist.

**2. A JSON number for `issue` is rejected outright.**

```
fabrik_status {\"issue\":2050}
→ invalid arguments: json: cannot unmarshal number into Go struct field .issue of type string
```

The schema declares `issue` as a string, but issue numbers are numbers, and a model will often
pass `2050`. The rejection costs the session a failed call and a retry. On the action tools it
costs a failed action.

## Requirements

- **R1 — No phantom repo.** `managedRepos` must contain only real `owner/repo` keys. Include the
  configured repo only when `cfg.Repo` is set, and otherwise derive the set from the cached
  items. Audit the other local-API and channel uses of `defaultRepo()` for the same
  owner-only-key hazard and fix any that build a set, match a scope or name a repo from it:
  - `engine/localapi*.go`
  - `engine/overseer_actions.go`
  - `engine/channel_*.go`

  Record the audit result in the PR.
- **R2 — Bare `N` works whenever it is unambiguous.** With exactly one real repo among the
  cached issues, `\"2050\"` and `\"#2050\"` resolve to it, whether or not `repo:` is configured. With
  more than one, the error lists only real repos.
- **R3 — Accept a number.** Every tool argument that names an issue accepts a JSON number as well
  as a string, with the same meaning as the bare `N` string. That covers `issue`, and `issues`
  entries in `fabrik_subscribe` if they are issue references. Advertise it in the input schema
  (e.g. `\"type\": [\"string\",\"integer\"]`, or `oneOf`) so clients and models see it.
  - Keep `additionalProperties: false`.
  - A non-integer, zero or negative number is a clear `invalid arguments` error.
- **R4 — Descriptions say what works.** Update the tool descriptions to match the behaviour.

## Scope

In scope:
- issue-reference resolution in the local API and the `fabrik mcp` shim;
- the `defaultRepo()` audit above.

Out of scope:
- changing `defaultRepo()`'s behaviour for its other ~200 engine call sites;
- new tools;
- Hookdeck.

## Acceptance

- **Multi-repo with one cached repo:**
  - A test configures `Owner` with no `Repo`, and every cached issue is in one repo.
  - `fabrik_status` with `\"N\"`, `\"#N\"` and `N` (a number) resolves.
  - It is shown non-vacuous: it fails with the phantom key restored.
- **Multi-repo with two cached repos:** the bare form is `ambiguous`, and the message lists
  exactly the two real repos (no `owner/`).
- **Single-repo config** (`Repo` set): unchanged.
- **Numbers on action tools:** each action tool accepts a numeric `issue` (tested through the shim's
  argument decoding).
- `go test -race ./...` is clean.

Mentioned for context: #1966 (the human feature thread). Mention it by bare number only in the
PR body. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #1966."

## Background

The daemon's local API and the `fabrik mcp` stdio shim (#1966 family) let an overseer session read and act on board items by issue reference. An issue reference is either the full `owner/repo#N` form or a bare `N` that the daemon resolves when only one repo is in play. The tool descriptions promise this: "or just N when the daemon manages exactly one repo".

Driving the shim against the live dev daemon (multi-repo mode: board owner set, no `repo:` key) exposed two defects that a single-repo test fixture cannot show:

1. **Phantom repo makes bare `N` always ambiguous.** In multi-repo mode the "default repo" the engine reports is the owner followed by a slash and an empty repo name. The local API adds that value to its set of managed repos alongside the real repos found in cached items. The set therefore always has at least one bogus member, so the "exactly one repo" shortcut can never fire. The resulting error names a repo that does not exist (`handarbeit/`), which is misleading.
2. **A numeric `issue` is rejected.** The input schema declares `issue` as a string. Issue numbers are numbers, and a model will often pass `2050`. The call fails at argument decoding. For read tools this costs a retry; for action tools (promote, set-autonomy, revalidate) it costs a failed action.

The same owner-only-key hazard may exist wherever the engine's local-API, overseer-action or channel code uses the default repo to build a set, match a scope or name a repo, so those uses are audited as part of this issue. The engine's wider use of the default repo (about 200 call sites) is deliberately left alone.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Bare issue number resolves on a multi-repo daemon with one real repo (Priority: P1)

An overseer session connected to a multi-repo daemon (board owner configured, no `repo:` key) whose cached issues all live in one repo asks for `fabrik_status` using only the issue number.

**Why this priority**: This is the configuration the real dev instance runs, and the bare form is the first thing a session tries. Today it always fails, so the documented shortcut is dead for exactly this deployment.

**Independent Test**: Configure an engine with an owner and no repo, cache issues from a single repo, and call status with `"N"`, `"#N"` and the number `N`; all three resolve to the same item.

**Acceptance Scenarios**:

1. **Given** a multi-repo daemon whose cached issues are all in one repo, **When** `fabrik_status` is called with `"2050"`, **Then** it resolves to that repo's issue 2050.
2. **Given** the same daemon, **When** called with `"#2050"`, **Then** it resolves identically.
3. **Given** the same daemon, **When** called with the JSON number `2050`, **Then** it resolves identically.
4. **Given** the same test with the phantom owner-only key restored in the managed-repo set, **Then** the test fails (the test is non-vacuous).

---

### User Story 2 - Ambiguity errors name only real repos (Priority: P1)

When a multi-repo daemon genuinely has issues in more than one repo, a bare issue number is rejected as ambiguous and the error tells the caller which real repos to choose from.

**Why this priority**: Correct ambiguity handling is the other half of making the bare form trustworthy, and the current message points at a nonexistent repo.

**Independent Test**: Cache issues from two repos with no `repo:` configured; call status with a bare number and check the error kind and text.

**Acceptance Scenarios**:

1. **Given** a multi-repo daemon with cached issues in exactly two repos, **When** a bare `N` is given, **Then** the error is `ambiguous` and its message lists exactly those two `owner/repo` values and no owner-only entry.
2. **Given** a daemon configured with `repo:` set, **When** a bare `N` is given, **Then** behaviour is unchanged from today.

---

### User Story 3 - A numeric `issue` is accepted by every tool that names an issue (Priority: P1)

A session passes the issue as a JSON number to `fabrik_status`, `fabrik_promote`, `fabrik_set_autonomy` or `fabrik_revalidate` (and to `fabrik_subscribe`'s `issues` list), and it is treated exactly like the bare `N` string.

**Why this priority**: Models routinely emit numbers for numeric-looking identifiers; on action tools a rejection means a failed action and a wasted round trip.

**Independent Test**: Through the shim's argument decoding, call each tool with a numeric `issue` and confirm the request sent to the daemon carries the same reference as the string form; call with a fractional, zero or negative number and confirm a clear `invalid arguments` error.

**Acceptance Scenarios**:

1. **Given** any of the four issue-taking tools, **When** `issue` is the integer `2050`, **Then** the call is decoded as the bare reference `2050`.
2. **Given** `fabrik_subscribe` with `issues: [7, "o/r#8"]`, **When** decoded, **Then** both entries are accepted.
3. **Given** `issue` is `1.5`, `0` or `-3`, **When** the tool is called, **Then** the result is an `invalid arguments` error that says an issue number must be a positive integer.
4. **Given** the tool listing, **When** a client reads the input schemas, **Then** each issue argument advertises that it accepts a string or an integer, and `additionalProperties: false` is still present.

---

### User Story 4 - Descriptions match behaviour (Priority: P2)

The tool descriptions tell a model exactly which reference forms work and when the bare form is unambiguous.

**Why this priority**: The descriptions are what a model reads; an inaccurate promise was part of the defect.

**Independent Test**: Read the listed tool descriptions and verify they mention `owner/repo#N`, bare `N` / `#N` / a number, and the "exactly one repo with issues" condition.

**Acceptance Scenarios**:

1. **Given** the tool listing, **When** inspected, **Then** every issue-argument description states the accepted forms and the single-repo condition accurately.

---

### Edge Cases

- A bare `N` when the cached board has no issues at all: behaves as today for "not found"/unresolvable, not as a phantom-repo match.
- `repo:` configured but the cache also holds issues from another repo: the configured repo is included in the set along with real cached repos, as today.
- `"#N"` with surrounding whitespace, or a numeric string with a leading `+`/zeros: treated per the existing string parser; the numeric path accepts only positive integers.
- A JSON number written with a fractional part or exponent, even an integral one (e.g. `2050.0`, `2050.5`, `1e3`): rejected as `invalid arguments`. Only a plain positive-integer literal is accepted (decided in Plan: the strict literal rule avoids float rounding).
- A number too large to be a valid issue number: rejected as `invalid arguments` rather than overflowing.
- Items in the cache whose repo cannot be determined must not contribute an owner-only key to the set.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The daemon's set of managed repos MUST contain only real `owner/repo` keys. The configured repo is included only when a repo is configured; otherwise the set is derived solely from the cached items.
- **FR-002**: With exactly one real repo among the cached issues, a bare `N` and `#N` MUST resolve to that repo's issue, whether or not `repo:` is configured.
- **FR-003**: With more than one real repo, a bare `N` MUST be rejected as `ambiguous`, and the message MUST list only real `owner/repo` values.
- **FR-004**: Behaviour for a daemon with `repo:` configured MUST be unchanged.
- **FR-005**: Every use of the engine's default repo in the local API (`engine/localapi*.go`), overseer actions (`engine/overseer_actions.go`) and channel code (`engine/channel_*.go`) MUST be audited for the owner-only-key hazard (building a set, matching a scope, naming a repo), and any affected use MUST be fixed. The PR MUST record the audit result, including uses found safe.
- **FR-006**: Every MCP tool argument that names an issue (`issue` on status, promote, set-autonomy, revalidate; and the `issues` entries of subscribe, as they are issue references) MUST accept a JSON integer in addition to a string, with the same meaning as the bare-`N` string.
- **FR-007**: The advertised input schema for each such argument MUST declare that both a string and an integer are accepted, and each tool schema MUST keep `additionalProperties: false`.
- **FR-008**: A numeric issue argument that is non-integer, zero or negative MUST produce a clear `invalid arguments` error.
- **FR-009**: Tool descriptions MUST state the accepted reference forms (`owner/repo#N`, `N`, `#N`, a number) and the condition under which the bare form resolves (exactly one repo among the daemon's issues).
- **FR-010**: Changing the engine default-repo accessor's behaviour for its other call sites is NOT part of this change; the fix is confined to the local-API / shim / audited-channel code paths.

### Key Entities *(if applicable)*

- **Issue reference**: a caller-supplied identifier of a board issue — `owner/repo#N`, a bare `N` / `#N`, or (new) a JSON integer equivalent to bare `N`.
- **Managed repo set**: the set of real `owner/repo` keys the daemon can currently resolve a bare reference against; configured repo (if any) plus repos of cached issues.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: On a multi-repo daemon with all cached issues in one repo, a status request using `"N"`, `"#N"` or the number `N` succeeds in 100% of tests, and the test fails when the phantom key is reintroduced.
- **SC-002**: On a multi-repo daemon with issues in two repos, a bare reference yields an `ambiguous` error whose message names exactly those two repos and no owner-only entry.
- **SC-003**: Each of the four issue-taking tools (and subscribe's `issues`) accepts a numeric issue with zero argument-decoding failures, verified through the shim's decoding path.
- **SC-004**: Fractional, zero and negative numbers each produce an `invalid arguments` error in tests.
- **SC-005**: Single-repo-configured behaviour shows no change in the existing test suite, and `go test -race ./...` passes.
- **SC-006**: The PR description contains a complete audit of the default-repo uses in the three named file groups.

## Assumptions

- The `issues` entries of `fabrik_subscribe` are issue references and so are included in the numeric-acceptance requirement (the issue says "if they are issue references"; they are, per the existing tool description "owner/repo#N, or N").
- The existing string forms `N` and `#N` already parse as bare references once the repo set is correct; only the repo set and numeric decoding need to change.
- A numeric issue is treated as exactly the bare form (never as carrying a repo), so it follows the same single-repo rule.
- Using `oneOf`/a type union in the schema is acceptable to the target MCP clients; the exact schema encoding is a planning decision.
- The audit may conclude that some listed uses are safe; recording that conclusion satisfies the audit requirement.

## Out of Scope *(optional)*

- Changing the default-repo accessor's behaviour for its roughly 200 other engine call sites.
- New MCP tools.
- Hookdeck.
- Closing or otherwise modifying issue #1966 (mentioned for context only; the PR must reference it by bare number with no closing keyword).

## Source References *(optional)*

- Observed failures against the live dev daemon (`dev(5f61019)`, board handarbeit #1, no `repo:` key) via `fabrik mcp` over stdio.
- Local API backend's managed-repo set and the engine's default-repo accessor (referenced in the original report); current default-repo uses in `engine/channel_hooks.go`, `engine/channel_settled.go`, `engine/localapi.go` and `engine/overseer_actions.go` are the starting list for the audit.
- #1966 — the parent human feature thread (context only).

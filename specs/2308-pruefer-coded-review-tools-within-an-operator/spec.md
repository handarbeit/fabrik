# Feature Specification: Pruefer coded review tools within an operator allowlist; outside authors reviewed read-only

**Feature Branch**: `fabrik/issue-2308`
**Created**: 2026-10-16
**Status**: Specified
**Input**: User description:

> Fourth in the chain replacing #1859.
>
> ## Problem
>
> The reviewer's tool grant is fixed in code (`pruefer/claude.go`; #2192 set it to read-only file and shell tools plus `Task`). Coded review tools can't run: linters, analyzers and scanners such as golangci-lint, staticcheck, govulncheck, gosec, semgrep, shellcheck, actionlint, gitleaks and osv-scanner. Those tools give findings a model alone misses.
>
> The real exposure is the PR **head**, not the tools. Many tools execute configuration they find in the tree they analyse: `eslint.config.js`, `conftest.py`, golangci-lint plugins, Makefile targets. Fabrik workers only build branches Fabrik wrote. Pruefer reviews whatever PR arrives, including fork PRs from outside contributors.
>
> ## Requirements
>
> - **R1 (operator ceiling):** `.pruefer/config.yaml` gains `review_tools:`, extra `--allowedTools` rules added to the base read-only grant, for example `Bash(golangci-lint:*)` and `Bash(semgrep:*)`. Default: empty, which keeps today's grant. `go test`, and any other test-runner rule, is refused at config load: running tests is the worker's job, not Pruefer's.
> - **R2 (repo selection within the ceiling):** a repo's base-ref `.pruefer/config.yaml` may list `review_tools:` too. Only rules that are also in the operator's list are granted. Others are ignored with one log line naming them. A repo can never widen the grant beyond the operator's ceiling.
> - **R3 (outside authors are read-only):** the full grant (base plus `review_tools`), and plugin hooks and MCP servers, apply only when the PR author has write access to the repo. Decide that from the PR's `author_association` (`OWNER`, `MEMBER` or `COLLABORATOR`) or an equivalent permission read; anything unknown counts as outside. For any other author, the review runs with today's read-only grant and loads plugins without hooks or MCP servers. The posted review says that it ran read-only and why.
> - **R4 (Dependabot exception):** PRs authored by `dependabot[bot]` get the full grant, as a trusted author would.
> - **R5 (the environment scrub stays mandatory):** every tool, hook and MCP server runs in the #2192 scrubbed environment. That is what keeps head-tree configuration from reaching credentials.
> - **R6 (record):** each review logs the resolved grant: base, operator, repo-selected and whether the outside-author policy downgraded it. The Bash commands actually run come from the transcript.
>
> ## Out of scope
>
> - Installing the tools. The operator puts them on the daemon's `PATH`.
> - Any sandbox beyond the scrubbed environment and the author policy.
>
> ## Acceptance
>
> - An operator rule granted to a trusted author's PR is in the constructed `--allowedTools`. The same rule is absent for an outside author's PR, whose review body states it was read-only.
> - A repo rule outside the operator ceiling is not granted, and the log names it.
> - A `dependabot[bot]` PR receives the full grant.
> - `review_tools: ["Bash(go test:*)"]` is refused at config load.
> - A tool's environment contains no GitHub token or App key.
> - New tests are shown to be non-vacuous; `go test -race ./...` passes.

## Background

Pruefer's reviewer currently runs with a fixed, read-only tool grant: path-scoped Read/Grep/Glob over the clone and staged diff, a short list of read-only shell commands, `Task` and the one review skill (#2192, #2306, #2307). That is safe but leaves useful signal on the table: linters, static analyzers, vulnerability and secret scanners find things a model reading code cannot, and operators already have these tools installed.

The risk of running them is not the tools themselves but what they read. Many execute configuration from the tree under analysis (`eslint.config.js`, `conftest.py`, golangci-lint plugins, Makefile targets), so running one against an attacker-controlled PR head can run attacker-chosen code. Fabrik's own workers only build branches Fabrik wrote; Pruefer reviews any PR that arrives, including fork PRs from outside contributors. The feature therefore pairs an operator-controlled tool allowlist with an author-trust gate, and relies on the already-mandatory scrubbed environment (#2192) as the backstop that keeps credentials out of reach of anything that does run.

This is the fourth issue in the chain replacing #1859. It builds on the operator/repo plugin and skill-selection mechanism of #2307, whose repo-config layering ("a repo can narrow, never widen") it reuses; repo-supplied plugins themselves remain deferred to #2366.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Operator grants coded review tools for trusted authors (Priority: P1)

An operator lists linters and scanners in `review_tools:` in Pruefer's operator config. Reviews of PRs from authors with write access can run those tools and report their findings; the base read-only grant is unchanged.

**Why this priority**: This is the feature's value. Without it nothing else matters.

**Independent Test**: Configure `review_tools: ["Bash(golangci-lint:*)"]`, review a PR whose author is a `MEMBER`, and inspect the constructed tool arguments and the review log.

**Acceptance Scenarios**:

1. **Given** an operator `review_tools` containing `Bash(golangci-lint:*)` and a PR by a `COLLABORATOR`, **When** the review runs, **Then** the constructed `--allowedTools` contains the base read-only rules plus `Bash(golangci-lint:*)`.
2. **Given** an empty or absent `review_tools`, **When** any review runs, **Then** the constructed grant is identical to today's.
3. **Given** `review_tools: ["Bash(go test:*)"]`, **When** the config is loaded, **Then** loading fails with an error naming the rule and stating that test runners are not permitted.

---

### User Story 2 - Outside authors are always reviewed read-only (Priority: P1)

A PR from a fork or from any author without write access is reviewed with today's read-only grant and with plugin hooks and MCP servers disabled, regardless of what the operator configured, and the posted review says so.

**Why this priority**: This is the safety property that makes Story 1 acceptable; the two ship together.

**Independent Test**: Review one PR authored by a `FIRST_TIME_CONTRIBUTOR` (and one with unknown association) with the same operator config as Story 1; inspect the arguments and the posted review body.

**Acceptance Scenarios**:

1. **Given** an operator rule granted to trusted authors, **When** a PR by an author whose `author_association` is `CONTRIBUTOR`, `FIRST_TIMER`, `FIRST_TIME_CONTRIBUTOR`, `NONE` or `MANNEQUIN` is reviewed, **Then** the rule is absent from `--allowedTools`, plugin hooks and MCP servers are not loaded, and the posted review states it ran read-only and why.
2. **Given** the association cannot be determined (field absent, empty, unrecognised value, or a failed permission read), **When** the review runs, **Then** the author is treated as outside.
3. **Given** a PR by an outside author, **When** the review runs, **Then** the base read-only grant is exactly today's.

---

### User Story 3 - Repos choose a subset of the operator's tools (Priority: P2)

A repo's base-ref `.pruefer/config.yaml` can list `review_tools:` to select which of the operator's permitted tools apply to its PRs. It can never add one the operator did not allow.

**Why this priority**: Useful tailoring, but the operator ceiling alone already delivers the feature.

**Independent Test**: Operator allows two rules; repo config lists one of them plus one the operator never listed; review a trusted author's PR and inspect arguments and log.

**Acceptance Scenarios**:

1. **Given** an operator list `[A, B]` and a repo list `[A, C]`, **When** a trusted author's PR is reviewed, **Then** the grant includes `A` only (beyond the base), `B` and `C` are not granted, and one log line names `C` as ignored.
2. **Given** an operator list `[A, B]` and no repo `review_tools`, **When** a trusted author's PR is reviewed, **Then** the grant includes the full operator list (the repo did not narrow).
3. **Given** a repo `review_tools: []` explicitly, **When** a trusted author's PR is reviewed, **Then** no extra rules are granted (the repo selected none).
4. **Given** a repo list containing a test-runner rule, **When** the review runs, **Then** the rule is ignored with a log line and the review proceeds.

---

### User Story 4 - Dependabot PRs get the trusted grant (Priority: P2)

PRs authored by `dependabot[bot]` get the full grant even though the bot has no write association, because dependency-bump PRs are exactly where scanners such as govulncheck and osv-scanner are most useful.

**Why this priority**: A narrow, explicit exception; the feature works without it.

**Independent Test**: Review a PR authored by `dependabot[bot]` with an operator rule configured and inspect the arguments and log.

**Acceptance Scenarios**:

1. **Given** an operator rule and a PR whose author login is `dependabot[bot]`, **When** the review runs, **Then** the rule is in `--allowedTools` and the log records that the Dependabot exception applied.
2. **Given** a PR by a lookalike login (for example `dependabot`, `dependabot-preview[bot]` or `not-dependabot[bot]`), **When** the review runs, **Then** the exception does not apply.

---

### User Story 5 - The grant is auditable and tools never see credentials (Priority: P1)

Every review logs exactly what it was allowed to do and why, and every tool, hook and MCP server runs in the scrubbed environment.

**Why this priority**: The environment scrub is the mandatory backstop for head-tree configuration; the record makes the policy verifiable.

**Independent Test**: Run a review with a fake tool that dumps its environment; assert no GitHub token or App key appears. Read the log line for the resolved grant.

**Acceptance Scenarios**:

1. **Given** a granted tool run during a review, **When** it inspects its environment, **Then** it contains no GitHub token, App private key material or other daemon secret.
2. **Given** any review, **When** it starts, **Then** the log shows the base rules, the operator list, the repo-selected rules, the ignored repo rules, the author association (or unknown) and whether the outside-author policy downgraded the grant.

---

### Edge Cases

- Fork PR authored by a trusted member: trust follows the PR author, not the head repository.
- Author association changes between the webhook and the review: the value read for this review is used; no caching across reviews.
- Review that falls back to the default skill after a failed first attempt (#2307) uses the same resolved grant and author decision as the first attempt.
- Repo `.pruefer/config.yaml` that is invalid or oversized: existing fallback to operator config applies; no repo selection occurs, so the full operator list applies for trusted authors.
- Duplicate or whitespace-variant rules in either list: compared after trimming whitespace; duplicates collapse.
- A repo `review_tools` key in a config read for an outside author: parsed and logged for the record, but has no effect on the grant.
- Operator rules that are malformed (not a recognisable permission rule) are refused at config load rather than passed through.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The operator config accepts a `review_tools` list of extra tool-permission rules appended to the base read-only grant. The default is empty and yields a grant identical to today's.
- **FR-002**: At config load, an operator `review_tools` entry that is a test-runner rule is refused with an error naming the entry and the reason. `Bash(go test:*)` is the required case; the refused set also covers other common test runners (for example `npm test`, `yarn test`, `pytest`, `cargo test`, `make test`, `jest`, `vitest`), and the set is documented.
- **FR-003**: At config load, an operator `review_tools` entry that is malformed or grants an unrestricted capability (a bare `Bash`, `Bash(*)` or equivalent blanket grant) is refused with an error naming the entry.
- **FR-004**: A repo's base-ref `.pruefer/config.yaml` may include `review_tools`. Only entries that exactly match an entry in the operator's (valid) list are granted; every other entry is ignored and one log line names the ignored entries. Omitting the key leaves the operator's full list in effect; an explicit empty list selects none.
- **FR-005**: A repo can never cause a rule outside the operator list to be granted, including via a test-runner, malformed or blanket rule.
- **FR-006**: The full grant (base plus operator/repo-selected `review_tools`) and plugin hooks and MCP servers are enabled only when the PR author is trusted. An author is trusted when their `author_association` is `OWNER`, `MEMBER` or `COLLABORATOR`, or when an equivalent repository-permission read shows write access, or under FR-008.
- **FR-007**: Any author that cannot be positively established as trusted (association absent, empty, unrecognised, or the read failed) is treated as outside. For outside authors the review runs with today's read-only base grant and plugins load with hooks and MCP servers disabled.
- **FR-008**: A PR whose author login is exactly `dependabot[bot]` is trusted for FR-006, regardless of association. No other login pattern qualifies.
- **FR-009**: When the outside-author policy removed anything the trusted path would have granted (extra rules, or operator plugin hooks/MCP servers), the posted review states that it ran read-only and why.
- **FR-010**: Every tool, hook and MCP server a review runs executes in the scrubbed review environment of #2192; the scrub is not relaxed for any author class. No GitHub token, App private key or other daemon secret is present in that environment.
- **FR-011**: Each review logs, in one place, the resolved grant: the base rules, the operator list, the repo-selected rules, the repo rules ignored, the author's association (or "unknown"), whether the Dependabot exception applied, and whether the outside-author policy downgraded the grant.
- **FR-012**: The grant and author decision apply identically to every review attempt for one PR event, including the single fallback re-run to the default skill.
- **FR-013**: The existing static check that guards the review's tool arguments is extended so granted extra rules are recognised as legitimate only when they derive from validated `review_tools`; it still rejects bare tools, unscoped path rules and write/execute/network tools.
- **FR-014**: The operator-facing documentation describes `review_tools`, the repo narrowing rule, the author-trust policy, the Dependabot exception, the refused rules and the fact that tools must be installed on the daemon's `PATH`.

### Key Entities

- **Operator ceiling**: the validated `review_tools` list from operator configuration; the upper bound of any grant.
- **Repo selection**: the subset of the ceiling a repo's base-ref config picks.
- **Author trust decision**: trusted / outside, with the basis (association, permission read, Dependabot exception, or unknown).
- **Resolved grant**: base rules plus selected extras (or base only), as logged and passed to the reviewer.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: For a trusted author's PR, every operator-listed rule appears in the constructed review arguments; for an outside author's PR, none does, and the arguments equal today's baseline byte for byte.
- **SC-002**: A repo-listed rule absent from the operator list never appears in any constructed arguments, and its name appears in exactly one log line for that review.
- **SC-003**: A `dependabot[bot]` PR yields the same arguments as a trusted author's PR; no other login does without write association.
- **SC-004**: Loading a config containing `Bash(go test:*)` (or any other refused test runner) fails 100% of the time with a message naming the rule.
- **SC-005**: A process launched as a granted tool during a review observes no GitHub token or App key in its environment.
- **SC-006**: Every review's log contains the resolved-grant record described in FR-011, and every downgraded review's posted body states the read-only reason.
- **SC-007**: Each new test is demonstrated non-vacuous (fails when the behaviour it covers is removed), and the full suite passes under the race detector.

## Assumptions

- Rule matching between repo and operator lists is exact string equality after whitespace trimming; no globbing or prefix semantics.
- "Test-runner rule" is a documented, maintained denylist of command names (applied to the command inside a `Bash(...)` rule), not a semantic analysis; unlisted runners are the operator's responsibility.
- Only Bash-command-style rules are expected in `review_tools`; blanket and malformed forms are refused rather than interpreted. Non-Bash tool rules are not a goal of this issue.
- `author_association` from the PR (REST data Pruefer already fetches or can fetch alongside it) is the primary signal; a permission read is used only if association is unavailable. Either failing means "outside".
- Trust follows the PR author, not the head repository's owner, so a trusted member's fork PR is trusted.
- Operator `review_tools` is configured the same way as other operator settings (config file, with flag/environment equivalents where those exist for sibling keys).
- The posted-review notice is attached only when the policy actually removed something; a review where the operator configured nothing extra needs no read-only banner, though the log still records the decision.
- Repo-supplied plugins do not exist yet (deferred to #2366); the hook/MCP downgrade therefore applies to embedded and operator plugins today and will cover repo plugins when they arrive.

## Out of Scope *(optional)*

- Installing the review tools; the operator places them on the daemon's `PATH`.
- Any sandbox beyond the scrubbed environment and the author policy (containers, network isolation, filesystem jails).
- Running tests or builds as part of review.
- Repo-supplied plugins (#2366).
- Per-tool argument filtering beyond what a permission rule expresses.
- Reconstructing the commands actually run, which is read from the transcript rather than logged by this feature.

## Source References *(optional)*

- #1859 (the work this chain replaces), #2192 (read-only grant and scrubbed review environment), #2306 and #2307 (review skill as plugin, operator plugins, repo-config layering), #2366 (repo-supplied plugins, deferred)
- `pruefer/claude.go` (`buildReviewToolRules`, `checkReviewArgsScoped`, `buildReviewArgs`); `pruefer/reporeconfig.go` (repo config schema and narrowing)

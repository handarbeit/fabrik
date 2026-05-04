# Symphony — Competitive Comparison

| Field | Value |
|-------|-------|
| Subject | [`openai/symphony`](https://github.com/openai/symphony) |
| Symphony commit pinned | `58cf97da06d556c019ccea20c67f4f77da124bf3` (2026-04-27) |
| Compared on | 2026-04-29 |
| Fabrik commit | `d87e68cd` (HEAD of `main` at time of research) |
| Author | Fabrik Implement (issue #469) |

## What this is

Internal, opinionated comparison of Fabrik against Symphony. The two projects converge on the same outer shape — a long-running daemon that polls an issue tracker, stands up per-issue workspaces, runs a coding agent inside them, and keeps workflow policy in-repo — and diverge on essentially every internal contract. This document maps where they agree, where they differ, and which Symphony ideas are worth stealing, deferring, or rejecting.

## What this is not

- **Not public-facing marketing copy.** Frank, internal, sometimes uncomfortably honest. Symphony is doing several things better than Fabrik; that is named below.
- **Not an ADR.** Adopting any specific idea here warrants its own follow-up ADR.
- **Not a deep code archaeology.** Pinned to Symphony's `SPEC.md` (plus `README.md` and the reference Elixir `WORKFLOW.md`). Implementation details from the Elixir reference impl are flagged explicitly when cited — the spec defers many decisions, and impl ≠ contract.
- **Not a list of follow-up issues.** The "Ideas worth stealing" list at the end is a triage surface for the maintainer. Filing follow-ups is a separate manual step.

## Source material

- `SPEC.md` (read in full, 2169 lines) — language-agnostic, RFC-2119-style v1 draft.
- `README.md` (read in full).
- `elixir/WORKFLOW.md` — reference workflow front matter + prompt body, used as a concrete example of the policy layer.
- `elixir/` source tree — directory listing only. Per the issue spec, deeper code reading is out of scope.

---

## Symphony architecture, in one page

Layered, per [`SPEC §3.2`](https://github.com/openai/symphony/blob/58cf97d/SPEC.md):

1. **Workflow loader** — parses `WORKFLOW.md` (YAML front matter + Markdown body), validates schema, watches the file for hot reload (§5, §6.2). The Markdown body is a single Liquid template rendered per issue; unknown vars/filters fail loudly.
2. **Config layer** — typed getters over the parsed front matter (§5.3).
3. **Tracker client** — Linear-only in v1 (§5.3.1, §11). Reads via `fetch_candidate_issues`, `fetch_issues_by_states`, `fetch_issue_states_by_ids`. **Tracker writes are explicitly deferred to the agent** (§11.5). An optional `linear_graphql` client-side tool extension exposes Symphony's auth to the agent so it can issue arbitrary Linear GraphQL queries/mutations itself (§10.5).
4. **Orchestrator** — single-authority, serializes mutation. Per-issue worker with internal claim states `Unclaimed | Claimed | Running | RetryQueued | Released` (§7). Tracker states (`Todo`, `In Progress`, `Human Review`, `Merging`, `Rework`, `Done`) are workflow-defined; the agent (not Symphony) writes them. After each agent turn, the worker re-fetches tracker state and loops on the same Codex thread up to `agent.max_turns`; on clean exit it schedules a fixed 1s continuation retry.
5. **Workspace manager** — `<workspace.root>/<sanitized_identifier>` (§9). Char-class sanitization (`[^A-Za-z0-9._-] -> _`), no built-in VCS. Repo cloning, branch creation, sync are operator-supplied **shell hooks**: `after_create`, `before_run`, `after_run`, `before_remove` (§5.3.4, §9.4) with a single shared `hooks.timeout_ms`. Workspaces are preserved across runs and across successful exits — cleanup is gated on terminal-state transitions or a startup terminal sweep.
6. **Agent integration** — Codex app-server only, launched via `bash -lc <codex.command>` (§10). Approval, sandbox, and turn-input handling are explicitly **implementation-defined** — the spec refuses to mandate a posture.
7. **Concurrency / retries** — global `max_concurrent_agents` (default 10) plus optional per-state caps (§8.3). Exponential backoff `min(10000 * 2^(n-1), max_retry_backoff_ms)` for failed runs (§8.4); fixed 1s for clean-exit continuations. Retry timers are **in-memory only** — they do not survive restart.
8. **Reconciliation** — every poll tick: (A) stall detection on `last_codex_timestamp` (default 5m); (B) tracker-state refresh on running issues, terminate worker if state is terminal/non-active (§8.5).
9. **Observability** — structured logs REQUIRED with `issue_id`, `issue_identifier`, `session_id`, key=value (§13). Token accounting and rate-limit tracking REQUIRED in orchestrator state. Optional HTTP API: `/api/v1/state`, `/api/v1/<identifier>`, `/api/v1/refresh`, plus a dashboard at `/`.
10. **Restart recovery** — filesystem (preserved workspaces) + tracker poll (§14.3). No persistent DB. Live sessions and retry timers are not restored — recovery rehydrates from "what's still on disk" plus "what the tracker says is in progress."
11. **Trust posture** — implementations MUST declare their trust posture; the spec does not prescribe (§15). Hooks are "fully trusted configuration." `linear_graphql` is called out as a hardening attack surface to narrow.

## Fabrik architecture, in one page

Same outer shape, different internal contracts (cross-references: [`docs/state-machine.md`](../state-machine.md), [`docs/stage-lifecycle.md`](../stage-lifecycle.md), and the `adrs/` directory).

1. **Stage configs** — N per-stage YAMLs in `.fabrik/stages/`, each with its own `prompt`, optional `skill`, `model`, `max_turns`, `allowed_tools`, lifecycle flags (`post_to_pr`, `create_draft_pr`, `read_only`, `cleanup_worktree`, `wait_for_reviews`, `wait_for_ci`, `mark_pr_ready_on_complete`, `auto_advance`), and optional `comment_prompt` / `comment_skill` / `comment_max_turns` (ADR 004). Defaults are embedded in the binary; `fabrik init` extracts them, and `fabrik refresh-stages --apply` adds keys that newer binary defaults introduced (additive only — existing values are never overwritten). **No hot reload.**
2. **Skills** — Markdown skill bodies under `plugin/fabrik-plugin/skills/` are baked into the binary via `embed.FS` (`plugin/embed.go`) and deployed to `.fabrik/plugin/` by `fabrik init`; `fabrik upgrade` upgrades the binary and refreshes the deployed plugin skills (separate path from stage YAML refresh).
3. **Tracker client** — GitHub Projects, single GraphQL fetch per poll (`github/project.go`) — items, comments, linked PRs in one call. **The engine performs tracker writes itself**: labels (`github/labels.go`), reactions (`github/comments.go`), status (`github/status.go`), draft PR creation, mark-ready (`github/prs.go`). The agent emits markers (`FABRIK_STAGE_COMPLETE`, `FABRIK_BLOCKED_ON_INPUT`, `FABRIK_ISSUE_UPDATE_*`, `FABRIK_SUMMARY_*`) on stdout; the engine scrapes them and translates them into mutations.
4. **Engine / poll loop** — `engine/poll.go` semaphore-dispatches workers up to `MaxConcurrent` (default 5). `sync.Map` for in-flight tracking, `sync.Mutex` around `processedSet` and worktree creation.
5. **Per-issue worker** — `engine/item.go` runs one stage per invocation, not a single multi-turn loop. Stage selection is driven by `stage:<name>:complete` / `stage:<name>:in_progress` labels. After a stage completes, the worker exits; the next poll dispatches the next stage.
6. **Worktrees** — bare clone at `.fabrik/repos/<owner>-<repo>.git`, per-issue worktree at `.fabrik/worktrees/<owner>-<repo>/issue-N/` on branch `fabrik/issue-N` (`engine/worktree.go`; ADR 006, ADR 014 (per-repo worktree manager), ADR 022, ADR 023). Git operations (clone, fetch, rebase, push, conflict pass-through) are baked into the engine. **No hooks.**

> **A note on ADR citations.** This repo currently has duplicate ADR numbers (014, 017, 028, and 032 each have two or three files). Where the number alone would be ambiguous, this doc cites by number + short title (e.g. `ADR 028 (merge-conflict gate)` vs. `ADR 028 (rate-limit backoff hysteresis)`). Where context already pins which one is meant (e.g. `ADR 032` in a CI-gate context resolves to the conjunctive-completion-label ADR, not the webhook-event-delivery ADR), the bare number is used.
7. **Agent integration** — Claude Code via `claude` CLI (`engine/claude.go`). Default tool allowlist is enumerated in `CLAUDE.md`; `--permission-mode dontAsk` by default; `fabrik:unrestricted` label flips to `--dangerously-skip-permissions`. Per-stage `allowed_tools` *replaces* (not extends) the default set. `max_wall_time` enforces SIGTERM→SIGKILL deadlines; a 15-minute inactivity timeout applies regardless.
8. **Concurrency / retries** — single `MaxConcurrent` semaphore. No per-stage caps. Stage retries are not separately tracked; if a stage fails to complete, the next poll re-invokes it. Progress is preserved across restart by **rocket reactions** (👀/🚀; ADR 009) and `stage:<name>:complete` labels — both durable on github.com.
9. **Reconciliation** — the poll loop refetches the board each tick; `itemMayNeedWork` / `itemNeedsWork` filter to the dispatchable set. CI/review gates (`fabrik:awaiting-ci`, `fabrik:awaiting-review`) are evaluated by a separate catch-up loop without re-invoking Claude.
10. **Observability** — `[#N tag] ...` per-issue stdout prefix and the GitHub Project board UI. **No HTTP API. No token accounting in orchestrator state. No rate-limit accounting surfaced.** A new TUI exists (`tui/`) but the engine itself does not expose a JSON snapshot.
11. **Restart recovery** — durable state lives on github.com (labels, reactions, comments). `processedSet` is in-memory; rocket reactions are the durable equivalent. No persistent DB.
12. **Trust posture** — the default tool allowlist plus `--permission-mode dontAsk` is the declared posture. `fabrik:unrestricted` bypasses both. Symphony's "implementations MUST declare their trust posture" requirement is **satisfied here only implicitly** in `CLAUDE.md`.

---

## Per-axis comparison

Each axis: a compact table, a short narrative, and an honest "who's better and why." Citations are intentionally light — Symphony §refs and Fabrik ADR/`docs/*.md` paths in parens, no quotes.

### 1. Issue tracker integration

| | Symphony | Fabrik |
|---|---|---|
| Tracker | Linear (`linear` adapter, v1 only) | GitHub Projects |
| Fetch model | Single `fetch_candidate_issues` + per-state refresh | Single GraphQL fetch (items + comments + linked PRs) |
| **Writes initiated by orchestrator** | **None.** Status, comments, PR linking — all deferred to the agent (SPEC §11.5). | **Most.** Labels, reactions, status, PR creation/ready, issue/PR comments. |
| Optional escape hatch | `linear_graphql` client-side tool exposes Symphony's auth so the agent can issue arbitrary GraphQL (§10.5). | None — the engine is the single tracker writer. |

This is the single most consequential divergence and warrants the depth.

Symphony's "the orchestrator runs the agent; the agent writes the ticket" boundary is genuinely interesting. It pushes all tracker mutation responsibility onto the workflow author and the agent's tool repertoire. The win: consistent audit trail (every state change has an agent reasoning trace behind it), uniform auth surface (one client, one token, one set of rate limits), and the workflow body becomes self-contained — `WORKFLOW.md` is *the* policy. The cost: every tracker change is a turn cost; failure modes are agent-shaped (the agent decided not to update status, vs. an explicit bug); auth attack surface widens because the agent now has full Linear write capability via `linear_graphql`.

Fabrik takes the opposite stance. The engine is the trusted writer for mechanical state (labels, reactions, status, draft PR creation/marking). The agent only writes content via markers the engine extracts (ADRs 002, 007, 009, 012, and 017 (decomposed marker state machine)). The win: predictable, cheap state transitions; small audit surface for engine writes; the agent does not need GitHub write tools at all. The cost: the engine is a meaningful body of code (`github/`, ~6 files) that needs to keep up with GitHub's API; behavioral changes ripple through engine + state-machine docs.

**Verdict — different, value-neutral on net.** Fabrik's choice is a better fit for GitHub Projects (where labels and status are the natural primitives and reactions provide free durable state). Symphony's choice is a better fit for Linear (where state is a typed enum and the "agent writes the ticket" boundary is conceptually clean). Neither approach is portable to the other tracker without conceptual loss.

### 2. Workflow definition model

| | Symphony | Fabrik |
|---|---|---|
| Form | Single `WORKFLOW.md` — YAML front matter + Liquid prompt body (§5) | N per-stage YAMLs + Markdown skill bodies (ADR 004) |
| Number of prompts per issue | 1 | One per stage invocation (Research / Plan / Implement / Review / Validate, plus `comment_prompt` per stage) |
| Routing | Encoded in the Markdown body (e.g. `## Status map`, `## Step 0` in Symphony's reference workflow) | Encoded in the engine via stage labels |
| Hot reload | **REQUIRED** (§6.2) — file watcher re-applies config without restart | **Not supported** — operator edits `.fabrik/stages/*.yaml` (or runs `fabrik refresh-stages --apply` to pick up new default keys) and restarts |
| Templating | Liquid, strict (unknown vars/filters fail) | None — prompts are literal Markdown |

**Where Symphony is better**: hot reload is a real operator-UX win, and strict Liquid templating is a defensible discipline. Reloading workflow without restart is genuinely useful when iterating on prompts against a live board. The strict-templating posture (fail loudly on unknown vars) is harder to add to a system that doesn't have templating in the first place.

**Where Fabrik is better**: per-stage configuration. Stage-specific `model`, `max_turns`, `allowed_tools`, `skill`, `comment_prompt`, and lifecycle flags (`post_to_pr`, `read_only`, `cleanup_worktree`, `wait_for_*`, `auto_advance`) are awkward to express as YAML scalars on a single `WORKFLOW.md` front matter. Fabrik's stage YAMLs make those decisions per-stage natively. Per-stage skills also make Claude Code's skill system load only the relevant playbook for the current stage — a smaller prompt surface than Symphony's "one prompt body that internally routes on tracker state."

**Different, value-neutral**: Liquid vs. literal Markdown is a taste call. Fabrik's `.fabrik-context/` files (issue body, prior-stage outputs, codebase changes) achieve a similar goal as Symphony's Liquid variables — injected context — without templating ceremony.

### 3. Stage / lifecycle model

| | Symphony | Fabrik |
|---|---|---|
| Pattern | Single-agent loop, re-fetches tracker state after each turn (§7) | Multi-stage pipeline (Research → Plan → Implement → Review → Validate) |
| Loop bound | `agent.max_turns` per claim, fresh continuation after clean exit | `max_turns` per stage invocation; engine re-invokes if not yet complete |
| Stage advancement | Implicit — agent writes new tracker state and the worker continues until tracker state is non-active | Explicit — engine advances the `stage:*` label set on `FABRIK_STAGE_COMPLETE` (`docs/stage-lifecycle.md`) |
| Inter-stage handoff | Agent's own context within a single thread | Context files in `.fabrik-context/` (ADR 011) |

**Where Fabrik is better**: explicit stage boundaries enable per-stage models (`opus` for Research/Review, `sonnet` for Implement), per-stage tool allowlists, per-stage skills, and per-stage gates (CI, reviews). The marker/label contract is mechanically inspectable — you can answer "what stage is this issue in?" by reading labels alone, without parsing tracker history. Inter-stage context handoff via `.fabrik-context/` files is also genuinely useful: it survives session reset, lets the next stage pick up cleanly, and is human-readable.

**Where Symphony is better**: less ceremony. One prompt, one thread, one loop, one tracker-state recheck. For workflows that are essentially one-shot (e.g., "fix this bug end-to-end"), Symphony's lighter structure is a real advantage. Fabrik's pipeline is heavier than necessary for small or non-pipelined work.

**Different, value-neutral**: Symphony's "re-check tracker after each turn" is more reactive than Fabrik's "advance on marker." Both converge on similar end behavior but with different latency profiles — Symphony reacts to manual tracker edits faster; Fabrik reacts to comments via reactions and labels but does not poll tracker mid-stage.

### 4. Workspace and worktree management

| | Symphony | Fabrik |
|---|---|---|
| Path | `<workspace.root>/<sanitized_identifier>` | `.fabrik/worktrees/<owner>-<repo>/issue-N/` |
| Sanitization | Char-class `[^A-Za-z0-9._-] -> _` | Branch-name based (`fabrik/issue-N`) |
| VCS bootstrap | **Operator-supplied shell hooks** (§5.3.4, §9.4) — `after_create`, `before_run` clone/branch/sync | **Built-in** — bare clone, fetch, rebase onto base branch, push (`engine/worktree.go`) |
| Hook timeout | Single shared `hooks.timeout_ms` | N/A — built-in operations have their own timeouts |
| Preservation | Across runs and across successful exits; cleaned only on terminal-state transition or startup sweep | Across all states except `cleanup_worktree: true` stages (e.g. Done) |
| Concurrency safety | Spec is silent; reference impl serializes its own state mutation | Worktree creation serialized by `sync.Mutex` (git config isn't concurrent-safe) |

**Where Fabrik is better**: built-in VCS operations are simpler for the common case (one repo, GitHub-flow, default branch). Conflict resolution is left as a marker pass-through to Claude (ADR 028 (merge-conflict gate)) — the engine doesn't try to resolve conflicts, but it doesn't make the operator script that path either. Worktree management is opinionated and tested; the operator does not write shell.

**Where Symphony is better**: hooks are genuinely more flexible. A workflow that needs a custom dependency install, language-specific setup (`mix deps.get`, `npm ci`, `cargo build`), or a non-git VCS can express that without forking Symphony itself. Fabrik's "engine bakes git in" is an explicit scope choice — but that means a non-git project, an exotic monorepo layout, or a multi-repo workflow has no escape hatch short of a Bash-tooled `before_run`-equivalent inside the agent prompt.

**Different, value-neutral**: char-class sanitization (Symphony) vs. branch-name-driven path (Fabrik) is a value-neutral implementation choice. Fabrik's path is keyed off the issue number, which is durable and unique within a repo; Symphony's is keyed off the tracker identifier, which is durable and globally unique. Both work.

### 5. Concurrency, retries, restart recovery, idempotency

| | Symphony | Fabrik |
|---|---|---|
| Global concurrency cap | `max_concurrent_agents` (default 10) | `MaxConcurrent` semaphore (default 5) |
| Per-state concurrency cap | **Yes** — optional map (§8.3) | **No** |
| Retry policy | Exponential backoff `min(10000 * 2^(n-1), max_retry_backoff_ms)`; fixed 1s for continuation (§8.4) | Implicit — next poll tick re-invokes if the stage didn't complete |
| Stall detection | **Yes** — `last_codex_timestamp` (default 5m, §8.5A) | Implicit — 15m inactivity timeout per Claude invocation; no orchestrator-level stall sweep |
| Idempotency / restart | Filesystem (preserved workspaces) + tracker poll (§14.3); retry timers and live sessions **not restored** | Rocket reactions (👀/🚀, ADR 009) + `stage:*:complete` labels — both durable on github.com |

**Where Symphony is better**: per-state concurrency caps are a real feature (e.g. "don't run more than 2 Implement workers at a time, but let Review run wide"). Explicit exponential backoff and explicit stall detection are also better-defined than Fabrik's "the next poll tick will get it." Symphony's spec language around §8.4–8.5 is the kind of thing Fabrik should crib for `docs/state-machine.md`.

**Where Fabrik is better**: per-comment durable processed-state via reactions (ADR 009) is novel. Symphony's restart recovery is "what's still on disk + what the tracker says is in progress" — coarser than Fabrik's "this exact comment was already handled." Fabrik can resume comment processing across restart at single-comment granularity; Symphony cannot, because Linear comments don't have a reaction primitive Symphony uses for state.

**Different**: Symphony explicitly does not restore retry timers or live sessions on restart; Fabrik also does not, but the rocket-reaction model means losing in-flight work is cheaper because the engine knows precisely what was already handled.

### 6. State model

| | Symphony | Fabrik |
|---|---|---|
| Source of truth | Linear tracker state (workflow-defined enum) + filesystem | GitHub labels + reactions + comments + filesystem |
| Who writes state | Agent (via Codex tools or `linear_graphql`) | Engine (labels/reactions/status/PR) + agent (content via markers) |
| Markers | None — agent emits state via tracker writes | `FABRIK_STAGE_COMPLETE`, `FABRIK_BLOCKED_ON_INPUT`, `FABRIK_ISSUE_UPDATE_*`, `FABRIK_SUMMARY_*` |
| Lock primitive | Tracker assignment / state | `fabrik:locked:<user>` label, `fabrik:editing`, `fabrik:paused` (ADR 007) |
| Comment-state coupling | Loose — agent reads comments as input | Tight — reactions on comments are durable processed-state (ADR 009) |

**Where Fabrik is better**: marker-based separation (engine owns mechanical state, agent owns content state) is genuinely clean. The full label vocabulary (`stage:*:in_progress`, `fabrik:awaiting-input`, `fabrik:awaiting-review`, `fabrik:awaiting-ci`, `fabrik:bot-reprompted`, `effort:*`, `model:*`, `base:*`, etc., per `CLAUDE.md`) makes engine state mechanically inspectable from github.com without parsing prose. Symphony's reliance on tracker-state-as-string is a thinner state surface — it works, but every dimension of state has to be encoded in the workflow's tracker-state vocabulary, which couples policy and state more tightly.

**Where Symphony is better**: less code. Fabrik's label/marker/reaction system is a meaningful body of mechanism (ADRs 002, 007, 009, 012, 017 (decomposed marker state machine), 026, 027, 028 (merge-conflict gate), 032, 033). Symphony delegates that to the workflow author + agent, which is less code in the orchestrator at the cost of more responsibility on the workflow author.

**Different**: marker-based vs. tracker-write-based is a deep architectural choice; both are internally consistent.

### 7. Observability and operator UX

| | Symphony | Fabrik |
|---|---|---|
| Structured logs | **REQUIRED** — `issue_id`, `issue_identifier`, `session_id`, key=value (§13) | `[#N tag] ...` stdout prefix; not key=value structured |
| Token accounting | **REQUIRED** in orchestrator state | None |
| Rate-limit tracking | **REQUIRED** in orchestrator state (rate-limit window, headroom) | Internal hysteresis logic exists (ADR 028 (rate-limit backoff hysteresis)) but not surfaced |
| HTTP API | Optional `/api/v1/state`, `/api/v1/<identifier>`, `/api/v1/refresh` | None |
| Dashboard | Optional `/` dashboard | None — but a TUI exists (`tui/`) |
| Live snapshot fields | `running` / `retrying` / `codex_totals` / `rate_limits` | N/A |

This is the axis where Fabrik most clearly trails. Symphony's spec is materially ahead at the contract level: structured logs are not optional, token accounting is not optional, the snapshot API is well-defined. Fabrik's observability story is "tail stdout and read github.com." That works in practice — labels are inspectable, reactions are inspectable, comments are inspectable — but it puts the operator burden on the GitHub UI rather than a Fabrik-side surface.

**Where Symphony is better**: structured logs, token accounting, rate-limit accounting, HTTP snapshot API, and dashboard. All of these are useful for any production deployment beyond a single laptop.

**Where Fabrik is better**: zero-infrastructure. The TUI is local-only and does not require operating an HTTP server.

**Different**: Symphony pulls operator state into Symphony itself; Fabrik pushes it into github.com.

The honest call-out: Fabrik should not ship a dashboard (it conflicts with `docs/positioning.md`'s "Fabrik ships no UI" stance), but a read-only `/state` JSON snapshot is a separable and possibly worthwhile idea. See "Ideas worth stealing" below.

### 8. Trust / safety posture

| | Symphony | Fabrik |
|---|---|---|
| Spec posture | Implementations MUST declare trust posture (§15); spec does not prescribe | `--permission-mode dontAsk` default + default tool allowlist; `fabrik:unrestricted` bypasses |
| Hooks | "Fully trusted configuration" (§9.4) | N/A — no hooks |
| Tool allowlist | Implementation-defined | Per-stage `allowed_tools` *replaces* the default set |
| Sandboxing | Spec defers to Codex version | Inherited from Claude Code's built-in sandboxing |
| Notable callout | `linear_graphql` widens agent auth surface; spec flags it as a hardening attack surface (§10.5) | `fabrik:unrestricted` is the equivalent caution — bypasses default allowlist entirely |

**Where Symphony is better**: making "MUST declare trust posture" a spec-level requirement is a good discipline. The `linear_graphql` callout is the kind of attack-surface honesty that Fabrik should imitate.

**Where Fabrik is better**: more concrete defaults out of the box. The default tool allowlist (Read, Edit, Write, Glob, Grep, TodoWrite, Skill, Task, plus narrowly-scoped Bash patterns — see `CLAUDE.md`) is a real, enforced posture rather than "implementation-defined."

**Different**: Symphony defers, Fabrik prescribes. Both are defensible — Symphony's spec is intentionally agent-agnostic; Fabrik is intentionally Claude-Code-specific.

### 9. PR lifecycle integration

| | Symphony | Fabrik |
|---|---|---|
| Draft PR creation | Agent (via Codex tools or `linear_graphql`) | Engine, on stage with `create_draft_pr: true` |
| Mark-ready | Agent | Engine, on stage with `mark_pr_ready_on_complete: true` |
| CI gate | Workflow body asks the agent to wait | `wait_for_ci: true` + `fabrik:awaiting-ci` (ADRs 027, 032) |
| Review gate | Workflow body asks the agent to wait | `wait_for_reviews: true` + `fabrik:awaiting-review` + bot-reviewer escalation ladder (ADR 026, recent ladder work in `state-machine.md` §6) |
| Merge | Agent (via tools) | Engine on `fabrik:yolo` after Validate; otherwise human |
| Auto-merge | Workflow-defined | `fabrik:yolo` label |

**Where Fabrik is better**: this whole axis is a direct consequence of the tracker-write boundary (axis 1). Engine-driven PR lifecycle gates produce mechanically-inspectable state (`fabrik:awaiting-ci`, `fabrik:awaiting-review`) and let the engine cheaply skip re-invocation while a gate is held. Symphony's "agent writes everything" model means each gate is an agent turn cost.

**Where Symphony is better**: by deferring PR lifecycle entirely, Symphony avoids being coupled to GitHub's PR semantics. Linear's "Merging" tracker state is a state, not a PR object, which is a thinner contract.

**Different**: Fabrik's PR-lifecycle gating is a real engineering investment (ADRs 026, 027, 028 (merge-conflict gate), 032, 033, a dedicated bot-reviewer escalation ladder, a CI-failure path). Symphony has no equivalent because PRs are not in scope at the orchestrator layer.

### 10. Comment / human-in-the-loop interaction

| | Symphony | Fabrik |
|---|---|---|
| Comment ingestion | Agent reads comments as input within its prompt (e.g. Symphony's reference `## PR feedback sweep protocol`) | Engine surfaces comments via reactions; per-stage `comment_prompt` / `comment_skill` / `comment_max_turns` (ADRs 008, 009, 012) |
| Per-comment durable state | Tracker-state-driven | 👀 (eyes) on receive; 🚀 (rocket) on success — survives restart at single-comment granularity |
| Distinct prompt surface for comments | None — folded into the single workflow body | Yes — separate prompt and skill per stage |
| Living-document comment | None | Yes — stage comments are edited in place across iterations (ADR 012) |

This is Fabrik's clearest standalone advantage. The reaction-based comment lifecycle (ADR 009), the per-stage `comment_prompt` / `comment_skill` separation, and the living-document stage comment (ADR 012) are all original Fabrik design — none of them have a Symphony analog. Symphony folds comment-handling into the single workflow prompt body, which works but commits the workflow author to encoding the entire comment-response policy as Markdown prose.

**Where Fabrik is better**: per-stage `comment_*` configuration; reaction-based durable state; living-document comments.

**Where Symphony is better**: simpler model. One workflow body, no separate `comment_*` surface to maintain.

**Different**: Fabrik's distinction between "initial-stage prompt" and "comment-response prompt" is a meaningful feature; Symphony's single-prompt model is meaningfully simpler.

---

## Ideas worth stealing

Triage tags: `adopt` / `consider` / `reject` / `watch`. Default is one or two sentences; depth reserved for genuinely creative ideas.

### Adopt (≤ 3)

1. **Structured logs with `issue_id` / `issue_identifier` / `session_id` / key=value (§13).** `adopt` — Fabrik's `[#N tag] ...` prefix is human-readable but not machine-parseable. Adding a structured-log mode (env var `FABRIK_LOG_FORMAT=json` or similar) would make production deployments materially easier to operate without changing the existing prefix format for interactive use.

2. **Token accounting in orchestrator state (§13).** `adopt` — Symphony tracks per-issue token totals as first-class state. Fabrik does not surface this anywhere, despite having access to it via Claude Code's invocation results. A `tokens:*` field per issue would feed both observability and `model:*` decisions ("this issue burned 5M tokens; suggest splitting").

3. **Spec-mandated trust-posture declaration (§15).** `adopt` — Fabrik already has a posture (default allowlist + `fabrik:unrestricted`), but it is documented in `CLAUDE.md` rather than treated as a contract. Promoting it to a dedicated `docs/trust-posture.md` (or a new section in `docs/state-machine.md`) would close the discipline gap. This is documentation work, not code work.

### Consider (depth on the genuinely creative ones)

4. **Read-only `/state` JSON snapshot endpoint.** `consider` — Symphony's `/api/v1/state` is the tip of an iceberg that includes a dashboard at `/` (which Fabrik should reject — see below). But the snapshot itself is a clean separable feature: a single read-only endpoint exposing `running` / `retrying` / `codex_totals` / `rate_limits` would let operators integrate Fabrik with external dashboards (Grafana, etc.) without forcing Fabrik to ship a UI. Conflicts mildly with the "no UI" positioning but is JSON-only, not HTML. Worth deciding deliberately.

5. **`linear_graphql`-style client-side tool injection (§10.5).** `consider`, with depth — Symphony exposes its Linear auth to the agent via a thin GraphQL tool, letting the agent issue arbitrary tracker mutations using the orchestrator's credentials. The Fabrik analog would be a `github_graphql` MCP tool exposing the engine's GitHub token to the agent for queries it cannot otherwise make. The interesting part is not the tool itself but the **boundary it implies**: agent gets engine auth, and the engine's trust surface widens. Fabrik has deliberately stayed on the opposite side of this boundary (engine writes, agent emits markers). Adopting this would be a meaningful posture change. Default to `reject`; revisit only if the marker-based contract breaks down for some specific surface (e.g. reading PR review thread content, which is currently awkward).

6. **Stall detection on `last_*_timestamp` (§8.5A).** `consider` — Fabrik has a 15-minute inactivity timeout per Claude invocation, but no cross-invocation orchestrator-level stall sweep. A "no progress for N minutes" reconciler at the poll loop level would catch cases where a stage is silently wedged (e.g. a stuck CI gate that never times out).

7. **Per-state concurrency caps (§8.3).** `consider` — Fabrik has a single `MaxConcurrent`. A per-stage cap (e.g. "max 2 Implement workers, max 5 Validate workers") would be useful for token budget management and review-bot rate limiting. Low priority but cheap.

8. **Hot reload of stage YAMLs (§6.2).** `consider` — Symphony REQUIRES hot reload; Fabrik requires editing `.fabrik/stages/*.yaml` and restarting (with `fabrik refresh-stages --apply` to pick up new default keys when upgrading). Hot reload would speed iteration, particularly when tuning prompts. Watch-and-reload is well-trodden territory; the implementation cost is moderate. The reason to defer: Fabrik's stage YAMLs are tracked in git and edited per-repo, not centrally; a reload would need to refresh per-repo configs and the embedded plugin source, with subtle ordering. Worth doing; not urgent.

### Reject

9. **HTML dashboard at `/`.** `reject` — conflicts directly with `docs/positioning.md`'s "Fabrik ships no UI" stance and the Fabrik-as-driver-not-observer thesis. The TUI is the local UX; github.com is the remote UX. Adding a dashboard puts Fabrik in the same bucket as Vibe Kanban et al., which is the *competitor* positioning, not Fabrik's. Decoupled from idea #4 above (JSON snapshot is fine; HTML dashboard is the line).

10. **Single `WORKFLOW.md` model (§5).** `reject` — losing per-stage `model`, `max_turns`, `allowed_tools`, `skill`, `comment_*`, and lifecycle flags would be a regression. Symphony's single-prompt model works for one-shot workflows but does not express the SDLC pipeline cleanly. The `WORKFLOW.md` `## Status map` and `## Step 0` constructs in Symphony's reference workflow are roughly Fabrik's stage YAMLs encoded as Markdown — a step backwards in typing and validation.

### Watch

11. **SSH worker pool (Appendix A) — central orchestrator, remote workers over SSH stdio, per-host concurrency caps, host stickiness on retry.** `watch`, with depth — this is the genuinely creative idea in Symphony's spec. It addresses a real pain point (scaling agent execution across machines without paying for a managed runner like GitHub Actions or a Kubernetes pool) with a thin transport (SSH stdio) and a clean design (host stickiness on retry preserves workspace locality). Fabrik has no equivalent and no near-term need — the typical Fabrik deployment is one operator, one box. But if Fabrik ever grows multi-host requirements (e.g. heavy Implement parallelism across cheap rented hardware), this design is worth re-reading rather than re-deriving. Pinned here as the highest-value item to revisit on Symphony v2.

12. **Strict Liquid templating with fail-on-unknown-vars (§5).** `watch` — Fabrik has no templating today, and adding one would be a meaningful surface increase. Liquid's strict mode is a defensible choice if templating is ever introduced. Not actionable today.

---

## Closing

Symphony and Fabrik converge on the **outer shape** (poll → per-issue workspace → in-memory orchestrator → durable recovery via tracker + filesystem → no DB) and diverge on **internal contracts** (single-prompt vs. staged pipeline; agent-writes-tracker vs. engine-writes-tracker; hooks vs. baked-in VCS; mandated observability vs. github-native observability). Neither approach dominates; the choice between them is genuinely tied to which tracker you target and which workflow shape you privilege.

Where Symphony is materially ahead today: observability (structured logs, token accounting, snapshot API), spec discipline (mandated trust posture, mandated hot reload), and per-state concurrency caps. Where Fabrik is materially ahead today: per-stage configuration, comment-handling lifecycle (reactions + per-stage `comment_*`), engine-driven PR gates, and inter-stage context-file handoff.

**Revisit triggers:**
- Symphony cuts a v2 spec (the current SHA is a draft).
- Symphony graduates from "low-key engineering preview" to a stable release.
- Symphony's tracker scope expands beyond Linear (especially to GitHub Projects — the surface where the "agent-writes-the-ticket" boundary is most directly comparable to Fabrik's).
- Symphony's reference impl ships a non-Codex agent integration, surfacing the spec's agent-agnosticism in practice.
- Fabrik considers any of the `adopt` items above seriously enough to ADR them.

Adopting any of the above items beyond documentation work warrants its own follow-up ADR. This document is analysis, not a Fabrik decision.

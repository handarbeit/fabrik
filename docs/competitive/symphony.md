# Symphony vs. Fabrik: Architecture Comparison

**Symphony commit:** `2c1851830477434100fdb8980fcc1fce1a8af81d`  
**Compared on:** 2026-05-23  
**Audience:** Fabrik engineering — internal reference, not public-facing.

---

## Framing

[Symphony](https://github.com/openai/symphony) is an OpenAI project with the same basic shape as Fabrik: a long-running daemon that polls an issue tracker, creates per-issue workspaces, runs coding agent sessions inside them, and keeps workflow policy in-repo. The projects converge on similar ground from different starting points — Symphony from OpenAI's Codex toolchain and Linear, Fabrik from Claude Code and GitHub Projects.

This document is a frank, side-by-side comparison based on Symphony's `SPEC.md` (Draft v1) as the primary source. Symphony's spec is written around Codex as the agent; where Codex-specific primitives (e.g., `approval_policy`, `thread_sandbox`) appear, they are noted but don't distort the architectural comparison — the underlying design choices translate.

---

## Axis-by-Axis Comparison

### 1. Issue Tracker Integration

| Dimension | Symphony | Fabrik |
|---|---|---|
| Tracker | Linear (v1 only) | GitHub Projects (only) |
| Dispatch trigger | Issue in `active_states` | Issue in board column matching a stage name |
| Tracker writes | Delegated to agent via `linear_graphql` tool | Engine performs mutations (labels, reactions, comments, PR creation) |
| Priority | Linear priority field (1–4), oldest-first tiebreak | None (FIFO order of board items) |
| Dependency blocking | Native: `Todo` with non-terminal `blocked_by` not dispatched | Engine `checkDependencies` via GraphQL `trackedIssues`/`blockedBy` |
| State abstraction | Configurable `active_states`/`terminal_states` (string names) | Fixed board column names = pipeline stages |

**Fabrik is better here:** Rich durable state (labels, reactions) survives restarts with full pipeline context. Engine-level PR lifecycle management is reliable and doesn't depend on agent instruction.

**Symphony is better here:** Clean read-only tracker model — the engine never mutates Linear state, which removes a whole class of concurrency bugs and simplifies testing. Configurable `active_states`/`terminal_states` means the state machine isn't hardcoded to specific column names; a team can rename their board without touching config.

**Value-neutral:** Both are deliberately tracker-coupled rather than tracker-agnostic.

---

### 2. Workflow Definition Model

| Dimension | Symphony | Fabrik |
|---|---|---|
| Definition | Single `WORKFLOW.md` per project | Multiple `.fabrik/stages/*.yaml` per project |
| Config location | YAML front matter in same file | Separate `.fabrik/config.yaml` |
| Prompt | One Liquid template with `issue` + `attempt` variables | Per-stage prompt; skills can augment it |
| Dynamic reload | Required — file-watch, live re-apply without restart | Not supported — restart required |
| Per-invocation flags | `codex.*` (global) | Per-stage: model, max_turns, allowed_tools, etc. |
| Skills system | `.codex/skills/<name>/SKILL.md` — loaded by agent, not engine | `.fabrik/plugin/skills/<name>/SKILL.md` — loaded at stage invocation by engine |

**Fabrik is better here:** Per-stage customization is a major differentiator. A different model, turn budget, and tool allowlist per stage means Research can run cheaply with read-only tools while Implement gets full write access. Skills are loaded by the engine before each stage invocation — the agent doesn't need to discover or load them itself.

**Symphony is better here:** Dynamic reload is a hard requirement in Symphony's spec — prompt and config changes apply without restarting the daemon or disrupting in-flight sessions. Fabrik restarts can interrupt long-running stages. Symphony's Liquid template rendering with strict unknown-variable errors is also cleaner than Fabrik's plain-text substitution (silent failure on missing variables).

**Value-neutral:** Both keep the workflow definition in-repo and version-controlled.

---

### 3. Stage / Lifecycle Model

| Dimension | Symphony | Fabrik |
|---|---|---|
| Pipeline model | Single-agent loop; agent decides stages/state transitions | Explicit linear pipeline; engine advances through stages |
| Completion signal | Agent writes tracker state to `Human Review`/terminal | Agent emits `FABRIK_STAGE_COMPLETE` marker |
| Continuation | In-worker (same thread, continuation prompt) up to `max_turns` | Separate invocations per stage; context files bridge context |
| Human review handoff | Agent moves issue to `Human Review` state | Engine gates on `fabrik:awaiting-review` + PR reviewer submissions |
| Rework | Agent handles `Rework` state via WORKFLOW.md routing | Engine dispatches Review comment invocation on PR comments |

**Fabrik is better here:** Enforced stage discipline prevents an agent from skipping validation. The multi-stage approach naturally enables the Plan stage before Implement. Per-stage primitives (read-only mode, post-to-PR, CI gate) are engine-enforced — they apply regardless of what the agent decides to do. An agent can't accidentally skip Validate.

**Symphony is better here:** Simpler for teams who don't need enforced SDLC stages. The single-loop model means the agent can adapt its approach per issue — it can decide to skip research on a trivial bug or iterate on a plan without a separate pipeline stage. Less configuration overhead for smaller teams.

**Value-neutral:** Different philosophies about where workflow intelligence lives — in the engine (Fabrik) vs. in the prompt (Symphony). Neither is objectively correct; it depends on how much you trust the agent vs. the pipeline.

---

### 4. Workspace and Worktree Management

| Dimension | Symphony | Fabrik |
|---|---|---|
| Workspace location | `workspace.root/<sanitized_identifier>` | `.fabrik/worktrees/<owner>-<repo>/issue-N/` |
| VCS model | VCS-agnostic; git clone done via `after_create` hook | Git-native: bare clone + worktree on `fabrik/issue-N` |
| Context injection | None — agent has fresh workspace | `.fabrik-context/` files injected per stage (issue body, prior outputs, PR desc, codebase diff) |
| Lifecycle hooks | `after_create`, `before_run`, `after_run`, `before_remove` | None — update-from-main on each stage; stash/restore for read-only stages |
| Path safety | Explicit invariants in spec: workspace key sanitized, path must stay under root | Worktree path under `.fabrik/worktrees/`; no explicit root-containment check documented |

**Fabrik is better here:** Context file injection is a major differentiator. Prior stage outputs, the codebase diff since last stage, and the PR description are all written to `.fabrik-context/` before each invocation — the agent doesn't need to fetch them or remember them across session boundaries. Git-native branching means the issue branch is immediately push-ready for a PR; no hook required.

**Symphony is better here:** Workspace lifecycle hooks (`after_create`, `before_run`, `after_run`, `before_remove`) are clean and give operators flexibility for custom toolchain setup, environment bootstrapping, and post-run cleanup without patching the daemon. VCS-agnostic design is more portable — it works with Mercurial or a fresh checkout or a mounted NFS path. Symphony's spec also formalizes path safety (workspace_path must have workspace_root as a prefix) as a required pre-launch invariant; Fabrik's worktree containment is implicit.

**Value-neutral:** Both preserve workspaces across runs for the same issue.

---

### 5. Concurrency, Retries, Restart Recovery, and Idempotency

| Dimension | Symphony | Fabrik |
|---|---|---|
| Concurrency limit | Global `max_concurrent_agents` (default 10) + per-state overrides | Global `MaxConcurrent` (default 5), no per-stage override |
| Retry model | Exponential backoff: `min(10000 × 2^(attempt−1), max_retry_backoff_ms)`; continuation retries (1s after normal exit) | Fixed `max_retries` count per stage, no backoff |
| Restart recovery | Fresh poll + reuse workspaces; no retry queue restored | Labels + reactions are durable; `itemstate.Store` is in-memory but rocket reactions provide cross-restart idempotency |
| Idempotency | `claimed` set prevents duplicate dispatch | `stage:X:in_progress` + rocket reactions; `itemstate.Store` (`ProcessedComments`, `LastAttemptAt`) for within-session deduplication |
| Stall detection | Yes — kills stalled sessions after `stall_timeout_ms`, schedules retry | Yes — 15-minute inactivity timeout kills inactive sessions |

**Fabrik is better here:** Durable state via labels means an accurate picture of in-flight work after restart — not "re-poll and hope." Rocket reactions prevent re-processing comments across restarts even when the in-memory `itemstate.Store` is rebuilt. An operator can read the issue labels on github.com and understand exactly what state the pipeline left it in.

**Symphony is better here:** Exponential backoff is more sophisticated than Fabrik's fixed retry count. Symphony's formula is well-calibrated for transient failures; Fabrik's fixed count can thrash when the failure is persistent. Per-state concurrency controls are operationally useful. The continuation retry (1-second re-poll after normal worker exit) ensures issues don't stall waiting for the next full poll tick.

**Value-neutral:** Both use in-memory runtime state as the primary dispatch structure, which is rebuilt on restart.

---

### 6. State Model

| Dimension | Symphony | Fabrik |
|---|---|---|
| Authoritative state | Tracker (Linear state field) | GitHub labels + board column |
| Runtime state | In-memory orchestrator only | In-memory `itemstate.Store` (per-item invocation tracking, processed comments) + durable GitHub labels/reactions |
| State visibility | Tracker state field + optional HTTP API | GitHub issue labels visible to all collaborators |
| Recovery state | None (fresh poll on restart) | Labels + reactions survive restart |
| State granularity | Active / terminal / non-active (3 categories) | 20+ labels encoding per-stage, per-gate state |

**Fabrik is better here:** Rich label state is inspectable on github.com by any collaborator without tooling. Labels survive restarts with full pipeline context — which stage was in progress, which gates were open, whether the issue was paused. Multiple gates (CI, review, merge) are encoded as distinct states rather than folded into the tracker's stage field.

**Symphony is better here:** Simpler model — state transitions are visible in Linear where the team already works. No proprietary label system for collaborators to learn. Symphony's model also avoids the label-mutation complexity that Fabrik's engine carries.

**Value-neutral:** Both deliberately couple to their respective trackers for state storage.

---

### 7. Observability and Operator UX

| Dimension | Symphony | Fabrik |
|---|---|---|
| Runtime status | Optional HTTP server (`/api/v1/state`, `/api/v1/<id>`, `POST /api/v1/refresh`) | TUI in terminal; no HTTP API |
| Token accounting | Per-session + aggregate input/output/total tokens, runtime seconds | Per-invocation cost (USD) + aggregate session totals; surfaced in TUI and stdout logs |
| Rate limit tracking | Yes — latest Codex/Claude rate-limit payload from agent | GitHub REST + GraphQL limits tracked and logged each poll; TUI shows GraphQL rate stats |
| Log format | Structured, required: `issue_id`, `issue_identifier`, `session_id` | Structured with `[#N tag]` prefix |
| Config reload | Dynamic, no restart required | Requires restart |

**Symphony is better here:** The HTTP observability API is substantially better than a TUI for scripting, CI monitoring, and dashboards. The per-issue debug endpoint (`/api/v1/<id>`) is particularly useful for diagnosing stuck issues without tailing logs. Symphony tracks Codex/Claude API rate limits from the agent side; Fabrik tracks only GitHub API rate limits, leaving Claude rate-limit pressure invisible.

**Fabrik is better here:** The TUI is on by default with contextual per-issue status, stage names, cost (USD), and turn counts. Token accounting (per-stage invocation and aggregate session totals) and GitHub rate limit stats are surfaced in the TUI and logs without any configuration. Fabrik's GitHub rate-limit backoff system (two-threshold hysteresis) is more sophisticated than Symphony's spec describes.

---

### 8. Trust / Safety Posture

| Dimension | Symphony | Fabrik |
|---|---|---|
| Permission model | Configurable `approval_policy` (Codex-defined), `thread_sandbox`, `turn_sandbox_policy` | `--permission-mode dontAsk` or `--dangerously-skip-permissions` (via `fabrik:unrestricted`) |
| Workspace isolation | Explicit spec invariants: agent cwd == workspace_path, path within root | Worktree isolation; Claude Code runs in worktree cwd |
| Harness hardening | Section 15 explicitly prompts implementors to document trust posture and list hardening options | Not explicitly documented |
| Secret handling | `$VAR` indirection in config, explicit "don't log tokens" requirement | `.env` with gitignore, `$VAR_NAME`-style in config.yaml |

**Symphony is better here:** The spec explicitly requires each implementation to document its trust posture (Section 15) and enumerate hardening options. The workspace path safety invariants are formally specified (workspace_path must have workspace_root as a strict prefix). Sandbox config is more granular, with per-thread vs. per-turn policies. Fabrik's posture is functional but not formally documented.

**Fabrik is better here:** Nothing notable.

**Value-neutral:** Both are targeted at trusted environments in practice. Notably, the reference Symphony WORKFLOW.md uses `approval_policy: never` — auto-approve everything — so the granular sandbox config is there if you want it, but the default is wide open.

---

### 9. PR Lifecycle Integration

| Dimension | Symphony | Fabrik |
|---|---|---|
| PR creation | Agent opens PR via tool call (part of WORKFLOW.md prompt) | Engine creates draft PR (Implement stage), marks ready |
| PR review gate | WORKFLOW.md routes `Human Review` state; agent polls for PR feedback | Engine `fabrik:awaiting-review` gate; waits for reviewer submissions or timeout |
| CI gate | Agent polls CI as part of WORKFLOW.md logic | Engine `fabrik:awaiting-ci` + conjunctive completion label |
| Merge gate | Agent handles merge conflicts in `Rework` flow | Engine `fabrik:rebase-needed` + rebase reinvoke loop |
| Merge trigger | Agent calls merge after `Merging` state via `land` skill | Engine auto-merges on Validate completion if `fabrik:yolo` |

**Fabrik is better here:** First-class engine-level gates for CI, review, and merge conflicts are reliable and don't depend on the agent being well-instructed or not hallucinating a CI status. The CI gate's conjunctive completion label (`fabrik:awaiting-ci`) is particularly clean — the stage isn't marked complete until CI passes, rather than the agent polling in a loop.

**Symphony is better here:** Agent-driven PR lifecycle is more flexible. The `land` skill can handle complex merge scenarios (squash vs. merge commit, rebase strategy). The agent can apply judgment — push back on a review comment instead of just fixing it, or decide a CI failure is a pre-existing flake.

**Value-neutral:** Both end up at the same outcome (PR landed, issue closed); the paths differ.

---

### 10. Comment / Human-in-the-Loop Interaction

| Dimension | Symphony | Fabrik |
|---|---|---|
| Comment processing | None at engine level; agent uses workpad comment as scratchpad | Engine-level: users comment on issues/PRs to steer stages |
| Human pause | No native pause mechanism | `fabrik:paused` + `fabrik:awaiting-input`; `FABRIK_BLOCKED_ON_INPUT` |
| Feedback loop | Agent polls PR review comments in WORKFLOW.md | Engine dispatches comment invocation with `comment_prompt` / `comment_skill` |
| Mid-stage control | None | Comment processing with separate turn budget and skill |

**Fabrik is better here:** Clearly superior for human-in-the-loop scenarios. Comment processing with per-stage `comment_prompt` and `comment_skill` lets users steer an in-progress stage without restarting it. `FABRIK_BLOCKED_ON_INPUT` is a clean escape hatch when the agent needs a decision before it can continue.

**Symphony is better here:** Nothing notable. Symphony explicitly defers human interaction to the agent and prompt.

**Value-neutral:** Symphony's workpad comment pattern — a single persistent comment per issue that the agent keeps updated as a scratchpad — is a useful UX pattern for showing agent progress without a TUI. Fabrik's stage comments serve a similar role but are multi-comment living documents rather than a single mutated scratchpad.

---

## Ideas Worth Stealing

These are candidate follow-up engineering items. Triage and file separately.

| # | Idea | Tag | Rationale |
|---|---|---|---|
| 1 | **Dynamic config reload** — detect stage YAML changes and re-apply without restart | `adopt` | Reduces operator friction significantly. Symphony makes this a hard requirement. Fabrik restarts are disruptive to in-flight sessions and create an unnecessary operational hazard for prompt iteration. |
| 2 | **HTTP observability API** — `/api/v1/state` endpoint with running sessions, retry queue, token totals | `adopt` | Much better than TUI for scripting, CI monitoring, and dashboards. The per-issue debug endpoint is particularly useful for diagnosing stuck issues remotely. |
| 3 | **Claude API rate-limit visibility** — surface Claude API rate-limit stats from the agent side (analogous to Symphony's per-session rate-limit payload) | `consider` | Fabrik already tracks per-invocation token cost and GitHub API rate limits. The gap is Claude-side rate-limit pressure: when sessions slow down due to Claude throttling, Fabrik logs nothing. Filling this gap would require parsing rate-limit events from Claude Code output. |
| 4 | **Exponential backoff retries** — `min(10000 × 2^n, max_retry_backoff_ms)` replacing fixed retry count | `consider` | More graceful under transient failures. Fabrik's fixed retry count can thrash when the underlying failure is persistent (e.g., a flapping remote). |
| 5 | **Continuation retry** — short re-poll (1s) after normal worker exit | `consider` | Symphony re-checks issue state immediately after a clean exit instead of waiting for the next full poll tick. Less critical for Fabrik since stage completion is explicit, but would help for faster comment detection. |
| 6 | **Per-stage concurrency controls** — `max_concurrent_agents_by_state` analog | `consider` | Lets operators cap expensive stages (e.g., Implement) without throttling cheap ones (Research). Value scales with the number of concurrent issues being processed. |
| 7 | **Workspace lifecycle hooks** — `after_create`, `before_run`, `after_run`, `before_remove` | `consider` | Clean operator extension point for custom toolchain setup and environment bootstrapping. Fabrik's worktree model handles most cases, but hooks would let users add non-Go toolchain setup without forking. |
| 8 | **Explicit workspace path safety invariants** — formal workspace-root containment check | `consider` | Symphony formalizes "workspace_path must be under workspace_root" as a required pre-launch check. Worth adding as an explicit assertion in Fabrik's worktree code. Low cost, eliminates a class of misconfiguration. |
| 9 | **Priority-aware dispatch** — issue priority field drives dispatch ordering | `watch` | Linear has a native priority field. GitHub Projects supports a priority custom field but Fabrik doesn't read it. Worth watching once Fabrik's scale warrants prioritization beyond FIFO. |
| 10 | **SSH worker extension** — run agent subprocess on remote hosts over SSH | `watch` | Compelling scalability story for orgs with expensive build infra or GPU requirements. Symphony's Appendix A defines this cleanly. Not a near-term Fabrik need but worth tracking the design pattern. |
| 11 | **Single `WORKFLOW.md` as a Fabrik mode** — collapse multi-stage YAML into one file for smaller orgs | `reject` | Fabrik's per-stage YAML is the intentional differentiator. Smaller orgs can already achieve simplicity with minimal stage configs. Collapsing to a single file sacrifices per-stage control (model, turn budget, tools) for marginal config reduction. |
| 12 | **Tracker-reads-only / agent-writes model** — engine doesn't mutate tracker state | `reject` | Fabrik's engine-driven labels, reactions, PR creation, and auto-merge are deliberate and load-bearing. Delegating these to the agent loses durable restart semantics, removes CI/review/merge gates as engine primitives, and requires the agent to be reliably instructed on every state transition. |

---

## Summary

Fabrik and Symphony converge on the same core insight — isolated per-issue workspaces driven by a polling daemon — but diverge sharply on where workflow intelligence lives. Fabrik puts it in the engine (explicit stage pipeline, engine-owned PR lifecycle, durable label state); Symphony puts it in the prompt (single-loop agent, tracker-state-driven routing, agent-owned writes).

Symphony's cleaner read-only tracker model, dynamic config reload, HTTP observability API, and token accounting are all genuine gaps in Fabrik. The `adopt` items above are not nice-to-haves; they are operational necessities at scale that Fabrik needs to close.

Fabrik's multi-stage pipeline, context file injection, engine-level gates (CI, review, merge), and comment-processing subsystem are genuine differentiators that Symphony doesn't have and would be hard to add within Symphony's single-loop model. These are the architectural moat.

The most interesting design tension: Symphony's read-only tracker model (items 11–12 tagged `reject`) is architecturally cleaner and would simplify a lot of Fabrik's code. The reason to reject it isn't that it's a bad design — it's that Fabrik's engine-owned writes are load-bearing for durable state and the gate system. If Symphony adds the equivalent of Fabrik's CI gate, it will need engine-owned writes too.

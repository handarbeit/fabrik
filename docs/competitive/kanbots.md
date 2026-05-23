# KanBots — Competitive Comparison

| Field | Value |
|-------|-------|
| Subject | [`leodavinci1/kanbots`](https://github.com/leodavinci1/kanbots) |
| KanBots commit pinned | `1b7b8106e6ef4190bfe64054b27494118951ce1e` (2026-05-22) |
| Compared on | 2026-05-22 |
| Fabrik commit | `56c85ad0` (HEAD of `main` at time of research) |
| Author | Fabrik Implement (issue #758) |
| Maturity note | KanBots first commit 2026-04-25; 4 weeks old at time of analysis. Treat claims as "current behavior, likely to shift." See revisit triggers at the end. |

## What this is

Internal, opinionated comparison of Fabrik against KanBots. Both projects share the same core premise — kanban board as source of truth, AI agent runner as executor, per-task git worktrees — and diverge on almost every architectural contract. KanBots is local-first (Electron, SQLite) where Fabrik is daemon-and-GitHub; KanBots is single-dispatch where Fabrik is multi-stage pipeline; KanBots defaults to `bypassPermissions` where Fabrik defaults to `dontAsk`. This document maps the agreements, the divergences, and which KanBots ideas are worth stealing, deferring, or rejecting.

## What this is not

- **Not public-facing marketing copy.** Frank, internal, sometimes uncomfortably honest. KanBots is doing several things better than Fabrik; that is named below.
- **Not an ADR.** Adopting any specific idea here warrants its own follow-up ADR.
- **Not a stability guarantee.** KanBots is 4 weeks old; a follow-up comparison should be run when KanBots reaches v1.0 or 6 months stable.
- **Not a list of follow-up issues.** The Ideas section is a triage surface for the maintainer. Filing follow-ups is a separate manual step.

## Source material

- `packages/dispatcher/src/worker.ts` (read in full from pinned commit) — agent invocation, session resume, cost accounting, decision-prompt flow.
- `packages/dispatcher/src/worktree.ts` — worktree lifecycle, branch naming, promotion paths.
- `packages/dispatcher/src/containment.ts` — path-scope containment guard.
- `packages/local-store/src/schema.ts` — SQLite schema; tables: `local_issues`, `cards`, `agent_runs`, `agent_events`, `autopilot_sessions`, `promotions`.
- `packages/mcp/src/index.ts` — MCP server tool catalogue.
- `packages/llm/src/adapter.ts` — `AgentCliAdapter` interface and provider catalogue.
- `docs/agents.md` — deployment and containment documentation.
- `README.md` and `https://www.kanbots.dev/` — public-facing claims cross-checked against source.

---

## KanBots architecture, in one page

KanBots is a TypeScript monorepo (`pnpm` workspaces, 8 packages), delivered as a local-first Electron app.

1. **Deployment model** — Electron app; renderer ↔ main process over IPC. No HTTP server for internal communication. A local-only HTTP shim on `127.0.0.1:<random-port>` backs the MCP bridge and uses a per-session bearer token. Runs on macOS, Linux, Windows. Zero cloud account required for the OSS tier.
2. **State store** — SQLite at `.kanbots/db.sqlite` via `better-sqlite3` (19 schema migrations). Key tables: `local_issues` (issue metadata), `cards` (UI state + decision payload + status), `agent_runs` (one row per invocation with `sessionId`, cost totals, stop reason), `agent_events` (streamed events: `text`, `tool_use`, `tool_result`, `decision`, `result`, `rate_limit`), `autopilot_sessions`, `promotions`.
3. **GitHub integration** — GitHub mode syncs via Octokit REST (PAT). Board columns (Backlog / In Progress / Review / Done / Inbox) are KanBots-internal; in GitHub mode, status moves are mirrored as `status:*` label edits on GitHub **Issues** (not GitHub Projects — Projects GraphQL is not used). SQLite remains the source of truth even in GitHub mode.
4. **Agent invocation** — `claude -p --output-format stream-json --verbose --permission-mode bypassPermissions --append-system-prompt "<issue context>" --model <model> [--resume <sessionId>] [--mcp-config <path>]`. `bypassPermissions` is the **default posture** for every dispatch (`packages/dispatcher/src/worker.ts`). Containment is enforced externally: (a) pre-push hook in every worktree; (b) path-scope containment guard in the dispatcher that watches `tool_use` events and can warn, pause, or block on out-of-worktree edits.
5. **Session resume** — each invocation produces a `session` event with a session ID. The dispatcher persists it on the `agent_runs` row. On retry or continuation, `claude --resume <sessionId>` is passed so full context is reused without resending history.
6. **Worktrees** — branch naming: `kanbots/issue-<n>-<runId>` — **per run, not per issue**. Each dispatch creates a new worktree at `.kanbots/worktrees/issue-<n>-<runId>/`. No fetch/rebase on dispatch — the worktree branches from the repo default branch at creation time. No automatic conflict resolution. Promotion paths: (a) merge/cherry-pick onto active branch + remove worktree; (b) push + open draft PR (GitHub mode); (c) discard + delete branch.
7. **Human-in-the-loop — Decision Prompts** — the agent emits a `decision` event → dispatcher marks card `awaiting_input`, stores `DecisionPayload { question, options: [{value, label}] }` on the card, pauses stdin. User answers in the UI; the `cards:resolve` handler writes the choice to agent stdin; run continues. Decision state survives app restart (SQLite). Slash commands in the reply box: `/spec`, `/review`, `/split`.
8. **Autopilot** — two modes:
   - `feature-dev`: up to 4 parallel slots; round-robin over a user-defined persona roster. Each slot atomically claims the next persona and dispatches an agent on the issue in its own worktree. Agents can `splitIssue` to create child cards. Configurable session cost budget; stop button halts all children.
   - `qa`: runs user-defined check commands (typecheck / tests / lint / build / e2e), dispatches fix runs for failing checks, repeats until all pass or cost budget is hit.
9. **Cost management** — every `result` event carries USD cost. The dispatcher accumulates per-run and per-session totals. Budget caps in `.kanbots/config.json` (`runCostBudgetUsd`, `sessionCostBudgetUsd`); run terminates with `stopReason: 'cost-budget'` on cap hit. Rate-limit hits surface as `rate_limit` events; dispatcher broadcasts `cooldown:changed`.
10. **Multi-provider** — `AgentCliAdapter` interface with two implementations: `ClaudeAdapter` (`claude -p`) and `CodexAdapter` (`codex exec`). User-selectable per card.
11. **MCP server** — `kanbots-mcp-server` standalone stdio process; uses the local HTTP tool bridge. Tools: issue CRUD (`listIssues`, `getIssue`, `createIssue`, `updateIssue`, `moveIssueStatus`, `archiveIssue`, `splitIssue`), agent run management (`dispatchAgent`, `stopAgentRun`, `listAgentRuns`), decision handling (`listPendingDecisions`, `resolvePendingDecision`). Compatible with Cursor, Claude Desktop, and Claude Code CLI (`--mcp-config`).
12. **No pipeline / no multi-stage model** — KanBots has no sequential pipeline concept. One dispatch = one agent run against one card. The closest analog is Autopilot `feature-dev` (parallel persona-flavored dispatches on the same issue), but that is concurrent, not sequential, and has no inter-run context handoff.
13. **Pricing** — OSS tier (MIT, free), Cloud ($19/seat/month, adds real-time presence, cross-device sync, Slack notifications, SSO), Enterprise (custom).

## Fabrik architecture, in one page

Same outer shape — long-running daemon, per-issue workspaces, AI coding agent, durable state — different internal contracts. Cross-references: [`docs/state-machine.md`](../state-machine.md), [`docs/stage-lifecycle.md`](../stage-lifecycle.md), and the `adrs/` directory.

1. **Stage configs** — N per-stage YAMLs in `.fabrik/stages/`, each with its own `prompt`, optional `skill`, `model`, `max_turns`, `allowed_tools`, lifecycle flags (`post_to_pr`, `create_draft_pr`, `read_only`, `cleanup_worktree`, `wait_for_reviews`, `wait_for_ci`, `mark_pr_ready_on_complete`, `auto_advance`), and optional `comment_prompt` / `comment_skill` / `comment_max_turns` (ADR 004). No hot reload.
2. **Tracker client** — GitHub Projects, single GraphQL fetch per poll (`github/project.go`) — items, comments, linked PRs in one call. The engine performs all tracker writes: labels, reactions, status, draft PR creation, mark-ready. The agent emits markers (`FABRIK_STAGE_COMPLETE`, `FABRIK_BLOCKED_ON_INPUT`, `FABRIK_ISSUE_UPDATE_*`, `FABRIK_SUMMARY_*`) on stdout; the engine scrapes and translates them.
3. **Per-issue pipeline** — `engine/item.go` runs one stage per invocation. Stage selection is driven by `stage:<name>:complete` / `stage:<name>:in_progress` labels. The default pipeline is Research → Plan → Implement → Review → Validate → Done; each stage has its own prompt, skill, model, and tool allowlist.
4. **Worktrees** — bare clone at `.fabrik/repos/<owner>-<repo>.git`; per-issue worktree at `.fabrik/worktrees/<owner>-<repo>/issue-N/` on branch `fabrik/issue-N` (per issue, not per run). Engine fetches and rebases onto the base branch on each stage invocation unless the worktree is dirty. No pre-push hook.
5. **Agent invocation** — Claude Code via `claude` CLI (`engine/claude.go`). Default: `--permission-mode dontAsk` + enumerated `allowed_tools`. `fabrik:unrestricted` label flips to `--dangerously-skip-permissions`. `max_wall_time` enforces SIGTERM→SIGKILL deadlines; a 15-minute inactivity timeout applies regardless.
6. **Durable state** — GitHub labels (`stage:*:complete`, `fabrik:paused`, `fabrik:awaiting-input`, `fabrik:awaiting-review`, `fabrik:awaiting-ci`, `model:*`, `effort:*`, `fabrik:yolo`, `fabrik:cruise`, `base:*`, etc.) and comment reactions (👀/🚀, ADR 009). No SQLite, no proprietary store. Everything inspectable on github.com without tooling.
7. **Inter-stage context handoff** — `.fabrik-context/` files injected before each stage invocation: `issue.md` (spec), `stage-<Name>.md` (prior stage output), `pr-description.md`, `codebase-changes.md` (files changed on base branch since last stage). No KanBots analog.
8. **Comment-driven revision** — per-stage `comment_prompt` / `comment_skill` / `comment_max_turns`; reaction-based durable processed-state (ADR 009); living-document stage comments (ADR 012).
9. **Cost / observability** — `[#N tag] ...` stdout prefix. No token accounting. No cost budget caps. No rate-limit surfacing beyond hysteresis logic in the engine.
10. **No MCP surface** — Fabrik does not expose the board as an MCP server.
11. **Pricing** — OSS (MIT, free), self-hosted. No cloud tier.

---

## Per-axis comparison

Each axis: a compact table, a short narrative, and an honest verdict.

### 1. Architecture & deployment model

| | KanBots | Fabrik |
|---|---|---|
| Delivery | Local-first Electron app (macOS / Linux / Windows) | Server daemon CLI (`fabrik run`) |
| Process model | Electron main + renderer over IPC; no HTTP server | Single Go process; poll loop drives all work |
| Persistence | SQLite at `.kanbots/db.sqlite` | No local DB; durable state lives on github.com |
| Config | `.kanbots/config.json` + SQLite personas | `.fabrik/stages/*.yaml` + stage YAML defaults baked in binary |
| Restart recovery | SQLite survives; decision state on the card; `--resume <sessionId>` for sessions | GitHub labels + rocket reactions (ADR 009); no session resume |
| Install footprint | Electron runtime (~300 MB), local SQLite | Single Go binary; no runtime dependency beyond `git` and `claude` |

**Where KanBots is better**: Electron delivers a real GUI (React 19 + Vite 6) with a zero-config install for individual developers. Decision state and cost totals survive restarts natively. Session resume via `--resume` eliminates full-context resend on retry.

**Where Fabrik is better**: zero GUI footprint, purpose-built for shared team boards. The daemon model scales to N concurrent issues without per-user app instances. Durable state on github.com is inspectable by any team member without running the daemon.

**Different, value-neutral**: local-first vs. server daemon is a fundamental audience split. KanBots optimizes for individual developer workflow; Fabrik optimizes for shared team SDLC.

### 2. Board / source-of-truth

| | KanBots | Fabrik |
|---|---|---|
| Primary board | SQLite (`cards` table); columns: Backlog / In Progress / Review / Done / Inbox | GitHub Projects (GraphQL); board columns = stage names |
| GitHub integration | GitHub Issues REST via Octokit PAT (github mode); status moves mirrored as `status:*` label edits | GitHub Projects GraphQL; engine is the single writer (labels, status, reactions, comments) |
| GitHub Projects | **Not used.** GitHub mode = Issues only. | **Native.** The Project board is the source of truth; no proprietary store. |
| Multi-repo support | Per-board config; one repo per board instance | Per-repo bare clone; one engine instance drives N repos concurrently |
| Human board edits | UI (drag-and-drop on columns) | github.com (drag-and-drop) or `gh` CLI |

**Where KanBots is better**: the proprietary SQLite board is faster to manipulate from the local app and works offline. The Inbox column and drag-and-drop import from GitHub Issues are practical onboarding affordances.

**Where Fabrik is better**: no proprietary board to maintain. GitHub Projects is the board — existing team workflow, existing permissions, no migration, no sync lag. Fabrik's "your existing Project board is the driver" is a genuine zero-overhead value proposition for teams already on GitHub Projects.

**Verdict — philosophically different.** KanBots chose SQLite for speed and offline capability; Fabrik chose GitHub-native for zero-overhead integration. KanBots' GitHub mode is additive sync; Fabrik's github.com dependency is by design (ADR 002).

### 3. Workflow / pipeline model

| | KanBots | Fabrik |
|---|---|---|
| Pipeline | **None** — one dispatch per card | Multi-stage YAML pipeline (Research → Plan → Implement → Review → Validate) |
| Stage configuration | Per-dispatch model selection only | Per-stage: `prompt`, `skill`, `model`, `max_turns`, `allowed_tools`, lifecycle flags, `comment_prompt` |
| Routing | User-initiated dispatch or Autopilot | Engine-driven: `stage:*:complete` / `stage:*:in_progress` labels |
| Prompt surface | Single system prompt + issue context (append mode) | One prompt per stage + one `comment_prompt` per stage |
| Persona flavoring | Autopilot personas (Product Manager, Senior Engineer, etc.) | Per-stage `skill` loads stage-specific playbook |
| Hot reload | Not documented | Not supported — restart required |

**Where KanBots is better**: no ceremony. Dispatch an agent, get a result. For one-shot tasks ("fix this bug"), the single-dispatch model has dramatically less overhead than a 5-stage pipeline. Autopilot `feature-dev` with parallel personas is a creative alternative to sequential stages.

**Where Fabrik is better**: the pipeline encodes SDLC discipline. Each stage runs with the right model and tool allowlist (e.g., `research` stage uses Sonnet and read-only tools; `implement` stage uses the full tool set). Inter-stage context handoff means the Implement agent reads the Research and Plan outputs directly. No KanBots analog for this.

**Different, value-neutral**: single-dispatch vs. multi-stage is an audience/use-case split. Short tasks: KanBots wins on overhead. Complex multi-role SDLC tasks: Fabrik's pipeline is a meaningful structural advantage.

### 4. Agent lifecycle per task

| | KanBots | Fabrik |
|---|---|---|
| Invocations per card | One per dispatch (continuation via `--resume`); Autopilot: one per persona per round | One per stage (N stages = N invocations per issue) |
| Context across invocations | `--resume <sessionId>` (reuses claude session; avoids full-history resend) | `.fabrik-context/stage-<Name>.md` written by engine before each invocation |
| Child card creation | Yes — agent can call `splitIssue` tool | Not supported — issues are atomic |
| Stage failure / retry | Dispatcher retries the same `agent_run`; new run on explicit re-dispatch | Engine re-invokes the failed stage on next poll tick if `stage:*:complete` not set |
| Mid-stage interruption | Decision Prompt pauses stdin; state on SQLite card survives restart | `FABRIK_BLOCKED_ON_INPUT` marker → engine sets `fabrik:awaiting-input`; engine resumes on next comment |

**Where KanBots is better**: `--resume <sessionId>` is a material cost-reduction win — Fabrik re-sends full context on every invocation. `splitIssue` enabling child cards for QA loops is a creative mechanism for self-spawning work decomposition.

**Where Fabrik is better**: per-stage context-file handoff is explicit and human-readable. The Research output is a committed artifact that the Plan stage reads; Plan output is committed and Implement reads it. KanBots' single-session model has no equivalent for structured inter-agent handoff.

**Different**: session resume (KanBots) vs. context-file handoff (Fabrik) are two valid approaches to continuity. They solve different problems: resume minimizes token cost on retry; context files enable structured handoff across stage boundaries.

### 5. Worktree & git management

| | KanBots | Fabrik |
|---|---|---|
| Branch naming | `kanbots/issue-<n>-<runId>` (per run) | `fabrik/issue-N` (per issue) |
| Worktree path | `.kanbots/worktrees/issue-<n>-<runId>/` | `.fabrik/worktrees/<owner>-<repo>/issue-N/` |
| Repo bootstrap | User-supplied (clone before use) | Engine bare-clones to `.fabrik/repos/<owner>-<repo>.git` on first access |
| Sync before dispatch | None — worktree branches from default at creation time; no rebase | `updateWorktreeFromMain` fetches and rebases onto base branch (skipped if dirty) |
| Conflict handling | Remains in worktree; user handles before promotion | Merge markers passed through to Claude for resolution |
| Promotion paths | Merge/cherry-pick onto active branch; open draft PR; discard | Engine-managed PR lifecycle (`create_draft_pr`, `mark_pr_ready_on_complete`) |
| Pre-push safety | **Pre-push hook in every worktree** — prevents agent-driven publish | No hook; agent relies on `--permission-mode dontAsk` (cannot push without user's explicit allow) |

**Where KanBots is better**: per-run branch naming allows multiple in-flight attempts on the same card without worktree clobbering. The pre-push hook is a defense-in-depth measure that Fabrik lacks. Three explicit promotion paths (merge, draft PR, discard) give the user control before any code leaves the local machine.

**Where Fabrik is better**: per-issue branch naming (`fabrik/issue-N`) enables idempotent re-invocation across all stages — the branch accumulates commits from Research through Validate on one timeline. Automatic fetch/rebase keeps the worktree current with main; KanBots branches from default at creation time and may diverge silently across a long-running run.

**Fabrik verdict**: per-issue branch is the right choice for a multi-stage pipeline. Per-run branches would create N branches per issue across 5 stages — noise and fragmented git history.

### 6. State model

| | KanBots | Fabrik |
|---|---|---|
| Source of truth | SQLite (`cards` + `agent_runs` + `agent_events`) | GitHub labels + comment reactions + GitHub Project status |
| GitHub mode | Additive sync; SQLite remains authoritative | GitHub is the only store; no local DB |
| State inspectability | KanBots UI (local app) or direct SQLite query | github.com (labels, reactions, comments) — no tooling required |
| Decision state durability | SQLite survives app restart | `fabrik:awaiting-input` label + comment reaction survive daemon restart |
| Comment-state coupling | Decision event pauses stdin; resolution writes to stdin | 👀 / 🚀 reactions on each processed comment (ADR 009) |
| Cloud sync (Pro) | SQLite cloud sync ($19/seat) | N/A — GitHub *is* the cloud |

**Where KanBots is better**: SQLite is fast and local; offline mode works. Cloud Pro sync is a genuine team collaboration affordance. Decision state durability across app restarts is well-implemented.

**Where Fabrik is better**: GitHub-native state is readable by every team member without installing anything. Labels, reactions, and comments are first-class GitHub objects — filterable, searchable, webhook-able. The rocket-reaction model (ADR 009) provides per-comment idempotency across daemon restarts at finer granularity than SQLite's per-run rows.

**Different (ADR 002 counterpoint)**: KanBots chose SQLite to avoid GitHub API rate limits and enable offline use. Fabrik chose GitHub labels deliberately — the board is the state; no sync lag, no secondary store, no backup concern. Both are internally consistent choices with different tradeoffs.

### 7. Multi-provider / model support

| | KanBots | Fabrik |
|---|---|---|
| Providers | Claude Code (`claude -p`) + OpenAI Codex (`codex exec`) via `AgentCliAdapter` | Claude Code only (`claude` CLI) |
| Model selection | Per-card UI selection | Per-stage YAML (`model: sonnet|opus`) + `model:*` label override |
| Effort / thinking | Not documented | Per-stage `effort_level` (low/medium/high/max) + `effort:*` label override |
| Per-stage model | Not applicable (single dispatch) | Yes — Research/Review on Opus, Implement on Sonnet is a common pattern |

**Where KanBots is better**: multi-provider support is a real differentiator. Users who prefer Codex for Implement tasks and Claude for Review can mix within a single workflow. The `AgentCliAdapter` abstraction is clean and extensible.

**Where Fabrik is better**: per-stage model selection (e.g., `model: opus` for Research, `model: sonnet` for Implement) is a cost-management tool that KanBots' single-dispatch model can't replicate. Per-stage `effort_level` similarly optimizes thinking budget by task type.

**Watch**: Fabrik's hardcoded `claude` invocation would need significant refactoring to support additional providers. No current user demand for Codex support; this is a `watch` item, not urgent.

### 8. Human-in-the-loop / comment-driven revision

| | KanBots | Fabrik |
|---|---|---|
| Primary HITL mechanism | Decision Prompts (agent emits `decision` event → run pauses → UI shows options → user picks → stdin continues) | `FABRIK_BLOCKED_ON_INPUT` marker → `fabrik:awaiting-input` label → user comments on issue |
| Comment-driven continuation | Slash commands in UI (`/spec`, `/review`, `/split`) + free-text reply | Issue or PR comment → engine detects → `comment_prompt` / `comment_skill` invoked |
| Per-stage comment prompt | N/A | Yes — each stage defines its own `comment_prompt`, `comment_skill`, `comment_max_turns` |
| Durable "already handled" state | SQLite `agent_events` row | 👀 / 🚀 reactions (ADR 009) |
| Living-document outputs | Not documented | Stage comments edited in-place across iterations (ADR 012) |
| Edit-and-resubmit | Yes — user can edit prior reply before resubmitting | Not supported |

**Where KanBots is better**: Decision Prompts are a tight UX loop — the agent pauses, presents numbered options, user clicks, run resumes. This is more immediate than Fabrik's comment-based async cycle. Edit-and-resubmit is a nice quality-of-life feature.

**Where Fabrik is better**: per-stage `comment_prompt` / `comment_skill` / `comment_max_turns` is a meaningfully richer model. A comment during Research triggers a different prompt (and different skill) than a comment during Implement. KanBots' single-dispatch model has no per-stage comment routing. Reaction-based durable state (ADR 009) provides comment-level idempotency that KanBots' SQLite rows don't match at the same granularity.

**Different**: KanBots' Decision Prompts model is synchronous (stdin pause); Fabrik's comment model is asynchronous (poll and re-invoke). For interactive sessions, KanBots' model is lower-latency. For team workflows where the reviewer comments hours later, Fabrik's async model is more natural.

### 9. Cost management & observability

| | KanBots | Fabrik |
|---|---|---|
| Cost tracking | **Per-run + per-session USD totals** from `result` event; live cost meter in UI | **None** |
| Budget caps | `runCostBudgetUsd`, `sessionCostBudgetUsd` in `config.json`; terminates with `stopReason: 'cost-budget'` | **None** |
| Rate-limit handling | `rate_limit` event → `cooldown:changed` broadcast; queued dispatches delayed | Internal hysteresis (ADR 028) but not surfaced |
| Structured logging | Not documented | `[#N tag] ...` stdout prefix; not key=value |
| HTTP API / dashboard | None | None |
| Token accounting | Per-run accumulation in `agent_runs` | None |

This is the axis where Fabrik most clearly trails. KanBots' cost management story is complete and immediate: USD cost per run, configurable caps, explicit termination reason. Fabrik has no equivalent. For operators running many concurrent issues, unbounded cost is a real production risk.

**Where KanBots is better**: cost budget caps with `stopReason: 'cost-budget'` is production-essential. Live cost meter in the UI makes cost visible without external tooling. Rate-limit event surfacing is more explicit than Fabrik's hysteresis.

**Where Fabrik is better**: nothing on this axis. Fabrik is behind.

**Verdict — Adopt cost budget caps.** See Ideas section.

### 10. Trust & safety posture

| | KanBots | Fabrik |
|---|---|---|
| Default permission mode | **`bypassPermissions`** (equivalent to `--dangerously-skip-permissions`) | **`--permission-mode dontAsk`** |
| Containment layer | Pre-push hook (every worktree) + path-scope containment guard in dispatcher (warn/pause/off) | Tool allowlist (`allowed_tools` per stage) |
| Tool allowlist | Not applicable — `bypassPermissions` bypasses tool restrictions | Per-stage `allowed_tools` *replaces* default set; `fabrik:unrestricted` bypasses |
| Source for `bypassPermissions` claim | `packages/dispatcher/src/worker.ts` at pinned commit | N/A |
| Trust posture documentation | `docs/agents.md` in KanBots repo | `CLAUDE.md` (implicit), `docs/state-machine.md` (partial) |

**Where KanBots is better**: defense-in-depth at a different layer. The pre-push hook prevents autonomous publishing regardless of what the agent does — it's a process boundary, not a permission flag. The path-scope containment guard is an independent layer that watches `tool_use` events and can pause or block out-of-worktree edits.

**Where Fabrik is better**: the default `dontAsk` posture is more conservative than KanBots' default `bypassPermissions`. Fabrik's per-stage `allowed_tools` gives fine-grained control over what each stage can do (e.g., Research is read-only, Implement gets full tool set). KanBots has no per-dispatch tool restriction mechanism.

**Verdict — different but Fabrik's default posture is safer.** KanBots' `bypassPermissions` default with external containment is a deliberate architectural choice (containment outside the agent, not inside). Fabrik's `dontAsk` default with per-stage `allowed_tools` is tighter at the permission layer but lacks a physical pre-push backstop. Consider adopting the pre-push hook as a complementary layer.

### 11. Team collaboration

| | KanBots | Fabrik |
|---|---|---|
| Shared board | App instance per user; SQLite is local | Shared GitHub Projects board; any team member reads the same state |
| Real-time presence | Pro tier ($19/seat) | N/A — GitHub Projects has native presence |
| Cross-device sync | Pro tier (cloud sync) | N/A — GitHub is the sync layer |
| Notifications | Slack (Pro tier) | GitHub notifications (native) |
| SSO | Enterprise tier | N/A — GitHub SSO |
| Access control | OSS: local; Pro: account-based | GitHub org / repo permissions |
| OSS model | MIT | MIT |

**Where KanBots is better**: the Pro tier cloud features (real-time presence, cross-device sync, Slack notifications, SSO) are a complete team collaboration story for teams that don't want GitHub as their board. Explicit pricing ($19/seat) is transparent.

**Where Fabrik is better**: zero-overhead team collaboration. The GitHub Projects board is already shared, already permission-controlled, and already notifies team members via GitHub notifications. No per-seat charge for Fabrik itself (though GitHub's pricing applies).

**Verdict — different audience.** KanBots' collaboration story is self-contained and works for teams not invested in GitHub Projects. Fabrik's collaboration story is "GitHub is already your collaboration layer" — which is true for teams that have adopted GitHub Projects as their board.

### 12. MCP / external tool integration

| | KanBots | Fabrik |
|---|---|---|
| MCP server | **Yes** — `kanbots-mcp-server` (stdio, local HTTP bridge) | **No** |
| Tools exposed | Issue CRUD, agent dispatch, decision resolution, status moves | N/A |
| Compatible clients | Cursor, Claude Desktop, Claude Code CLI (`--mcp-config`) | N/A |
| Board as MCP resource | Yes — issues and cards exposed as resources | N/A |
| MCP auth | Per-session bearer token on local HTTP bridge | N/A |

**Where KanBots is better**: MCP server is a genuine integration story. Cursor and Claude Desktop users can query the KanBots board, dispatch agents, and resolve decisions without opening the KanBots UI. This is a meaningful developer-workflow integration for users embedded in those environments.

**Where Fabrik is better**: nothing on this axis. Fabrik has no MCP surface.

**Verdict — Watch.** MCP adoption is growing. Exposing the Fabrik board (or at least issue status) as an MCP server would unlock Cursor / Claude Desktop integration without shipping a GUI. Not urgent today, but worth revisiting as MCP becomes a standard IDE integration point.

---

## Ideas worth stealing

Triage tags: `adopt` / `consider` / `reject` / `watch`. One or two sentences; depth reserved for genuinely creative ideas.

### Adopt

1. **Cost budget caps per run and per session.** `adopt` — KanBots' `runCostBudgetUsd` / `sessionCostBudgetUsd` in `config.json`, with explicit `stopReason: 'cost-budget'` when the cap is hit, is the most user-visible feature gap between KanBots and Fabrik. For teams running Fabrik concurrently across many issues, unbounded cost is a production risk. Claude Code's invocation results include cost data; the engine already has what it needs to implement this. Adopt: add `max_run_cost_usd` and `max_session_cost_usd` to the engine config, emit a warning comment on the issue when the run terminates due to budget, and set `fabrik:paused` for user intervention.

### Consider

2. **Session resume via `--resume <sessionId>`.** `consider`, with depth — KanBots persists the `sessionId` from the `session` event on the `agent_runs` row and passes `claude --resume <sessionId>` on retry, eliminating full-context resend. Fabrik re-sends full context (issue body + prior stage outputs) on every invocation, which grows proportionally with pipeline depth. The engineering question: does `--resume` survive a worktree switch? Claude Code's session context is server-side (keyed on session ID), not filesystem-bound. If `--resume` works across worktrees, adding it to Fabrik's retry path would reduce token cost on failed-stage retries. Investigate before committing.

3. **Pre-push hook in every worktree.** `consider` — KanBots installs a pre-push hook at worktree creation time. This is a defense-in-depth layer orthogonal to Fabrik's `--permission-mode dontAsk`: even if the agent somehow bypasses the permission mode (e.g., via `fabrik:unrestricted`), the hook prevents autonomous publishing. Adding a standard pre-push hook during `engine/worktree.go`'s worktree creation step is low-friction and composable with existing posture. It does not conflict with `fabrik:unrestricted` — `unrestricted` widens what the agent can do locally; the hook prevents publishing regardless. Consider a hook that blocks direct pushes and prints a message directing the operator to review and manually push or promote.

4. **Path-scope containment guard.** `consider` — KanBots' dispatcher watches `tool_use` events from the stream parser and flags or blocks edits to paths outside the current worktree. Fabrik could implement a similar watch on Claude Code's `--output-format stream-json` output. This would catch cases where an agent with `fabrik:unrestricted` attempts to modify files outside its worktree. Low-friction to add as a logging/warning layer; requires more work to make it blocking without false positives on symlinks or shared config paths.

### Reject

5. **Per-run branch naming (`kanbots/issue-<n>-<runId>`).** `reject` — Fabrik's per-issue branch (`fabrik/issue-N`) is a deliberate choice (ADR 006). It enables idempotent re-invocation across all stages — Research, Plan, Implement, Review, and Validate all commit to the same branch, producing one reviewable PR per issue. Per-run branch naming would create up to 5 branches per stage and N×stages branches per issue, fragmenting git history and complicating the PR lifecycle (which PR closes the issue?). The per-issue branch is the right design for a multi-stage pipeline.

6. **SQLite state store.** `reject` — ADR 002 is the authoritative counterargument. GitHub-native durable state (labels, reactions, comments) is core to Fabrik's design: inspectable by any team member without installing a client, webhook-able for external automation, and subject to GitHub's existing backup and audit infrastructure. A SQLite store adds a new backup concern, a sync surface (especially for cloud teams), and a dependency that conflicts with Fabrik's zero-infrastructure positioning.

### Watch

7. **MCP surface exposing the Fabrik board.** `watch` — KanBots' `kanbots-mcp-server` exposes issue CRUD, agent dispatch, and decision resolution to Cursor and Claude Desktop. As MCP becomes standard in AI-assisted IDEs, a `fabrik-mcp-server` would let team members query board state and trigger actions from their editor without opening github.com. Not urgent today; revisit when MCP adoption in engineering tools is materially wider (e.g., VS Code official MCP support, JetBrains stable support).

8. **Multi-provider `AgentCliAdapter`.** `watch` — KanBots' `AgentCliAdapter` abstraction cleanly supports Claude Code and Codex via a common interface. Fabrik's `engine/claude.go` hardcodes the `claude` CLI. Adding an adapter layer is not urgent (no current user demand for Codex support), but the abstraction would future-proof Fabrik for provider expansion without a big-bang refactor. Watch for user demand or a compelling non-Claude model before investing in the refactoring cost.

---

## Closing

KanBots and Fabrik converge on the **outer shape** — kanban board as source of truth, per-task git worktrees, AI coding agent runner, local execution — and diverge on **almost every internal contract**: local-first vs. daemon; SQLite vs. GitHub-native state; single-dispatch vs. multi-stage pipeline; `bypassPermissions` vs. `dontAsk`; no MCP vs. no MCP surface (Fabrik trails here). The divergences are deliberate on both sides; neither architecture is wrong.

Where KanBots is materially ahead today: **cost management** (budget caps, live cost meter, explicit `stopReason`), **session resume** (eliminates full-context resend on retry), **MCP surface** (board as IDE integration point), **multi-provider** (Claude Code + Codex via `AgentCliAdapter`), **pre-push safety hook**, and **path-scope containment**.

Where Fabrik is materially ahead today: **multi-stage pipeline** (per-stage model, tool allowlist, skill, comment prompt, inter-stage context handoff), **GitHub-native durable state** (inspectable by any team member, no local DB), **comment-driven revision at stage granularity** (reaction-based idempotency, per-stage `comment_prompt`), **safer default trust posture** (`dontAsk` vs. `bypassPermissions`), and **shared team board** (no per-user app instance).

**Revisit triggers:**
- KanBots reaches v1.0 or 6 months stable after first release (first commit: 2026-04-25; revisit target: ~2026-11).
- KanBots adds GitHub Projects support (currently Issues-only; Projects would narrow the board positioning gap).
- Fabrik evaluates the `adopt` items above (cost budget caps) seriously enough to ADR them.
- MCP becomes standard in VS Code or JetBrains stable (revisit the MCP `watch` item).
- User demand for a non-Claude provider surfaces (revisit multi-provider `watch` item).

Adopting any item above beyond documentation work warrants its own follow-up ADR. This document is analysis, not a Fabrik decision.

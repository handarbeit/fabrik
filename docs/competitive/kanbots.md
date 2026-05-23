# KanBots vs Fabrik — Competitive Analysis

> **Internal document — frank framing, not for publication.**
>
> Analysis date: 2026-05-22
> KanBots version: v1.0.1
> KanBots commit: `1b7b8106e6ef4190bfe64054b27494118951ce1e`
> Source: [github.com/leodavinci1/kanbots](https://github.com/leodavinci1/kanbots) (MIT, 212 stars); [kanbots.dev](https://www.kanbots.dev/)
>
> KanBots architectural claims are sourced from the public website and README at the version above, not from a full source-code audit. KanBots was at v1.0.1 as of the analysis date (one day after v1.0.0 shipped); some features may change quickly.

---

KanBots is a local-first Electron desktop app for kanban-board-driven AI agent orchestration, built by Leonardo Cunha (GitHub: `leodavinci1`). It shipped v1.0.1 on 2026-05-22. It targets the same niche as Fabrik — kanban as the driver for AI agent work — but takes a desktop-app / local-state approach vs. Fabrik's server daemon / GitHub-native approach. KanBots is OSS (MIT) with a $19/seat Pro tier and enterprise custom pricing.

---

## Axis 1: Architecture & Deployment Model

**KanBots:** Local Electron desktop app that runs on macOS, Linux, and Windows. All state is local; the app bundles its own SQLite database. The user launches it on their machine; agents run from that machine's environment.

**Fabrik:** Server daemon CLI written in Go. Typically runs on a server, CI runner, or a shared machine. No GUI. Managed via CLI flags and YAML config files checked into the repo.

**Assessment:** KanBots wins for individual developer UX — install it, open it, go. Fabrik wins for team automation: it runs unattended 24/7 without a laptop staying open, survives reboots, and is trivially daemonized. Fabrik is the better fit when the orchestrator is "infrastructure" rather than a personal tool.

---

## Axis 2: Board / Source of Truth

**KanBots:** Maintains an internal SQLite board at `.kanbots/db.sqlite`. GitHub Issues integration is optional — the user can connect a PAT to sync issues bidirectionally, but the canonical state lives locally. Offline-first by design.

**Fabrik:** GitHub Projects v2 is the board. There is no proprietary local store. The issue itself — its status column, labels, comments, linked PRs — is the state. No sync required; no divergence possible.

**Assessment:** Fabrik's approach means zero migration cost, full board history on github.com, and no "local vs. remote" divergence risk. KanBots' SQLite gives offline-first UX and richer local query capability, but you now have two sources of truth to keep in sync. For teams already on GitHub Projects, Fabrik's model is strictly simpler. KanBots' model is better for solo devs who don't want to rely on GitHub at all.

---

## Axis 3: Workflow / Pipeline Model

**KanBots:** No multi-stage pipeline. One agent dispatch per card. The user can trigger slash commands (`/spec`, `/review`, `/split`) to invoke specific behaviors, and Autopilot personas (see Axis 13) can spawn child tasks. Each card dispatch is a single unbounded agent session.

**Fabrik:** YAML-configured sequential multi-stage pipeline (Research → Plan → Implement → Review → Validate by default). Each stage has its own prompt, skill, model, tool allowlist, `max_turns`, `max_wall_time`, `comment_prompt`, `comment_skill`, lifecycle flags (`post_to_pr`, `create_draft_pr`, `read_only`, `cleanup_worktree`, `wait_for_reviews`, `wait_for_ci`), and effort level. Stages are board columns; board column = pipeline position.

**Assessment:** Fabrik wins decisively for complex codebases and team workflows where auditability, reproducibility, and staged review matter. KanBots wins for quick one-shot tasks where the overhead of a multi-stage pipeline is noise. The pipeline model is Fabrik's primary architectural differentiator — KanBots has no equivalent.

---

## Axis 4: Agent Lifecycle Per Task

**KanBots:** Dispatches one agent session per card. In Autopilot mode, up to 4 agents run in parallel across cards. Context accumulates in the conversation window during a session; there is no structured handoff between logical phases.

**Fabrik:** Advances a single issue through multiple sequential stages. Before each stage, the engine writes structured context files to `.fabrik-context/`: the issue body (`issue.md`), prior-stage outputs (`stage-Research.md`, `stage-Plan.md`, etc.), the linked PR description, and a diff of codebase changes since the last stage ran. Each stage starts with full, structured context from prior stages — not a conversation window that may have summarized or lost earlier details.

**Assessment:** Fabrik's context-file handoff model produces more predictable and auditable outcomes — each stage sees exactly what the engine intends, not whatever survived the conversation window. KanBots' single-session model is simpler and requires less configuration, but loses structured recall across logical phases.

---

## Axis 5: Worktree & Git Management

**KanBots:** Uses per-task worktrees at `.kanbots/worktrees/issue-<n>-<runId>/` on branches named `kanbots/issue-N`. A pre-push hook prevents the agent from autonomously pushing to remote. Conflict handling is not documented.

**Fabrik:** Per-issue worktrees at `.fabrik/worktrees/<owner>-<repo>/issue-N/` on branches named `fabrik/issue-N`. The base repo is always bare-cloned to `.fabrik/repos/<owner>-<repo>.git`. On each stage invocation, the engine fetches and merges `origin/<baseBranch>` unless the worktree is dirty (dirty = preserve partial work). Conflicts are left as conflict markers for Claude to resolve. Preservation semantics are documented in ADR 006 — worktrees with content are never destroyed.

**Assessment:** Both tools use the same basic pattern. Fabrik's preservation semantics are more explicit and battle-tested (ADR 006; the dirty-skip logic is intentional). KanBots' conflict handling is a gap — it's unclear what happens if the agent's branch diverges from main during a long run. The pre-push hook in KanBots is a meaningful safety advantage (see Axis 10).

---

## Axis 6: State Model

**KanBots:** SQLite at `.kanbots/db.sqlite` for the free tier; cloud sync added in the $19/seat Pro tier. State is opaque to anyone without the KanBots app (or direct SQLite access).

**Fabrik:** GitHub labels + comment reactions as sole state (ADR 002). Labels like `stage:Research:complete`, `fabrik:paused`, `fabrik:awaiting-review`, `model:opus`, `fabrik:yolo` encode control flow. Comment reactions (👀/🚀) provide durable idempotency. Everything is inspectable on github.com without tooling; state survives machine loss; no migration scripts required.

**Assessment:** Fabrik wins for teams. GitHub-as-state means anyone on the team can read or override state by adding/removing a label — no special tooling, no KanBots install required. KanBots' SQLite gives richer local query capability and offline-first UX, but the opacity is a real cost for collaborative workflows. Cloud sync (Pro) partially recovers inspectability but introduces a proprietary dependency.

---

## Axis 7: Multi-Provider / Model Support

**KanBots:** Supports Claude Code and OpenAI Codex via an `AgentCliAdapter` abstraction. The adapter pattern suggests more providers could be added. Model selection is per-card.

**Fabrik:** Claude Code only. Within Claude, per-stage model override via `model:` label (e.g., `model:opus`) and effort-level override via `effort:` label (e.g., `effort:max`). Stage YAML can specify `model:` and `effort_level:` defaults.

**Assessment:** KanBots wins on breadth — Codex is a real alternative and the adapter pattern is the right abstraction. Fabrik wins on depth of Claude integration — per-stage model and effort-level control is more granular than KanBots' per-card model selection. Fabrik's single-provider focus is a conscious scope choice, but it is a gap if teams want to use Codex or mix providers across stages.

---

## Axis 8: Human-in-the-Loop / Comment-Driven Revision

**KanBots:** Uses "Decision Points" — the agent presents numbered options and the user selects via a slash command. This is a structured, bounded-choice HITL interaction (based on public website description; exact UX flow is not fully documented in the README).

**Fabrik:** Uses `FABRIK_BLOCKED_ON_INPUT` to pause a stage and wait for a free-form comment. Each stage can define a separate `comment_prompt` and `comment_skill` to process comments during normal operation — not just at explicit pause points. Comment-driven revision can steer any prior stage's output at any time.

**Assessment:** KanBots' Decision Points are more structured — bounded choices reduce ambiguity and are easier to act on quickly. Fabrik's approach is more flexible — comments can be free-form, and any stage can receive iteration at any point. The tradeoff is clarity (KanBots) vs. expressiveness (Fabrik). Both approaches are more capable than most tools in the landscape, which treat each card dispatch as one-shot.

---

## Axis 9: Cost Management & Observability

**KanBots:** Per-run, per-card, and per-project cost rollups; a live cost meter; and configurable budget caps that halt execution when the limit is reached.

**Fabrik:** No built-in cost tracking. There is no mechanism to see what a run cost, set a budget, or be alerted when spending exceeds a threshold.

**Assessment:** KanBots leads clearly. This is a meaningful gap for Fabrik — teams using Fabrik at scale have no visibility into Claude API spend until they check their Anthropic billing dashboard. Budget caps would be especially useful for `fabrik:unrestricted` runs, where tool restrictions are lifted.

---

## Axis 10: Trust & Safety Posture

**KanBots:** Pre-push git hook prevents the agent from autonomously publishing to remote. Budget caps halt runaway spending. Both are automatic guardrails — they apply without user configuration.

**Fabrik:** `--permission-mode dontAsk` is the default (tool allowlist enforced per stage). `--dangerously-skip-permissions` is available via `fabrik:unrestricted` label. No pre-push hook. The tool allowlist is highly configurable and per-stage, but the default posture allows `git push` without a confirmation hook.

**Assessment:** KanBots' pre-push hook is a stronger autonomous-push guardrail out of the box. Fabrik's tool allowlist is more granular — you can restrict which tools each stage can use — but it doesn't prevent a push that's within the allowed tool set. For `fabrik:unrestricted` runs especially, the lack of a push hook is a gap.

---

## Axis 11: Team Collaboration

**KanBots Pro ($19/seat):** Real-time presence, cross-device sync, Slack notifications, SSO.

**Fabrik:** OSS; the GitHub Projects board is the collaboration surface — visible and shared by the entire team without additional cost. No proprietary collaboration layer; no per-seat licensing.

**Assessment:** KanBots wins for teams that want a dedicated collaboration layer with real-time features. Fabrik wins for teams already invested in GitHub Projects — the board is the collaboration surface, no additional tool or seat cost required. The $19/seat Pro tier is a meaningful ongoing cost for larger teams; Fabrik's OSS model has no equivalent.

---

## Axis 12: MCP / External Tool Integration

**KanBots:** Exposes the kanban board as an MCP (Model Context Protocol) server. Editors like Cursor and Claude Desktop can read and write tasks directly through the MCP surface — creating cards, updating status, reading context — without opening the KanBots UI.

**Fabrik:** No MCP surface. Fabrik is not accessible to external tools via MCP; the only interaction surface is GitHub (labels, comments) and the CLI.

**Assessment:** KanBots leads. An MCP server is an increasingly expected integration point for agentic tooling. Fabrik's GitHub-native state model means there's already a conceptual path — `gh` API or GraphQL — but no MCP wrapper. This is an addressable gap.

---

## Axis 13: Autopilot Personas

**KanBots:** Ships five Autopilot personas — PM, Senior Engineer, UX Designer, Growth Lead, Reliability Engineer. In Autopilot mode, a persona can autonomously spawn subtasks from a card, creating a self-evolving roadmap. Up to 4 agents run in parallel in Autopilot.

**Fabrik:** No equivalent. Fabrik's pipeline is scoped to a single issue at a time. Cross-issue subtask spawning requires manual issue creation. Sub-issue decomposition is a planned feature (issue #762 area) but is not the same as autonomous persona-driven roadmap generation.

**Assessment:** KanBots leads significantly on autonomous roadmap generation. This is a genuinely differentiated feature that has no Fabrik analog today. Whether this is a gap worth closing depends on how teams use Fabrik — if the issue backlog is human-curated, autonomous persona spawning is noise; if teams want to point an agent at a goal and have it decompose the work, KanBots is well ahead.

---

## Axis 14: Task Templates & Creation Modes

**KanBots:** Provides five task templates (Bug fix, Feature, Refactor, Review, Spike) and three creation modes: spec-first (write the spec, then dispatch), create-and-dispatch (dispatch immediately), and queue-for-later (add to backlog without dispatching). Templates pre-fill the card with structured fields appropriate to the task type.

**Fabrik:** Issues are plain GitHub issues. There are no templates or structured creation modes built into Fabrik. Teams can use GitHub's built-in issue templates, but there's no Fabrik-specific integration or dispatch-control at creation time.

**Assessment:** KanBots wins on onboarding ergonomics — structured templates reduce the spec quality gap between teams and lower the barrier for new users. Fabrik's flexibility is equivalent (a well-written issue works as well as a template), but requires more user discipline. The queue-for-later creation mode maps naturally to GitHub's draft issue concept, but Fabrik doesn't surface it.

---

## Ideas

**Reject** — Internal SQLite state store. Contradicts ADR 002 (GitHub-as-state-store). SQLite would break inspectability, introduce migration risk, and add a new persistence dependency with no clear benefit for Fabrik's team-automation use case.

**Consider** — MCP server surface. Exposing the Fabrik board state as an MCP server for Cursor/Claude Desktop is architecturally tractable — the GitHub GraphQL API already provides the data, and a lightweight MCP adapter could surface it without changing the state model. Low risk, new interaction mode.

**Consider** — Per-run cost analytics. Track Claude API spend per issue/stage and surface it in the GitHub comment output. No budget-cap functionality needed to start — visibility alone is high value for teams watching API costs. Fabrik already logs per-stage output; adding a cost field is incremental.

**Consider** — Pre-push safety hook. A git hook that prevents `git push` without explicit engine confirmation would be a meaningful hardening option, especially for `fabrik:unrestricted` runs. Low implementation effort; reduces the blast radius of a runaway agent.

**Consider** — Issue templates / creation helpers. Lightweight `fabrik init --template=feature` (or equivalent) would reduce spec quality variance across teams and lower the barrier for first-time users. Could be as simple as populating `.github/ISSUE_TEMPLATE/` during `fabrik init`.

**Watch** — Autopilot personas / subtask spawning. KanBots' self-evolving roadmap feature is genuinely differentiated and has no Fabrik equivalent. Sub-issue decomposition (issue #762 area) is in progress but targets a different use case — human-defined decomposition, not autonomous persona-driven spawning. Monitor KanBots' user adoption here before deciding whether to close the gap.

**Watch** — Multi-provider support via AgentCliAdapter. Fabrik is Claude Code only by design; widening this would require significant engine changes. Watch KanBots' adapter abstraction pattern — if it matures and Codex or another provider proves compelling for teams, the adapter interface is worth studying before designing Fabrik's equivalent.

**Watch** — Decision Points HITL model. KanBots' structured numbered-choice prompt is a more constrained form of `FABRIK_BLOCKED_ON_INPUT`. Bounded choices may reduce ambiguity and response latency in HITL flows. Worth monitoring how teams respond to both approaches — if KanBots' model shows meaningfully better HITL completion rates, it's worth offering as an option in `comment_prompt` configuration.

# Design exploration: multi-user vs bot mode

> **Status: DRAFT — design exploration in progress.** This is NOT an ADR. It does not record a decision. Do not implement against it. Workers should treat its contents as a reasoning exercise, not a directive.
>
> When this exploration concludes, the resulting decision will be promoted to an ADR in `adrs/`. Until then this document captures use cases, constraints, and topology options under consideration.

## Context

Fabrik's current model assumes a single operator runs a single fabrik instance against a project board. The operator's machine holds the GitHub webhook subscription, runs Claude Code workers, owns the worktree state, and pays the Claude bill.

Two pressures are pushing against this assumption:

1. **Multi-user collaboration**: Teams want multiple operators to share one project board, each driving the issues assigned to them. The original "assignee-as-dispatch-filter" plan (#543) was the path: each fabrik instance filters dispatch on `cfg.User ∈ item.Assignees`, so multiple instances can share one board without conflict.

2. **GitHub's webhook-forwarding constraint**: GitHub allows only one user to run `gh webhook forward` against a given repository or organization at a time. The first successful subscriber holds the slot; others get HTTP 422 `Hook already exists` and fall back to GraphQL polling. This is documented behavior, not a fabrik bug — see `docs/USER_GUIDE.md` (post-#644) and the GitHub CLI extension docs.

These two combined produce an architectural fork. Per-operator multi-user mode means N fabrik instances against one repo, but only one of them gets webhook-driven feedback. The others must use GraphQL polling. This breaks the symmetric "many fabriks share a board" intuition: the user holding the webhook slot has seconds-fast feedback; everyone else is poll-cycle latency.

This document explores the tradeoffs and surfaces design questions.

## Use cases

### UC1. Solo developer
One person, one machine, one fabrik instance. Today's default. Webhooks just work; the constraint is invisible.

### UC2. Pair or small team (2–4 ops)
Each operator wants to run fabrik on their own machine, against a shared board, processing only their own assigned issues. Mental model: "fabrik is my dev tool, not the team's service."

### UC3. Larger team (5+) with persistent infrastructure
Team has shared infrastructure (a server, an org-owned VM, a cluster). Fabrik runs as a service. Issues get processed by whoever the team has assigned, but the runtime, billing, and observability are centralized.

### UC4. Enterprise / platform mode
Fabrik is a managed product for a tenant. Multi-tenant isolation, per-tenant billing, per-tenant scopes. Out of current scope but worth keeping in mind so today's architecture doesn't paint us into a corner.

## Constraint matrix

| Constraint | Solo | Small team | Service | Platform |
|---|---|---|---|---|
| Webhook slot (single-user per repo) | OK | **conflict** | OK | per-tenant OK |
| GraphQL rate limit (5000 pts/hr per token) | OK | scales with N tokens | OK | per-tenant OK |
| Claude billing | per-user | **per-user vs shared?** | shared | per-tenant |
| Auth scope on private deps | local env | **local env, varies per user** | central, but who owns it? | tenant-scoped |
| Trust model (who can dispatch?) | self | team | team via project ACL | tenant ACL |
| Failure mode (what if down?) | only mine | only that user's | **everyone blocked** | tenant blocked |
| Observability (TUI, logs) | local | local-each | central dashboard | dashboard |

The two cells in **bold** are the load-bearing tensions:

- **Webhook slot conflict** in small-team mode forces N-1 of N instances onto polling, asymmetrically degraded.
- **Claude billing** in small-team mode is unclear: shared bot account = shared bill (whose budget?); per-user accounts = each user pays for their own work but loses the trivial-shared-state benefit.

## Topology options

### T1. Pure per-user (current default, with assignee filter)

Each operator runs their own fabrik. Filter dispatch on `cfg.User ∈ item.Assignees`. Issues get picked up by whoever owns them.

- **Webhooks**: only one operator's instance gets webhooks. Others poll-only.
- **Cache**: each instance maintains its own cache. No sharing.
- **Billing**: each user pays for their own work via their own Claude account.
- **Trust**: each operator has full project-write access; coordination via assignees.

Pros: simple, no shared infrastructure, user autonomy preserved.

Cons: webhook slot conflict is real. GraphQL polling for non-slot-holding instances must be lightweight enough to be feasible on every operator's machine. Per-instance cache means N× the deep-fetch cost when reconciling — a 170-item board × 3 fabriks polling at 15s each = ~30 GraphQL queries/min just for board reads.

**Required for this to work well**: GraphQL polling must become as cheap as possible. Per-category freshness (#629) becomes load-bearing here, not just a nice-to-have. The slot-holding instance can use webhooks; others must rely on polling that intelligently skips fresh-via-webhook categories... oh wait, they don't have webhook signal. So they must poll every category at TTL.

Note: webhooks become an *optimization for the single-user case only*. Multi-user mode is GraphQL-driven by construction.

### T2. Bot mode

Single fabrik instance owns the board. Acts on behalf of all users via assignee dispatch. Webhooks just work (one slot, one claimant). Single shared cache, single Claude bill, single point of operation.

- **Webhooks**: solved trivially.
- **Cache**: one cache, consistent.
- **Billing**: shared. Either a shared Claude account (org-billed), or per-user delegation (each user's API key drives their issues).
- **Trust**: bot holds an account with project-write. Operators add/edit issues and labels normally; bot dispatches based on assignee.
- **Failure mode**: bot down = nobody's issues progress. Single point of failure.

Pros: clean architecture, no webhook slot conflict, consistent observability.

Cons: requires shared infrastructure. Billing model is non-obvious. Single point of failure. Trust questions about the bot's GitHub token (typically a service-account PAT or GitHub App).

**Open sub-question**: in bot mode, does each user supply their *own* Claude credentials (bot delegates execution to a user-provisioned worker), or does the bot have a shared Claude account?

- **Shared Claude account**: simplest. All issues bill to one place. Org-level cost control. But: tracking who-spent-what-on-what for chargeback or budgets is harder.
- **Delegated execution**: bot decides what to dispatch; sends the work to a per-user "claude executor" that pays from that user's account. Preserves cost ownership. Requires a new component (the executor) and trust mechanism.

### T3. Hybrid: bot for webhooks + dispatch, distributed execution

Bot owns webhook subscription and dispatch decisions. Once the bot decides "issue X needs stage Y for user U," it delegates execution to user U's local worker (or to a shared pool). Each operator runs a thin worker process that registers with the bot and receives jobs.

Pros: solves webhook conflict, preserves per-user execution environment (local creds, private deps, worktree isolation). Per-user billing trivially preserved.

Cons: most complex. Two-tier architecture (bot + workers). Network protocol between them. Coordination on worker availability. Bot still single point of failure for dispatch.

### T4. Peer-to-peer with leader election

Each operator runs a full fabrik instance. They elect a leader (e.g. via a GitHub Issue with a coordination label, or via a project field). Leader holds the webhook slot. On leader failure, another operator takes over.

Pros: no central infrastructure, resilient to single failures, all instances are interchangeable.

Cons: leader-election complexity, race conditions during failover, harder to reason about. Probably not worth the complexity unless team is large enough to actually need this.

## Implications for current work

If we lean toward **T1 (per-user with assignee filter)**, then:

- **Webhooks must explicitly become a single-user optimization.** Document this in `state-machine.md`. The non-slot-holding instances are *expected* to operate in poll-only mode; this is not a degraded state, it's the design.
- **GraphQL polling must be as lightweight as possible.** Per-category freshness (#629) is the critical enabling work. Without it, multi-user GraphQL polling burns rate limits and is operationally painful.
- **The webhook-related bugs we just chased (#628, #631, #637, #638, #641, #642, #643)** are all solving for the slot-holder's experience. They don't help the multi-user case. The slot-holder gets fast feedback; everyone else gets reliable-but-slower GraphQL polling.

If we lean toward **T2 (bot mode)**, then:

- The webhook fixes pay off long-term.
- We need to think about **bot identity**, **billing model**, **per-user delegation of execution if any**, and **failure recovery**.
- A new mode flag or deployment pattern: `fabrik serve` vs `fabrik run`?
- Multi-instance assignee filtering becomes irrelevant in bot mode but remains useful for the solo case.

If we lean toward **T3 (hybrid)**, then:

- Both above lists apply.
- Plus a new component / protocol design (executor registration, job dispatch, work confirmation, heartbeat).

## Open design questions

1. **Is per-user multi-user mode actually the goal, or did we drift there because it was the easy assumption?** If teams almost always want shared service mode, T2 is probably the right answer and T1 stays useful only for solo devs.

2. **Whose Claude credits pay for whose work in shared modes?** This is the cleanest fork for ownership / accountability discussions.

3. **What's the upper bound on number of concurrent fabrik instances per repo before GraphQL rate limits become uncomfortable?** Worth back-of-envelope arithmetic: with per-category freshness at reasonable TTLs, what's the per-instance steady-state GraphQL load? × N instances?

4. **Does fabrik want to be a developer tool (T1) or a team service (T2)?** This is partly a product question. Today's docs and onboarding lean tool. The architecture is mostly tool-shaped.

5. **Bot mode authorization model**: GitHub App vs PAT vs service-account user? Each has consequences for scope, audit trail, and how the bot's actions are attributed.

6. **Failure attribution**: in bot mode, when fabrik makes a mistake on a user's behalf, whose mistake is it visibly? This isn't just legal; it affects how users frame trust in the system.

7. **Local-environment dependency** (private deps requiring secrets, ssh keys, project-specific setup): how does that move from "operator's machine" to "bot environment" in T2 / T3?

## What this document is NOT

- A decision. The above tradeoffs aren't yet weighted; they're laid out for discussion.
- A spec. No implementation should be started against this.
- An ADR. When a decision crystallizes, an ADR will be created in `adrs/` and this draft will be retired or rewritten as background material.

## Suggested next steps

1. Decide which use cases (UC1–UC4) are in/out of scope for fabrik's near-term roadmap.
2. Pick a primary topology (T1, T2, T3, or some sequence — e.g. "T1 first, T2 by Q3").
3. For the chosen topology, identify the load-bearing prerequisites (e.g. per-category freshness for T1; bot identity model for T2).
4. Promote the chosen direction to an ADR. Move design notes here that are still relevant; archive the rest.

Until step 4, this document is the working draft. Comments, alternative framings, and counter-proposals welcome — open a discussion or annotate this file directly.

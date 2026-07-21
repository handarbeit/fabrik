---
layout: post
title: "The Core Idea"
description: "Fabrik is a workflow harness wrapped around Claude Code. The real question isn't Fabrik vs. subagents — it's whether you lean on a session-oriented harness alone, or wrap it in an external, SDLC-structured one."
---

# The context window is not a database

Fabrik is a workflow harness wrapped around Claude Code. Everything else about it follows from one architectural bet: durable state doesn't belong in a language model's context window.

## It started with a fair question

When Fabrik went public, the sharpest thing anyone asked me was this: *if it's Claude-specific, what's the benefit over Claude Code subagents? They already use worktrees, support varying degrees of autonomy, store memories at user and project levels, use models suited for each step, and collaborate with each other. Claude Code can generate a majority of this with a few prompts.*

He was right — on every point. And answering him honestly turned out to be the best way to explain what Fabrik actually is.

## Claude Code is a harness, not just a model

The first thing to get straight: Claude Code isn't just a model behind a prompt. It's a **harness** — an orchestration layer that manages subagents, tools, git worktrees, and memory within a running session. It's a strong harness, and it's superb at the *inner loop*: the coding task in front of it right now.

So the interesting question was never "Fabrik vs. subagents." Both are orchestration. The real question is about layering:

> Do you rely on the session-oriented Claude Code harness alone, or wrap it in an external, SDLC-structured one?

## Why you might want a second harness

A session harness keeps its state where the session lives — in and around the context window. That's exactly right for the inner loop, and exactly wrong for everything outside it, on two axes.

**Functionally**, a context window is working memory. It's ephemeral — it dies with the session. It's lossy as it fills — the model's attention degrades, the thing people call context rot. It's single-threaded — one session is one train of thought. And it's opaque — you can't cleanly inspect or edit it, and neither can other tools. Durable work needs the opposite: state that persists, stays exact, runs many things at once, and is open to humans and tooling.

**Cost-wise**, every token you keep in context you pay for again on every turn. Hold your whole project history in the window and you re-read it — and re-pay for it — continuously, and it only grows. State you move *out* of the window, you pay for once.

Put plainly: the context window is not a database. Using it as one is the mistake Fabrik is built to avoid.

## Separate the state from the work

This isn't a new idea — it's one of the oldest in systems design. Separate compute from state. Stateless workers plus a backing store. Twelve-factor apps. Serverless functions and a database. RAM and disk. The machinery works precisely because the thing doing the work doesn't also have to *be* the system of record.

The default in agentic AI is the opposite: pour everything into one ever-growing context and hope attention holds. Fabrik takes the boring, proven path instead.

## How Fabrik embodies it

Fabrik's system of record is your **GitHub Project board** — not a database it invented, but the board, issues, labels, and pull requests you already have. That's the durable state machine. Each unit of work is an issue; its column is its stage; its labels are its state.

Around that, Fabrik runs the outer loop — Specify, Research, Plan, Implement, Review, Validate — and at each stage it does something deliberate: it starts a **fresh Claude Code process** and hands it only the working set that stage needs, assembled from the board and the repo. The context window is scratch space, filled for one stage and discarded. Context rot never gets a chance to accumulate across the pipeline, because no single context spans it.

Because state lives on the board rather than in a session, three things come for free:

- **Durability** — work survives restarts, crashes, and days of wall-clock time. Stop Fabrik, restart it tomorrow, and the board tells it exactly where every issue stands.
- **Concurrency** — many issues are genuinely in flight at once, each in its own worktree, because there's no shared session they have to take turns inside.
- **Steering** — you stay in the loop asynchronously, through tools you already use. Comment on an issue to redirect it, move a card, review a PR, add a label. You don't have to be at the terminal.

## "But Claude already writes plans to disk"

True — and it's a fair challenge. Claude Code writes plans, notes, and memory to markdown, and that's real externalization. But those files are the session's own scratch: the session writes them, reads them back, and nothing *outside* the session acts on them. End the session and they're orphaned notes. A function spilling a variable to a temp file doesn't make that file the application's database.

Fabrik's board is a system of record, not scratch. It's structured — issue, column, labels — and an external engine, *not the model*, reads it to decide what runs next: which stage, whether to wait on CI, whether a human paused it. That's control state, living in a deterministic system outside any session, shared by humans, CI, and other tools, surviving every restart. The distinction was never disk vs. context — it's whose state it is, and what reads it.

## Nested, not competing

None of this replaces Claude Code's harness. Fabrik *uses* it — inside every stage, that inner-loop harness, with its subagents and worktrees and tools, is doing the actual coding. Fabrik doesn't write the code; Claude does. Fabrik decides what runs when, hands it the right context, and keeps the state on the board.

Two harnesses, nested: Claude Code runs the inner loop, Fabrik runs the outer SDLC loop. They complement; they don't compete.

## Is it for you?

Maybe not. If your work fits inside a session — a task, a feature, an afternoon — the session harness alone is lighter and probably enough. Fabrik earns its keep when work needs to *persist and be managed*: many issues, over days, with a team, where you want the state to be durable, inspectable, and steerable long after any one session has ended.

That's the whole idea. Everything else in Fabrik — the stages, the labels, the gates, the merge train — is plumbing in service of it: keep the state on the board, give the model only what it needs, and let each be excellent at the job it's actually good at.

---

Want the practical version next? See [How It Works]({{ '/#how-it-works' | relative_url }}), the [FAQ]({{ '/faq' | relative_url }}), or [start a discussion](https://github.com/handarbeit/fabrik/discussions).

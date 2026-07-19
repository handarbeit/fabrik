---
layout: post
title: "FAQ"
description: "Short, honest answers to the questions people ask most about Fabrik."
---

# Frequently asked questions

Short, honest answers to what people ask most about Fabrik. It's still rough in places — if something's missing, [start a discussion](https://github.com/handarbeit/fabrik/discussions).

## The basics

### What is Fabrik?

A small Go CLI that drives Claude Code through a software-development pipeline defined on a GitHub Project board. Each issue is a unit of work; the board columns are stages — Specify → Research → Plan → Implement → Review → Validate. Fabrik watches the board and runs Claude Code per stage, posting results back as comments, labels, and pull requests.

### Does Fabrik write the code?

No — Claude Code writes the code. Fabrik is the orchestrator: it decides which stage runs, sets up an isolated git worktree, feeds Claude the right context and prompt, and moves the issue forward. The resulting code is Claude's work, shaped by your stage config.

### What do I need to run it?

Go 1.26+, the Claude Code CLI, and a GitHub token with `repo` and `project` scopes. Everything runs locally on your own machine — you bring your own token and Claude Code.

### Is it free?

Fabrik itself is free and open source (Apache-2.0). You pay Anthropic for Claude Code usage; cost scales with how many stages you let run.

## Using it

### Human "in the loop" or "on the loop"?

Both — you choose per issue. Pause after each stage to approve before it advances, or let an issue run straight through and auto-merge. Same engine either way; the autonomy level is set with labels.

### What's the "Specify" stage for?

Turning a rough issue into a clear spec. A vague issue produces a vague PR, so Specify reads the raw request, asks clarifying questions when it's ambiguous, and rewrites it into something buildable — before any code is written.

### Do I need the binary, or can I just use the plugin?

There's a companion Claude Code plugin (the "Fabrik PM plugin") that needs no binary install. It gives your interactive Claude Code session awareness of your Fabrik board, so you can ask "what's on the board?" or "why is #42 stuck?" without leaving Claude Code. To actually run the pipeline, you need the CLI.

### Can I change the stages and prompts?

Yes — that's the point. Every stage is a YAML file plus a markdown "skill," so you can rewrite any stage's prompt, model, tools, and gates, or add entirely new stages to the flow.

### Does the resulting code come with tests and docs?

Tests, yes — the default Implement stage treats them as non-optional, written alongside the code. Docs get updated where it makes sense. Anything more specific — ADRs, changelog discipline, a docs-sync step — you bake in by editing the skills, which is how Fabrik's own repo enforces its conventions.

## Scope, cost, and limits

### What does it cost to run?

You pay Claude per stage, so cost scales with pipeline depth and autonomy. Gating stages (approving as you go) keeps it modest; running full autonomy across many issues is where it adds up. There's no Fabrik-side fee.

### Is it production-ready?

It's had real use — it drove most of its own development, and a few people run it on their projects — but it's not battle-tested. Expect sharp edges, and keep a human in the loop until you trust it on your codebase.

### Isn't this over-engineered for most projects?

Probably, if you use all of it. The ADRs, merge train, and gates exist because running many issues at once surfaced real problems — but you can use Fabrik simply (one issue, gated stages) and ignore the rest.

### Doesn't agentic AI just blow up scope and leave a mess?

It can, unconstrained. Fabrik's bet is that structure limits sprawl — spec-first, bounded issues, and Review/Validate gates exist to stop the model running away. Cleanup and deletion are a fair gap; since stages are just YAML, a "prune" stage is straightforward to add.

## How it compares

### How is this different from Aider / OpenHands / Devin / GitHub's agent?

It's a self-hosted orchestrator, not a single-task agent or a hosted product. Your GitHub Project board is the state machine, and each issue runs through a configurable multi-stage pipeline using tools you already have — no new dashboard, bring your own key. Whether that's better depends on your workflow.

### Claude-only?

Yes, today. The config abstracts model, prompt, and tools per stage, so other backends are plausible — but they're not built yet, and I won't pretend otherwise.

### How does it relate to GSD Core / Spec Kit / other spec-driven tools?

They're cousins — all spec-driven, gated loops. The difference is emphasis: Fabrik centers the GitHub board as durable, multi-issue state; others focus on context engineering within a single task. Convergent takes on the same idea.

---

Still have a question? [Start a discussion](https://github.com/handarbeit/fabrik/discussions) or [open an issue](https://github.com/handarbeit/fabrik/issues/new/choose).

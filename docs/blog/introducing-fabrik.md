---
layout: default
title: "A lean take on the software factory"
description: "Fabrik drives Claude Code through an SDLC pipeline defined on a GitHub Project board. Now open source under Apache-2.0."
date: 2026-07-18
published: false
---

# A lean take on the "software factory"

Fabrik is a small Go CLI that drives Claude Code through a software-development pipeline defined on a GitHub Project board. It's now open source under Apache-2.0. This is a modest write-up of what it is and where it stands — not a launch pitch.

## The itch

One-shot AI prompting is fine for small tasks and awkward for everything around them — turning an idea into a spec, breaking it up, reviewing, rebasing, landing. That connective tissue is most of the work, and it doesn't fit in a single prompt. I wanted the model to work in stages, with a human able to step in anywhere, using the tools I already have rather than a new dashboard.

## What it does

Work goes on a GitHub Project board as issues. The columns are stages — Specify → Research → Plan → Implement → Review → Validate. Fabrik polls the board and, for each issue, creates an isolated git worktree and runs Claude Code with a stage-specific prompt, model, and tool allowlist. Results come back as comments, labels, and pull requests. Stages are just YAML.

## In the loop, or on it

You pick the autonomy level per issue — pause after each stage to approve, or let it run through and auto-merge. Same engine either way.

## Where it stands

I've used it on four of my own projects and to drive most of Fabrik's own development; a few friends have run it on theirs, in some cases heavily. One of my projects is public: [liminis-context-graph](https://github.com/verveguy/liminis-context-graph), a Rust knowledge-graph service built through the same pipeline (84 issues, 104 merged PRs). And — more convincingly — a friend independently built [switchcraft](https://github.com/totalslacker/switchcraft), a Swift port, entirely through Fabrik (72 issues, 69 merged PRs), which I take more seriously than my own use because it shows the tool works for someone who isn't me. Both are public if you want to see Fabrik applied to something other than its own tooling.

Fabrik's own history (2,316 commits, 492 issues, 448 merged PRs, ~38k lines of Go, 66 ADRs) mostly went through the pipeline too, with me writing specs and reviewing PRs. I offer these as context, not benchmarks — one person's projects, mistakes and all.

## What was hard

Not prompting — coordination. Isolated worktrees, a board that behaves as real state, and a rebase-and-retest cascade when landing several PRs at once (which took a merge-queue integration and a fallback "merge train" to tame). And the ever-present truth that a vague spec yields a vague result — hence the Specify stage.

## What it isn't

Not magic, not free (you pay Claude per stage), and Claude-Code-specific for now. It won't fix a bad issue.

## Thanks &amp; inspiration

Fabrik didn't come from nowhere:

- The core "project board as a state machine" idea I first saw in an internal system built by a colleague. Fabrik is a small, public take on that idea — I'm not claiming the concept.
- After trying [Gastown](https://github.com/gastownhall/gastown) — a much larger, multi-agent take on the "software factory" — I wanted something far smaller and simpler. Fabrik is that leaner answer.
- [@totalslacker](https://github.com/totalslacker) had been down the same path and helped shape Fabrik in response, and built [switchcraft](https://github.com/totalslacker/switchcraft) with it; a friend contributed code and put it to real use. Thanks to both.

## Try it

A single Go binary, standard-library-only except the YAML parser, self-hosted, bring your own GitHub token and Claude Code.

→ [github.com/handarbeit/fabrik](https://github.com/handarbeit/fabrik) · [fabrik.handarbeit.io](https://fabrik.handarbeit.io)

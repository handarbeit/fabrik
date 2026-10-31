---
layout: post
title: "Companion Tools"
description: "Pruefer, a self-hosted Claude Code PR review bot, and Werkbank, tooling for self-hosted GitHub Actions runners. Two optional tools from handarbeit that run well alongside Fabrik."
---

# Companion tools

Fabrik works on its own. Two other tools from handarbeit are commonly run alongside it, and each solves a problem Fabrik-driven pipelines tend to hit. Neither is required.

## Pruefer: a PR review bot

[Pruefer](https://github.com/handarbeit/fabrik/tree/main/cmd/pruefer) is a self-hosted Claude Code PR review bot. It runs as a GitHub App daemon, reviews open pull requests by invoking the `claude` CLI on a Claude Code subscription (no per-token API billing), and submits a formal pull-request review as its own bot identity. It never approves a PR.

It exists to satisfy Fabrik's `wait_for_reviews: true` gate without depending on a hosted reviewer's quota. Its inline comments feed Fabrik's review-reinvoke path, so findings can be addressed automatically.

To tell the gate that Pruefer is coming, declare it on the stage that waits for reviews, using your own Pruefer App's handle (bare, with no `@` and no `[bot]` suffix):

```yaml
wait_for_reviews: true
expected_reviewers: [<your Pruefer app's handle>]
```

- [Pruefer overview]({{ '/pruefer' | relative_url }}): what it does, install, minimal configuration, the main settings and how it works with the review gate.
- [Declaring Expected Reviewers]({{ '/USER_GUIDE' | relative_url }}#declaring-expected-reviewers) in the User Guide.
- [Source and full README](https://github.com/handarbeit/fabrik/tree/main/cmd/pruefer) in this repository.

## Werkbank: self-hosted Actions runners

[Werkbank](https://github.com/handarbeit/werkbank) is handarbeit's tooling for self-hosted GitHub Actions runners on machines you already have. It provides ephemeral Linux containers on a Mac (OrbStack or Docker Desktop) or in WSL2, native macOS and Windows runners, an overflow controller that sends jobs to your fastest machine first and spills over to slower ones when it is busy, and a host watchdog.

**When you might want it with Fabrik.** Fabrik-driven CI (the CI gate, CI-fix rounds and merge-train trials) runs your workflows again and again, and on private repos that can use many runner minutes. GitHub-hosted runners bill by the minute for private repos, so running that load on machines you already own can be cheaper.

**Keep public repos on GitHub-hosted runners.** They're free there, and a self-hosted runner lets a fork's pull request run code on your machine.

Werkbank is installed through your own private deployment repo that pulls it in as a git submodule. Its README says it was extracted from a working setup and may have rough edges outside it, so read its docs before relying on it.

- [Werkbank on GitHub](https://github.com/handarbeit/werkbank)
- [CI Gate and CI-Fix Workflow]({{ '/USER_GUIDE' | relative_url }}#ci-gate-and-ci-fix-workflow) in the User Guide.

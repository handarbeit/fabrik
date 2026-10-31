---
layout: docs
title: "Pruefer"
description: "Pruefer is a self-hosted Claude Code PR review bot that satisfies Fabrik's wait_for_reviews gate. What it is, how to install it, the main settings, and how it works with Fabrik."
---

# Pruefer

Pruefer is a self-hosted pull-request review daemon. It watches the repositories its GitHub App is installed on, reviews open pull requests by running the `claude` CLI (backed by a Claude Code subscription, not metered API billing), and submits a formal `pull_request_review` pinned to the head commit it reviewed. It lives in this repository under [`cmd/pruefer/`](https://github.com/handarbeit/fabrik/tree/main/cmd/pruefer).

This page is a short overview. The [Pruefer README](https://github.com/handarbeit/fabrik/blob/main/cmd/pruefer/README.md) is the full reference and the authority on every setting and default. If the two ever disagree, the README wins.

## What it does

- **Reviews as its own bot identity.** Pruefer authenticates as a GitHub App you own, so its reviews come from `<app-slug>[bot]` and are never confused with the PR author.
- **Posts a formal review.** The review is a non-blocking `COMMENT` by default. Findings that map to a changed line are posted as inline comments, which is what lets Fabrik's review-reinvoke path pick them up and act on them. If you set `request_changes_threshold`, a finding at or above that severity makes the review a blocking `REQUEST_CHANGES` instead.
- **Never approves.** Pruefer does not approve a PR under any configuration. Approval stays a human decision.
- **Polls, with no webhook setup.** Every `poll_interval_seconds` it looks at open, non-draft PRs. It skips a head it has already reviewed, PRs it authored, and PRs labelled `pruefer:ignore`. Comment `/pruefer review` on a PR to force a fresh review of the current head.
- **Fails quietly.** If a clone, review or submission fails, Pruefer posts nothing and retries on the next poll.

It exists so a repo can satisfy Fabrik's `wait_for_reviews: true` gate (or simply have a review bot) without depending on a hosted third-party reviewer's quota.

## Install

You need Go, a working `claude` CLI on the host with a valid Claude Code subscription (a credential separate from the GitHub App), and a GitHub account that will own the App.

```bash
# in a checkout of this repository
go build -o bin/pruefer ./cmd/pruefer
./bin/pruefer
```

On the first run, with no local App credentials, Pruefer walks you through GitHub's App Manifest flow: it opens a browser page, you confirm the App's name, and GitHub hands the credentials back to a temporary local listener. Pruefer saves the private key to `.pruefer/app-private-key.pem` and the rest to `.pruefer/app-state.json` (neither is committed or logged). It then prints a link to install the App. Install it on the repos or the account you want reviewed. No restart is needed, because Pruefer picks up new installations on its own.

- The App it creates is **private** and owned by whichever account ran the flow, so it can only be installed on that account. To review several accounts, run one App and one Pruefer daemon per account. The README's "per-account model" section explains why.
- Running Pruefer on a remote host? The setup listener only binds to `127.0.0.1` on that host, so forward the port over SSH. The README has the exact steps.
- Prefer to register the App yourself? Manual setup is supported. See the README's Setup section.
- Releases are published with Fabrik. `pruefer --version` prints the build, and `auto_upgrade` (off by default, recommended for long-running deployments) keeps a daemon current. See "Version & self-upgrade" in the README.

## Minimal configuration

Pruefer reads `.pruefer/config.yaml` from its working directory. This is enough to start:

```yaml
# .pruefer/config.yaml
poll_interval_seconds: 120
model: sonnet
```

Which repos get reviewed is decided by where you install the App. There is no repo list to maintain.

## Main operator settings

Precedence is flag, then environment variable, then YAML, then default. The defaults below are from the README's [Configuration reference](https://github.com/handarbeit/fabrik/blob/main/cmd/pruefer/README.md#configuration-reference), which lists every setting with its flag and environment variable.

| YAML key | Default | What it does |
|---|---|---|
| `poll_interval_seconds` | `120` | How often Pruefer polls for PRs. |
| `model` | `sonnet` | Claude model used for reviews. |
| `effort` | `medium` | `low`, `medium`, `high` or `max`. |
| `concurrency_cap` | `3` | Maximum simultaneous `claude` reviews. |
| `max_diff_bytes` | `500000` | Diff size above which Pruefer trims the largest files and reviews the rest. |
| `watched_repos` | none | Optional filter that narrows the installed repos to a subset. |
| `served_accounts` | none | Optional allowlist of account logins. An installation outside it is removed. |
| `excluded_authors`, `excluded_labels`, `excluded_paths` | none | Skip PRs by author or label, and exclude files by glob from the review. |
| `request_changes_threshold` | off | `low`, `medium`, `high` or `critical`. When a finding meets it, the review is `REQUEST_CHANGES`. |
| `cadence` | `every-push` | `every-push`, `once` or `on-request`. |
| `catch_up_marker_authors` | none | Needed to skip reviews of pure merge-train catch-ups. See below. |
| `auto_upgrade` | `false` | Self-upgrade at the poll boundary. |

Review behaviour can be extended with `review_guidance`, `review_skill`, `plugins` and `review_tools`, and a reviewed repo can adjust some settings in its own `.pruefer/config.yaml`. The README covers these in depth.

## Working with Fabrik's review gate

Declare Pruefer on the stage that waits for reviews, using your own App's handle (the App slug, written bare, with no `@` and no `[bot]` suffix):

```yaml
wait_for_reviews: true
expected_reviewers: [<your Pruefer app's handle>]
```

Fabrik then knows a review is coming even though Pruefer is never formally requested as a reviewer. The User Guide's [Declaring Expected Reviewers]({{ '/USER_GUIDE' | relative_url }}#declaring-expected-reviewers) section explains the semantics, and its Pruefer review notes list the behaviours that matter at the gate. In short:

- A `partial` review still clears the gate. Every review ends with a `PRUEFER_COVERAGE: complete` or `PRUEFER_COVERAGE: partial` line, but Fabrik's gate only sees that a review was submitted.
- Every review also ends with a hidden findings marker. Fabrik reads it, so a clean review from a declared reviewer is not treated as actionable feedback.
- Pruefer's inline comments become review feedback that Fabrik can act on automatically, bounded by the stage's review-cycle limit.
- For the merge train, set `catch_up_marker_authors` so Pruefer can skip a head that is only a pure catch-up merge of one it already reviewed.
- The review runs with a scrubbed environment. If `claude` authenticates through an environment variable, name it in `PRUEFER_REVIEW_ENV_PASSTHROUGH`.

## Full reference

For everything else, including the config reference, review plugins, coded review tools, severity-gated `REQUEST_CHANGES`, review cadence and logging, read the [Pruefer README](https://github.com/handarbeit/fabrik/blob/main/cmd/pruefer/README.md).

Pruefer is one of the [companion tools]({{ '/companion-tools' | relative_url }}) used alongside Fabrik.

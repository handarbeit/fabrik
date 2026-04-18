# Architecture Decision Records

This directory contains the Architecture Decision Records (ADRs) for Fabrik.

ADRs document significant decisions made during development, their context,
and the reasoning behind them. They serve as a historical record for
contributors and future maintainers.

## Index

| ADR | Title | Status |
|-----|-------|--------|
| [001](001-go-implementation.md) | Go as implementation language | Accepted |
| [002](002-github-as-state-store.md) | GitHub Issues + Projects as sole state store | Accepted |
| [003](003-polling-over-webhooks.md) | Polling over webhooks | Accepted |
| [004](004-stage-based-pipeline.md) | Stage-based pipeline with YAML configs | Accepted |
| [005](005-claude-cli-invocation.md) | Shell out to Claude Code CLI | Accepted |
| [006](006-git-worktrees.md) | Git worktrees for issue isolation | Accepted |
| [007](007-label-based-locking.md) | Label-based locking for multi-user safety | Accepted |
| [008](008-human-in-the-loop.md) | Human-in-the-loop by default | Accepted |
| [009](009-comment-processing.md) | Comment processing with reaction flow | Accepted |
| [010](010-file-granularity.md) | File Granularity and Module Structure | Accepted |
| [011](011-context-files.md) | Context Files as Claude Context Delivery Mechanism | Accepted |
| [012](012-stage-comment-living-document.md) | Stage Comment as Living Document | Accepted |
| [013](013-project-config-yaml.md) | `.fabrik/config.yaml` as Project-Level Config Source | Accepted |
| [028](028-rate-limit-backoff-hysteresis.md) | Two-Threshold Hysteresis for Rate-Limit Backoff | Accepted |

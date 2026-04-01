# ADR 001: Go as Implementation Language

## Status

Accepted

## Context

Fabrik is a CLI tool that polls GitHub, manages git worktrees, and shells out
to Claude Code. We needed a language that produces a single static binary,
handles concurrent operations well, and has strong ecosystem support for
CLI tooling and HTTP clients.

## Decision

Implement Fabrik in Go.

## Rationale

- **Single binary**: No runtime dependencies. Users `go build` and get one
  executable — no node_modules, no virtualenvs, no Docker required.
- **Concurrency**: Goroutines and channels are a natural fit for poll loops
  and managing multiple subprocess invocations.
- **Ecosystem**: Excellent HTTP client, JSON handling, and `os/exec` for
  subprocess management. GitHub's GraphQL and REST APIs are straightforward
  to call with Go's standard library.
- **Startup speed**: Fast cold start matters for a CLI that gets rebuilt
  frequently during development.

## Alternatives Considered

- **Python**: Faster to prototype but slower runtime, dependency management
  adds friction, and distributing a CLI requires packaging.
- **TypeScript/Node**: Good GitHub library support but heavier runtime and
  subprocess management is less ergonomic.
- **Rust**: Excellent performance but higher development cost for a tool where
  the bottleneck is API calls and Claude Code execution, not the driver itself.

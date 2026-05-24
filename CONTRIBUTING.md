# Contributing to Fabrik

Thanks for your interest in Fabrik. This guide covers the basics — for a deeper map of the codebase and conventions, see [`CLAUDE.md`](CLAUDE.md).

## Development setup

```bash
git clone https://github.com/handarbeit/fabrik.git
cd fabrik
go build -o fabrik .
go test ./...
```

You'll need Go 1.22+ and `git`. A few tests shell out to the real `git` binary and skip when it's unavailable.

## Running checks before you push

```bash
go test -race ./...   # race detector catches concurrency bugs
go vet ./...
gofmt -s -d .         # fix with: gofmt -s -w .
```

CI runs `go test -race`, `go vet`, and the docs-drift check on every PR.

## Documentation bundle

If your change touches any of `docs/USER_GUIDE.md`, `docs/state-machine.md`, `docs/stage-lifecycle.md`, or `docs/positioning.md`, regenerate the LLM-facing bundle in the same commit:

```bash
bash scripts/generate-llms-full.sh
git add docs/llms-full.txt
```

The `docs-drift` workflow will fail the PR otherwise.

## Pull requests

- Branch off `main`. Small, focused PRs are easier to review.
- Every PR linked to an issue should include `Closes #N` in the body so the engine can discover the link.
- Match the existing commit-message style (`feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `chore:`).
- Code style: standard Go conventions (`gofmt`), `MixedCaps` naming, stdlib-first.
- Engine-behavior changes must update the corresponding as-built doc (`docs/state-machine.md` or `docs/stage-lifecycle.md`) in the same PR — these are authoritative specs, not afterthought notes.

## Reporting bugs

Open an issue on [the issue tracker](https://github.com/handarbeit/fabrik/issues). If the bug involves a stage run, attach the relevant section of `.fabrik/logs/fabrik.log` (redact any tokens first).

## Security

Please don't file public issues for security vulnerabilities — see [`SECURITY.md`](SECURITY.md) for the disclosure process.

## License

By contributing, you agree that your contributions will be licensed under the Apache License 2.0, the same license that covers the project.

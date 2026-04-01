---
name: build-check
description: Build, test, vet, and format the Fabrik codebase
---

Run the full Go build and quality check pipeline:

1. `go fmt ./...` — format all files
2. `go vet ./...` — static analysis
3. `go build ./...` — compile
4. `go test -race ./...` — run all tests with race detector

Report any failures clearly. If formatting changed files, list them.

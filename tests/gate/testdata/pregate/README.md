# Pre-gate crash-signature fixtures (#1973)

**These logs are SYNTHETIC.** The repository holds no recorded real log of the
TSan fork/exec crash (ADR-1624 / ADR-1677 describe it only in prose:
`CHECK failed: tsan_rtl.cpp:94`, the child exiting 66, and a child `git` dying with
`signal: segmentation fault`). They were written from those descriptions and from
the shapes `go test` and `os/exec` actually print, so the signature is only as
trustworthy as that reconstruction.

The signature therefore fails closed (anything unrecognised is a hard stop, as it
was before #1973), and a retry can never turn a genuine failure green — the step
must pass in full the second time.

If a real log from a crashed gate run is available, add it here as an extra
fixture (`real-*.log`) and add it to the table in `pregate_signature_test.go`.

| file | expected |
|---|---|
| `tsan-abort.log` | match |
| `git-segfault.log` | match |
| `ordinary-failure.log` | no match |
| `engine-panic.log` | no match |
| `segfault-and-panic.log` | no match (veto) |
| `bare-sigsegv.log` | no match (Go's own SIGSEGV is an engine crash) |

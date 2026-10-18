# ADR 2303: Pruefer keeps the review transcript when coverage is partial

## Status

Accepted. **Amended by ADR 2363:** the default is now `all`, retention is by age (`transcript_max_age`, default 168h) with a `transcript_max_bytes` backstop instead of a count of 50, and a review's hidden tool results are kept beside its transcript. Builds on ADR 2190 (diff coverage contract); `deriveOpened`'s credit rules are unchanged.

## Context

ADR 2190's coverage check reports a review as `partial` from the reviewer's own report intersected with what the stream-json transcript proves was read. The transcript was the only evidence behind that verdict and was dropped when `RealClaudeInvoker.Review` returned. On PR #2191 the reviewer said it read all 10 entries while the check reported `10 of 10 diff entries not opened`; with no transcript, "the check undercounts" and "the reviewer's text is false" could not be told apart.

## Decision

- `ReviewResult.Transcript` carries the invocation's raw stdout out of the invoker. `ClaudeInvoker` is unchanged; an invoker with no transcript returns none, which means nothing is kept.
- `ReviewPR` (the only place that knows coverage, owner, repo, PR and head SHA) calls `maybeKeepTranscript` right after `computeCoverage` and before submission. The review's temp directories are removed by defers that run after that point.
- Files go to `.pruefer/transcripts/` (cwd-relative like `log_file`; `transcript_dir` relocates it, YAML only) as `<UTC yyyymmddThhmmss.nnnnnnnnnZ>_<owner>_<repo>_pr<N>_<sha>.jsonl`. The timestamp comes first so lexical order is chronological and a forced re-review of one head cannot collide; name parts are sanitised to a single safe component. Contents are the unmodified stdout, with no header or metadata (nothing from the daemon's environment or config is added). Mode `0600` in a `0700` directory.
- Written to a temp name and linked into place, so a crash never leaves a truncated file that counts toward the cap, and an existing file is never overwritten.
- Retention is a fixed count of 50, not operator-configurable. Pruning deletes only names matching Pruefer's own shape, under a package mutex because reviews run concurrently. A write or delete failure is logged and never fails the review.
- `keep_transcripts: partial|all|off` (flag `--keep-transcripts`, env `PRUEFER_KEEP_TRANSCRIPTS`, default `partial`) widens or disables retention. It is validated fail-loud like `review_guidance_mode` and is deliberately absent from the repo-resident config: a reviewed repo must not control retention (ADR 1642).
- The `coverage: partial` log line names the file only after a successful write.
- The "no staged diff" partial case is included: same `!Complete` verdict, saved whenever a transcript exists.

## Consequences

- **Privacy.** A transcript holds every tool result, including full file reads of private repositories, and is not redacted. Mitigations: local-only, git-ignored `.pruefer/`, `0700`/`0600`, a bounded count. Transcripts are never posted anywhere.
- **Disk.** The bound is by count, so the worst case is 50 times the largest transcript. The transcript was already held in memory, so peak memory is unchanged.
- If saved transcripts show a check gap, changing how `deriveOpened` credits reads is a separate issue with the transcript as evidence.

## Rejected

- Having the invoker always write a file and delete it later: I/O on every review and the policy split across two places.
- A package-level directory variable: breaks parallel tests; a `Config` field gives tests an isolated directory with no global state.
- Ordering by mtime: perturbed by copying or restoring files.

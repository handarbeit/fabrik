## Problem

Pruefer reports "no findings" on large PRs after reading only part of the diff, and says so only in the review's prose. Fabrik's review gate treats a COMMENTED review with no findings as cleared. The PRs that most need a full review get the least of one.

Evidence, quoted from Pruefer's review bodies on handarbeit/fabrik, 2026-10-11/12:
- #2165: "I read the first 556 of 823 diff lines and did not read the rest". The sim tests went unread.
- #2181: "lines 601–911 … cut off by the tool cap and I couldn't open it".
- #2173 (+1997 lines): "Bash was denied in this session, so I could not open the diff file or run any tests".
- #2167, #2185: "shell access was denied" / "Bash was denied in this session".
- On #2185 (#2180, a cache-freshness rule change), the review before the rebase passed a correctness hole. A later pass caught it as "Confirmed".

### Dependency

Blocked by #2192 on purpose. #2192 adds Task subagents, so a review may split the diff across subagents rather than one session paging one file. Coverage here must be designed for that. The tool-naming prompt text (formerly R4) is delivered by #2192's R5 and is not duplicated here.

### Causes

1. **The whole diff is one file, read through `Read`'s per-call limit.** A diff with long lines hits the token cap well before 2000 lines. The model then has to page with offsets, and it often stops after the first page. Tests, sim and later files are at the tail, so they are what gets dropped.
2. **"Bash denied" is the allowlist working as designed, not an intermittent sandbox problem.** `reviewAllowedTools` (`pruefer/claude.go`) grants only `git diff/log/show/blame/grep/status`. A review that tries `sed`/`cat`/`head` to page the diff file, or `go test`, is denied. Whether a given pass hits a denial depends on what that run tried, which is why it looks intermittent. Running the PR's tests stays out of scope: it would execute unreviewed code.
3. **Coverage isn't machine-visible.** Unread areas appear only in the summary prose. Nothing in the output contract (Go-owned, `PRUEFER_SUMMARY_*` plus the findings JSON) records them, so neither Pruefer nor Fabrik can act on them.

## Requirements

- **R1:** Present the diff so no part of it can be silently cut off: one file per changed path (or bounded chunks) plus an index listing each one with its line count. The prompt requires the review to open every entry, or to name each one it did not open. This must hold whether the review is a single session or splits the diff across #2192's subagents: every index entry must be opened by some reader, and any entry no reader opened must be named as unread.
- **R2:** Add a Go-owned coverage field to the output contract, e.g. `coverage: complete | partial` plus the unread paths. A missing or unparseable value counts as `partial`, never `complete`. A file counts toward coverage if any subagent opened it in full; the review's coverage is computed from what all subagents together opened, not from the top-level session alone. An index entry that no reader reports on counts as unread.
- **R3:** A `partial` review must say so where a machine sees it, not only in prose: a fixed, greppable line in the review body at minimum. How Fabrik's review gate treats it (keep holding, re-request, or advisory only) is a decision for the Plan stage, recorded in an ADR.
- **R4:** Withdrawn. The exact-Bash-forms and `Read` offset/limit steering text is delivered by #2192's R5; this issue builds on that version. (Number kept so R1–R3 references do not shift.)

## Scope

In: Pruefer's diff staging, review prompt (coverage and open-every-entry requirements only), output contract, and review-body rendering. Fabrik's gate handling only if Plan decides R3 needs it, as a separate chained issue.
Out: running the PR's tests or build inside the review session. The tool-naming prompt text, which #2192 owns.

## Acceptance

- A review of a diff larger than one `Read` call's limit opens every changed file, or reports `partial` and names exactly what it skipped.
- A test feeds a synthetic diff past the read limit and asserts the staged layout and index cover every path.
- A test shows a review missing the coverage field is treated as `partial`.
- A test shows a diff split across several subagents reports `complete` only when every index entry was opened by at least one of them, and `partial` naming the unopened paths otherwise.
- The tests proving the above are checked non-vacuous: with the fix neutralised, they fail.

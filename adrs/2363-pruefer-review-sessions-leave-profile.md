# ADR 2363: Pruefer review sessions leave the Claude profile; transcripts are kept 7 days

## Status

Accepted. Amends ADR 2303 (transcript retention: the default, the count bound, what is stored beside a transcript) and ADR 2375 decisions 8 and 9 (session cleanup no longer depends on `max_coverage_passes`). Keeps ADR 1642 (a reviewed repo cannot set operator settings).

## Context

Every review leaves a session under the resolved Claude profile: `<profile>/projects/<encoded cwd>/<id>.jsonl` and, when the CLI hid large tool outputs, `<id>/tool-results/*.txt`. On the maintainer's machine there were 1,921 `*-pruefer-pr-*` folders going back a month, holding full reads of reviewed code (private repositories included) in a profile shared with the operator's interactive sessions. ADR 2375 removes a review's sessions when it ends, but only with `max_coverage_passes` above 0, deletes the hidden tool outputs, and leaves the emptied encoded-cwd directory behind. The raw transcript ADR 2303 keeps under `.pruefer/transcripts/` is kept only for partial reviews and bounded by count.

## Decision

1. **Persist, then delete.** The CLI persists the session while the review runs; Pruefer still never passes `--no-session-persistence` (ADR 2375's live probe: a session started with it cannot be resumed, so the coverage loop would end at `resume failure` on every large PR). When the review ends, the evidence worth keeping moves into `.pruefer/transcripts/` and the review's own session is deleted from the profile. The end-of-review hook is #2375's deferred `removeReviewSessions` in `RealClaudeInvoker.Review`; it is extended, not moved or duplicated.
2. **Cleanup at every cap.** The deferred removal is registered unconditionally (`max_coverage_passes: 0` included). The id and glob rules are unchanged: an id is used only after it matches the UUID pattern, the encoded cwd is globbed because it is a CLI detail, and nothing but an exact `<id>.jsonl` or `<id>/` is deleted.
3. **Remove the emptied directory.** Afterwards the encoded-cwd directory is removed only if it is a direct child of `<profile>/projects/`, its name contains `pruefer-pr-<N>-` for this review's PR (trailing dash included, so PR 7 never matches `pruefer-pr-70-`), and `os.Remove` (never `RemoveAll`) succeeds, which makes the emptiness check the filesystem's. A skip is logged. A sibling review's folder, an operator's own project folder and any non-empty directory are therefore untouched. Two concurrent reviews of the same PR number share the substring; only emptiness protects the second, which is intended.
4. **Keep the hidden outputs.** `ReviewPR` creates a private staging directory (`pruefer-toolresults-pr<N>-*` in the OS temp directory, `0700`, removed by a defer) unless `keep_transcripts` is `off` and passes it as `ReviewRequest.ToolResultsStageDir`. Before deleting an `<id>/`, `removeReviewSessions` moves the regular files of `<id>/tool-results/` into it (not following symlinks, base names sanitised, numeric suffix on a collision, rename with a copy fallback across filesystems). Only when `maybeKeepTranscript` keeps a transcript are they moved on, under the transcript mutex, into `<transcript-name>.tool-results/<file>` (`0600` in a `0700` directory); otherwise they die with the staging directory. Staging could not be done in `Review()` directly because the transcript's name and the `partial` verdict exist only after `Review()` has returned and deleted the session. A staging or move failure is logged; the session is deleted regardless and the review is unchanged.
5. **Keep every transcript.** `keep_transcripts` defaults to `all` (an empty value on a bare `Config` means `all` too). `partial` and `off` keep their meaning; with `off` the session is still deleted and nothing is moved.
6. **Retire by age.** The count of 50 is gone. `transcript_max_age` (default `168h`; `--transcript-max-age`, `PRUEFER_TRANSCRIPT_MAX_AGE`, YAML a Go duration string) deletes any transcript or `.tool-results` directory whose file-name timestamp is older, never judged by mtime (ADR 2303). `transcript_max_bytes` (default 1 GiB, an integer; `--transcript-max-bytes`, `PRUEFER_TRANSCRIPT_MAX_BYTES`, YAML) is a backstop: while Pruefer's own entries total more, the oldest go first. A transcript and its `.tool-results` directory are one unit (same stem); a `.tool-results` directory with no transcript still ages out. The unit just saved is exempt from the byte ceiling, otherwise a single oversized review would delete its own evidence; it is judged by age at the next prune. Both settings are validated at load like `keep_transcripts`: zero, negative and unparseable values are refused naming the setting, including malformed env values (which the older numeric variables silently ignore), and an explicit flag `0` is seen. Both are operator-only and absent from `yamlRepoConfig` (ADR 1642).
7. **Prune after each save and once at startup**, under the existing package mutex that also covers the move, so a prune never sees a half-moved directory and counts the new bytes. Only names matching Pruefer's own shapes (`transcriptNameRE`, and a strict timestamp-first `….tool-results` shape that must be a real directory, not a symlink) are touched or counted; every other entry is left alone.

## What the CLI writes (checked 2026-10-17 on the maintainer's machine)

Under `<profile>/projects/<encoded cwd>/`: `<id>.jsonl` (the CLI's own conversation record) and an `<id>/` directory. In the 1,921 review folders present, `<id>/` held `tool-results/<name>.txt` (mode `0644`, tens of KB each, e.g. two files of 91 and 94 KB for PR 696) and, in a few, `subagents/`. Only `tool-results/` is kept; subagent transcripts are deleted with the session (the stream-json stdout does not carry subagent traffic either, `UnobservedSubagents`).

Whether Pruefer's stream-json transcript already contains everything in the profile JSONL: the tool results the CLI hid are the part that is not in stdout, because stdout shows the model the same 2 KB `<persisted-output>` preview (ADR 2362), so the `tool-results/*.txt` files are the only full copy and are what this ADR preserves. A byte-for-byte comparison of the profile JSONL against stdout was not done, so the JSONL itself is not kept.

## Consequences

- **Privacy.** The consequence ADR 2303 describes widens: every review's file reads, not just partial reviews', are kept for up to `transcript_max_age` (7 days by default). They now live in Pruefer's own `0700` directory, outside the profile shared with the operator's interactive sessions, and no review leaves anything in that profile.
- **Disk.** Bounded by age and by `transcript_max_bytes` (default 1 GiB), not by count. At roughly 400 KB per review a busy daemon can reach the ceiling inside a week; raise `transcript_max_bytes` or lower `transcript_max_age`.
- Folders left by earlier versions stay where they are. Pruefer never sweeps a Claude profile; operators delete `*-pruefer-pr-*` folders under `<profile>/projects/` by hand.
- ADR 2375 decision 9's "cap 0 leaves the session" no longer holds; its cap-0 test now asserts removal.

## Rejected

- `--no-session-persistence`: blocks the resume the coverage loop needs (ADR 2375).
- Moving the removal into `ReviewPR` after `maybeKeepTranscript`: breaks the "every return path, including errors" property of the deferred removal.
- A callback into `Review()` to place the files: it still cannot know the coverage verdict.
- Sweeping the profile for old folders: Pruefer must not delete anything it did not create in this review.
- Pruning by mtime: perturbed by copying and restoring (ADR 2303).

## Amendment (#2406): the CLI also leaves an empty `memory/`

Observed live on the dev daemon on 2026-10-19 with Claude Code `2.1.303` (the version `claude --version` reports on the maintainer's machine; the daemon's own CLI was not queried separately): besides `<id>.jsonl` and `<id>/`, the CLI creates an empty `memory/` directory in the session's encoded-cwd folder. It survives the session removal, so `os.Remove` of the folder failed with "directory not empty" and one empty folder was left per review.

`removeEmptyReviewDir` therefore removes `<folder>/memory` first, and only after the direct-child-of-`<profile>/projects/` and `pruefer-pr-<N>-` checks have passed. The removal is `os.Remove` (never `RemoveAll`) and only of a real directory (`Lstat`, so a `memory` file or symlink is left alone): a `memory/` holding any entry stays, and so does its folder. Nothing else in the folder is removed; any other entry keeps the folder and is logged as before. Failures remain non-fatal and logged. If the CLI later creates other directories, the folder is kept and logged, which is the safe failure. Folders left by earlier versions are still deleted by hand.

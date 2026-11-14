# closingrefs fixtures

Each `*.md` file is a PR body fed to `closingrefs.Find` by `closingrefs_test.go`.

## Verification status of code spans and fences: PENDING

GitHub's documentation ("Linking a pull request to an issue") lists the keywords and says
nothing about inline code spans or fenced code blocks. Nobody has yet observed what GitHub
does with `Closes #N` inside them, so the matcher treats them as matches (the safe default
for a guard). `code_span.md`, `fenced_block.md` and `unclosed_fence.md` assert that, and
the tests are annotated `UNVERIFIED against GitHub`.

`Options.ExemptCode` exists for the day this is verified; it is off by default and the
`check-closing-refs` tool does not set it.

### Probe recipe (manual, once)

Use a scratch repository you own, never this one:

1. Create issues A, B, C and D.
2. Open four PRs against the default branch whose bodies hold, respectively, `` `Closes #A` ``,
   a fenced block containing `Closes #B`, an unclosed fence containing `Closes #C`, and a
   plain `Closes #D` as the control.
3. Merge all four and record which issues closed.
4. If code is ignored by GitHub, flip the default (`ExemptCode` on in `tools/check-closing-refs`),
   update the three fixtures' expectations, and replace "PENDING" here with the result and date.

## Other deliberate choices

- `Closes #1, #2`: GitHub requires a keyword per issue, so only #1 matches.
- A keyword with no number (the PR template's bare `Closes #`) never matches.
- `fix(engine): ... (#2498)` never matches: `(` is not a separator.

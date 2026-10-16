---
name: review
description: Pruefer's default pull-request review criteria. Invoke this skill to carry out the review of the pull request under review.
---

Write a code review as you would comment on the pull request: call out bugs, correctness issues, security concerns, and significant design problems.

**Read the staged diff in amounts the tool returns whole.** When the prompt stages the diff as numbered entries, open one entry per Read, or one plain `cat` of entries by absolute path whose `bytes` in INDEX.txt add up to at most 20480 bytes. A longer shell result is saved to a file and you get only a `<persisted-output>` notice with a 2 KB preview. A `<persisted-output>` result means you did not see that content: open those entries again in smaller pieces, and do not try to read the file the notice names.

**The diff tells you what changed. It does not tell you what to check.** Reading it is the start of the review, not the whole of it. Before you conclude, follow the change outward:

- For every symbol the diff adds, removes, renames, or changes the meaning of, search the repository for its other uses. A change that is correct in its own file is still a defect if a caller, test, or script depends on the old behavior.
- Check whether the change falsifies anything written down. Grep the docs, README files, ADRs, and nearby comments for the flags, keys, functions, and behaviors the diff touches. A doc that now describes behavior the code no longer has is a real finding.
- Read the tests the diff adds or changes. Ask what would still pass if the fix were removed — a test that holds either way is not evidence.
- When the diff removes or narrows something, ask what depended on it.

**Verify before asserting.** Do not report a defect you have not confirmed by reading the relevant code. If you suspect something but cannot establish it, either check it or say plainly that it is unverified — never state it as fact. Equally, do not claim the absence of a problem ("no other call sites", "nothing else uses this") from a single narrow search; sweep properly or do not make the claim.

**Finish the review.** If some part of the change is hard to assess, say so explicitly and name what you could not check. Do not silently stop at the first file, and do not treat an early confident impression as a completed review. State what you examined, so a reader can tell a thorough pass from a partial one.

**Make every finding falsifiable.** For each one, state the concrete path to the failure: the input, state, or sequence that produces the wrong result, and what the wrong result is. "This could be a problem" is not a finding — if you cannot describe how it goes wrong, either work out whether it does, or leave it out.

**Say which findings you verified.** Begin a finding you have confirmed by reading the relevant code with "Confirmed:". Begin one you believe is real but could not establish with "Plausible:", and say what you would need to check. A reader must be able to tell the two apart without re-deriving your reasoning.

On a large PR, raise the bar for a "low"-severity finding: it must be something a reviewer would actually act on, not merely true — skip nitpicks, style preferences, and fidelity observations against test fixtures unless they matter.

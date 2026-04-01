# ADR 009: Comment Processing with Reaction Flow

## Status

Accepted

## Context

After Fabrik completes a stage, it often surfaces questions or presents research
findings. The user needs a way to provide feedback that Fabrik incorporates
into the issue. Simply re-running the stage doesn't capture the conversational
back-and-forth.

## Decision

Process user comments as a distinct workflow with visual feedback via GitHub
reactions and direct issue body updates.

## Flow

1. User comments on an issue in an active stage.
2. Fabrik adds :eyes: reaction to each new comment (signals "received").
3. `fabrik:editing` label applied (locks the issue).
4. Claude invoked with a comment-review prompt specific to the current stage.
5. Claude performs any actions requested in the comments (e.g., linking PRs, running commands).
6. If the issue body needs updating, Claude outputs the complete updated body between
   `FABRIK_ISSUE_UPDATE_BEGIN` / `FABRIK_ISSUE_UPDATE_END` markers.
7. Fabrik parses the markers and updates the issue body on GitHub (or posts output as a comment if no markers).
8. `fabrik:editing` label removed.
9. :rocket: reaction added to each comment (signals "processed").

## Rationale

- **Issue body as living document**: The issue body evolves as the workflow
  progresses. Comments add information; the body reflects the current state.
- **Visual feedback**: Reactions give the user immediate confirmation that
  their comment was seen (:eyes:) and processed (:rocket:) without cluttering
  the comment thread. The :rocket: reaction also serves as durable state —
  on restart, Fabrik skips comments that already have a :rocket: reaction,
  avoiding reprocessing.
- **Stage-specific handling**: Each stage can define a `comment_prompt` that
  knows how to incorporate feedback relevant to that stage (e.g., research
  answers vs. plan adjustments).
- **Locking**: The `fabrik:editing` label prevents concurrent processing
  during the body rewrite.

## Marker Convention

Claude is instructed to output the full updated issue body between markers:

```
FABRIK_ISSUE_UPDATE_BEGIN
(complete issue body)
FABRIK_ISSUE_UPDATE_END
```

The entire body is replaced, not patched. This is simpler and avoids merge
conflicts, though it means Claude must reproduce unchanged sections.

## Trade-offs

- **Full body replacement is destructive**: If Claude produces a malformed
  body, the original is lost. Mitigation: the original content exists in
  GitHub's issue edit history.
- **All-or-nothing**: If the markers aren't found, the output is posted as
  a comment instead. No partial body updates.
- **Comment-only from configured user**: Only comments from `--user` trigger
  processing. Other users' comments are ignored by this instance.

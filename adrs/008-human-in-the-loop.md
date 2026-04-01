# ADR 008: Human-in-the-Loop by Default

## Status

Accepted

## Context

Fabrik drives Claude Code through an SDLC pipeline. After each stage completes,
the issue could either automatically advance to the next stage or wait for
human review.

## Decision

Human-in-the-loop by default. Yolo mode (auto-advance) is opt-in via
`--yolo` flag or per-stage `auto_advance` override.

## Rationale

- **Trust but verify**: Claude Code is powerful but not infallible. A human
  checkpoint between stages catches issues early.
- **Steering**: Between stages, the human can comment to provide additional
  context, answer questions, or redirect the approach before the next stage
  picks it up.
- **Incremental trust**: Start with full oversight, enable `--yolo` for
  specific stages or the whole pipeline as confidence grows.
- **Comment processing**: The human-in-the-loop pause is where comment
  processing shines — comment on the issue, Fabrik processes it and
  updates the issue body, then advance when satisfied.

## How It Works

1. Stage completes -> `stage:<name>:complete` label added.
2. **Non-yolo** (default): Fabrik prints "waiting for human" and stops.
   The user reviews the output, optionally comments, then drags the issue
   to the next column.
3. **Yolo mode**: Fabrik automatically moves the issue to the next stage
   column via the GitHub Projects API.

## Per-Stage Override

```yaml
auto_advance: true  # This specific stage auto-advances even without --yolo
```

This allows hybrid workflows — e.g., auto-advance from Research to Plan
but require human approval before Implement.

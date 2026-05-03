# ADR 003: Polling Over Webhooks

## Status

Accepted

## Context

Fabrik needs to detect changes to the project board (issues moved between columns,
new comments). Two main approaches: webhooks (push) or polling (pull).

## Decision

Use polling via the GitHub GraphQL API at a configurable interval (default 30s).

## Rationale

- **Runs on a laptop**: Webhooks require a publicly accessible endpoint. Fabrik
  runs locally — no ngrok, no server, no DNS, no TLS certificates.
- **Single query efficiency**: GitHub's GraphQL API lets us pull the entire
  project board state in one request. This is cheap enough to do every 30s.
- **Simplicity**: No webhook registration, no event parsing, no retry logic,
  no state reconciliation after missed events.
- **Resilience**: If Fabrik restarts, it picks up where it left off on the
  next poll. No missed events to worry about.
- **Multi-user safe**: Each user's Fabrik instance polls independently.
  No shared webhook endpoint to coordinate.

## Trade-offs

- **Latency**: Changes are detected within one poll interval, not instantly.
  For an SDLC workflow where stages take minutes to hours, 30s latency is
  acceptable.
- **API usage**: At 30s intervals, that's ~2,880 requests/day per instance.
  Well within GitHub's 5,000/hour rate limit.

## Future Consideration

If real-time responsiveness becomes important, we could add optional webhook
support alongside polling, using polling as the fallback.

## Superseded by

- **ADR 032** (Webhook-Driven Event Delivery via gh webhook forward) implements this future consideration: an optional `--webhooks` mode delivers events in real-time while retaining polling as a safety net.
- **ADR 034** (Event-Sourced Board Cache via Webhook Deltas) extends ADR 032: incoming webhook payloads are applied as typed state deltas to an in-memory cache, reducing per-event GraphQL consumption. The core principle of this ADR — that polling is the reliable foundation — is preserved: the cache reconciles against GitHub every 60 minutes and falls back to live API calls when the webhook stream is unhealthy.

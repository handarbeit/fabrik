// Package channelevents is the engine-independent core of the Fabrik MCP
// channel push (#1968, ADR-1966-b): the event catalogue, the subscription
// model and its label-glob matcher, the persisted subscription registry, the
// bounded per-subscriber held queue with digest batching, and the Hub that
// routes published events to the subscriber sessions attached to it.
//
// It imports nothing from the engine, the store or GitHub, and makes no
// network call: every input arrives as an Event handed to Hub.Publish, so the
// zero-GitHub-cost rule (R9) holds structurally. A Hub never causes or alters
// an engine decision (R10) — it only observes what it is told.
package channelevents

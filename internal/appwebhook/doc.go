// Package appwebhook is the GitHub App webhook receiver shared by the engine and
// Pruefer (#2475, ADR 2475; built up by #2142, #2143, #2144, #2452 and #2549).
//
// It holds the HTTP receiver (path 404, method 405, size cap 413, HMAC 401, JSON
// 400, `ping` acknowledged, otherwise submitted to the consumer's Sink), the two
// listener sources (DirectListener, a plain TCP listener, and Funnel, the
// embedded Tailscale node's public Funnel listener), the tailnet/Funnel health
// tracker, route liveness from GitHub's delivery log (Liveness), the hook-config
// check (HookChecker) and the delivery replayer (Replayer).
//
// This package must never import the engine or Pruefer (ADR 1113); both import
// it, as they do internal/githubauth and internal/sessionreap. deps_test.go pins
// the boundary. What a consumer owns stays with it and is reached only through
// small interfaces: the delivery Sink, a read-only ledger (ReplayLedger,
// LivenessLedger: no method records a delivery), a Panel for operator warnings,
// a HealthSink for the receiver's state and a LogFunc. Dedupe, ordering, echo
// attribution and health publication are the consumer's.
//
// The Tailscale library is imported by exactly one file in the module,
// tsnetnode/node.go. This package reaches it only through the Node seam and an
// injected NodeFactory, so a consumer that wants only the direct receiver links
// no tsnet.
package appwebhook

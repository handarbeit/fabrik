// Package bedevents is the one definition of which event source a live e2e bed
// runs with on a given leg (#2408, ADR-2408), shared by the two bed launch sites
// (tests/gate/bed.go and tests/e2e/lifecycle.go), the gate's webhook delivery
// preflight and the tests that ask whether the leg has a webhook transport.
//
// The event source is a launch-injected, leg-scoped input like the bed wait
// timeouts (package bedtimeout): the launch sites strip every ambient
// event-source variable and then inject Env(). The engine's .env loading never
// overrides an already-set process variable (cmd/root.go, config.LoadDotenv), so
// the launch value wins over the bed's .env and over the operator's shell — a
// poll leg is genuinely poll-only and a webhook leg cannot be flipped by a stale
// value. The bed's own .env still supplies the per-bed values the injection
// deliberately does not touch (the tailnet hostname, the auth key and webhook
// secret variables), which is what keeps them from being shared between beds.
//
// The package is untagged and stdlib-only, like bedtimeout: tests/gate carries no
// e2e build tag and cannot import the tagged harness, and CI only compiles that
// harness, so the selection logic is covered by plain `go test ./...`.
package bedevents

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	// RequestVar is the gate input that selects the event source of a run
	// (E2E_EVENT_SOURCE). It reaches the restarted bed's harness process and every
	// test through the leg's environment, exactly as E2E_AUTH_MODE does. #2427
	// replaces it with a real matrix axis.
	RequestVar = "E2E_EVENT_SOURCE"
	// CellPollVar carries the poll cadence (seconds) the gate's plan gave the leg's
	// cell (#2426, FR-008). It is distinct from E2E_BED_POLL_SECONDS, which stays the
	// operator's override, so a planned value is never mistaken for an override.
	CellPollVar = "E2E_CELL_POLL_SECONDS"
	// OverrideVar is the operator override of the planned cadence.
	OverrideVar = "E2E_BED_POLL_SECONDS"
	// NeutraliseVar is the operator input that disables one ingestion behaviour in
	// the bed daemon for a non-vacuity run of a webhook live test (#2427, ADR
	// 2427-b). Its value (a comma-separated fault list, see engine.TestFaultsEnv)
	// is injected into the bed as FaultsLaunchVar; a leg run with it is archived
	// but never recorded in the coverage ledger.
	NeutraliseVar = "E2E_NEUTRALISE"
	// FaultsLaunchVar is the engine's hidden, test-only fault variable
	// (engine.TestFaultsEnv). It is honoured by the engine only on a bed.
	FaultsLaunchVar = "FABRIK_TEST_FAULTS"
	// AuthVar is the leg's auth mode variable the gate sets (E2E_AUTH_MODE).
	AuthVar = "E2E_AUTH_MODE"
	// LaunchVar is the engine's own variable (cmd/root.go) the launch sites inject.
	LaunchVar = "FABRIK_EVENT_SOURCE"
)

// The event sources a bed can run with.
const (
	// Poll attaches no transport: the daemon is poll-only.
	Poll = "poll"
	// Tailscale attaches the embedded tsnet Funnel receiver (#2143). It is an
	// App-auth transport.
	Tailscale = "tailscale"
)

// Poll cadences, in seconds, passed to the bed as `-poll`.
const (
	// PollOnlySeconds is the poll-only bed's cadence (unchanged since the beds existed).
	PollOnlySeconds = "60"
	// WebhookPollSeconds is the cadence of an AD HOC webhook bed run — one that does
	// not go through the gate's plan and so carries no planned cadence (#2426,
	// ADR-2426, amending ADR-2408 decision 4). It is longer than the 90 s window
	// TestWebhookChangeSurvivesReconcile waits and six times the production default
	// of 30 s. The gate never uses it: every planned cell, webhook cells included,
	// runs at PollOnlySeconds so the existing suite's fixed waits hold, and the
	// webhook path is evidenced structurally (the delivery preflight, observed
	// deliveries, HoldPolls), not by a long interval.
	WebhookPollSeconds = "180"
)

// The steps of the webhook delivery preflight, shared with probes.json so the
// failure a gate prints and the one it archives name the same thing.
const (
	StepTransportNotUp  = "transport-not-up"  // Funnel/transport not up
	StepHookURLMismatch = "hook-url-mismatch" // the App's webhook URL is not this bed's Funnel URL
	StepNoDelivery      = "no-delivery"       // transport up and URL right, but no delivery arrived
	StepProbeSetup      = "probe-setup"       // the probe could not make its write (bed .env has no FABRIK_TOKEN, scratch issue not creatable)
)

// Parse validates a requested event source. Empty is Poll; anything other than
// the two sources is an error (direct and hookdeck are not bed transports).
func Parse(v string) (string, error) {
	switch s := strings.ToLower(strings.TrimSpace(v)); s {
	case "", Poll:
		return Poll, nil
	case Tailscale:
		return Tailscale, nil
	default:
		return "", fmt.Errorf("%s=%q is invalid (must be %s, %s, or unset for %s)", RequestVar, v, Poll, Tailscale, Poll)
	}
}

// Effective is the event source a leg actually runs: Tailscale only when it was
// requested and the leg's auth is "app". The transport is App-auth only — the
// engine refuses it at startup under a PAT — so a PAT leg is always poll-only.
// An unparseable request degrades to Poll (the gate refuses it at start; a test
// process must never crash on it).
func Effective(requested, auth string) string {
	src, err := Parse(requested)
	if err != nil || src != Tailscale {
		return Poll
	}
	if strings.ToLower(strings.TrimSpace(auth)) != "app" {
		return Poll
	}
	return Tailscale
}

// FromEnv resolves the leg's effective source from a getenv (os.Getenv in the
// harness, the leg environment in the gate).
func FromEnv(getenv func(string) string) string {
	return Effective(getenv(RequestVar), getenv(AuthVar))
}

// Webhook reports whether an effective source attaches a webhook transport.
func Webhook(effective string) bool { return effective == Tailscale }

// Env returns the NAME=value entries both launch sites add after StripKeys.
func Env(effective string) []string {
	if effective != Tailscale {
		effective = Poll
	}
	return []string{LaunchVar + "=" + effective}
}

// StripKeys are the ambient variables both launch sites remove before injecting
// Env(). They are the engine's event-source selectors and the settings of the
// receiver transports; removing them means a poll leg cannot inherit a webhook
// setting from the operator's shell, and each webhook bed's own .env — never the
// shell — supplies its hostname, so two beds cannot share one.
func StripKeys() []string {
	return []string{
		LaunchVar,
		"FABRIK_TAILSCALE_HOSTNAME",
		"FABRIK_TAILSCALE_AUTH_KEY_ENV",
		"FABRIK_TAILSCALE_STATE_DIR",
		"FABRIK_APP_WEBHOOK_PATH",
		"FABRIK_APP_WEBHOOK_SECRET_ENV",
		"FABRIK_DIRECT_LISTEN",
		// The default names of the per-bed secrets: a value exported in the
		// operator's shell would otherwise beat each bed's own .env and be shared
		// between beds. (A bed that names a custom variable keeps it in its .env.)
		"FABRIK_TS_AUTHKEY",
		"FABRIK_GITHUB_WEBHOOK_SECRET",
	}
}

// ResolvePollSeconds is the `-poll` cadence a harness-side bed launch uses
// (#2426, FR-008/FR-010): the operator override first, then the cadence the
// gate's plan gave the cell, then the source's ad hoc default. A value that is not
// a positive integer is skipped.
func ResolvePollSeconds(getenv func(string) string, effective string) string {
	for _, v := range []string{getenv(OverrideVar), getenv(CellPollVar)} {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			return strconv.Itoa(n)
		}
	}
	return PollSeconds(effective)
}

// PollSeconds is the `-poll` cadence for an effective source when neither the
// operator nor the gate's plan has chosen one: PollOnlySeconds, or
// WebhookPollSeconds for an ad hoc webhook run.
func PollSeconds(effective string) string {
	if effective == Tailscale {
		return WebhookPollSeconds
	}
	return PollOnlySeconds
}

// FaultsEnv is the launch-environment entry that carries the operator's
// E2E_NEUTRALISE value to the bed as FABRIK_TEST_FAULTS, or nil when none is set.
// Both bed launch sites append it after stripping any ambient FaultsLaunchVar, so
// an operator's shell value cannot leak into an ordinary leg.
func FaultsEnv(getenv func(string) string) []string {
	v := strings.TrimSpace(getenv(NeutraliseVar))
	if v == "" {
		return nil
	}
	return []string{FaultsLaunchVar + "=" + v}
}

package appwebhook

import (
	"testing"

	"github.com/handarbeit/fabrik/internal/localapi"
)

// HealthyFor is the strict health read behind cooldown scheduling (#2532): the
// derived state must be healthy, the route verdict reachable (unknown is not
// enough) and the installation-coverage note empty.
func TestReceiverHealthyForIsStrict(t *testing.T) {
	r := New(Config{Name: "direct", Path: "/hook"})
	r.mu.Lock()
	r.listening = true
	r.mu.Unlock()

	if r.HealthyFor("o/r", "") {
		t.Error("no route verdict yet: not reliable")
	}
	r.SetRouteLiveness(localapi.RouteLiveness{Liveness: localapi.RouteUnknown})
	if !r.IsHealthyOrStartingUp() {
		t.Fatal("precondition: an unknown route still counts as healthy for the idle cap")
	}
	if r.HealthyFor("o/r", "") {
		t.Error("an unknown route is not reliable")
	}
	r.SetRouteLiveness(localapi.RouteLiveness{Liveness: localapi.RouteReachable})
	if !r.HealthyFor("o/r", "issue_comment") {
		t.Error("listening + reachable route + full coverage must be reliable")
	}
	r.SetInstallationCoverageNote([]string{"o/missing"}, false)
	if r.HealthyFor("o/r", "") {
		t.Error("an installation missing a repo makes every repo unreliable")
	}
	r.SetInstallationCoverageNote(nil, false)
	r.SetRouteLiveness(localapi.RouteLiveness{Liveness: localapi.RouteUnreachable})
	if r.HealthyFor("o/r", "") {
		t.Error("an unreachable route is not reliable")
	}
	r.SetRouteLiveness(localapi.RouteLiveness{Liveness: localapi.RouteReachable})
	r.TransitionHealth(false, "drift")
	if r.HealthyFor("o/r", "") {
		t.Error("tracked cache drift is not reliable")
	}
}

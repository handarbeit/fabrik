package gate

import (
	"os"

	"github.com/handarbeit/fabrik/tests/e2e/registry"
)

// PrintSimParitySummary is run.sh's print_sim_parity_summary (R6, #1933): one
// informational line saying how many live scenarios have a sim twin, are
// live-only, or are gaps — from tests/e2e/registry/registry.json, the single
// per-test registry (adrs/1933). It needs no budget, is called OUTSIDE the
// pre-gate so it prints even when the pre-gate is skipped, and never gates: on
// any problem it prints "unavailable" rather than a wrong count.
// E2E_PARITY_REGISTRY overrides the path (tests).
func (g *Gate) PrintSimParitySummary() {
	path := g.Getenv("E2E_PARITY_REGISTRY")
	if path == "" {
		path = g.Cfg.RepoRoot + "/tests/e2e/registry/registry.json"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		g.outf("sim parity: unavailable (registry not found: %s)\n", path)
		return
	}
	reg, err := registry.Decode(data)
	if err != nil {
		g.outf("sim parity: unavailable (cannot parse %s)\n", path)
		return
	}
	c := reg.Summary()
	g.outf("sim parity: %d covered, %d live-only, %d gap\n", c.Covered, c.LiveOnly, c.Gap)
}

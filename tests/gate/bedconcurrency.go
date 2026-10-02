package gate

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// The bed engine's worker cap (#1977, R4). The gate cannot set it: the bed is
// launched with `-notui -poll N` and no --max-concurrent, so the engine resolves
// it exactly as cmd/root.go does — FABRIK_MAX_CONCURRENT in the bed's .env, then
// config.yaml's max_concurrent, then the default of 5. The gate reads and records
// it so a measurement is interpretable; a mismatch warns and never blocks.

// engineDefaultMaxConcurrent is the engine's default worker cap (cmd/root.go).
const engineDefaultMaxConcurrent = 5

var configMaxConcurrentRE = regexp.MustCompile(`(?m)^max_concurrent:\s*(\d+)\s*(?:#.*)?$`)

// BedMaxConcurrent is the bed's effective max_concurrent and where it came from
// (".env", "config.yaml" or "default"). An invalid (non-positive or non-numeric)
// value is skipped, as the engine does.
func BedMaxConcurrent(bed string) (n int, source string) {
	if v := strings.TrimSpace(EnvFileValue(filepath.Join(bed, ".env"), "FABRIK_MAX_CONCURRENT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n, ".env"
		}
	} else if data, err := os.ReadFile(filepath.Join(bed, ".fabrik", "config.yaml")); err == nil {
		if m := configMaxConcurrentRE.FindSubmatch(data); m != nil {
			if n, err := strconv.Atoi(string(m[1])); err == nil && n > 0 {
				return n, "config.yaml"
			}
		}
	}
	return engineDefaultMaxConcurrent, "default"
}

// sharedParallel is the widest -parallel any of the phases runs at (0 when none
// parses).
func sharedParallel(phases []Phase) int {
	widest := 0
	for _, p := range phases {
		if n, err := strconv.Atoi(p.Parallel); err == nil && n > widest {
			widest = n
		}
	}
	return widest
}

// noteBedConcurrency records the bed's effective max_concurrent in the leg's
// archive (bed-concurrency.json) and warns — never blocks — when it is below the
// widest phase's -parallel: each running test needs at least one worker slot (the
// merge-train worker shares the same engine semaphore, ADR-1661), so a cap under
// -parallel makes the engine, not the host, the bottleneck. Returns the value.
func (g *Gate) noteBedConcurrency(arch *legArchive, cell Cell, phases []Phase) int {
	n, src := BedMaxConcurrent(g.Cfg.TestBed)
	par := sharedParallel(phases)
	if arch != nil && arch.dir != "" {
		rec := struct {
			MaxConcurrent int    `json:"max_concurrent"`
			Source        string `json:"source"`
			Parallel      int    `json:"widest_parallel"`
			Below         bool   `json:"below_parallel"`
		}{n, src, par, par > 0 && n < par}
		if err := os.WriteFile(filepath.Join(arch.dir, "bed-concurrency.json"), mustJSON(rec), 0o644); err != nil {
			g.errf("warning: archiving the bed's max_concurrent: %v\n", err)
		}
	}
	if par > 0 && n < par {
		g.errf("warning: leg %s runs the shared phase at -parallel=%d but the bed's max_concurrent is %d (%s): the engine's worker cap, not the host, will bound the run. Raise max_concurrent or lower E2E_PARALLEL / E2E_PARALLEL_ON (tests/e2e/README.md, \"Bed concurrency\") — this is a warning, not a gate.\n",
			cell.Label(), par, n, src)
	}
	return n
}

//go:build !windows

package engine

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// sentinelProbeTimeout bounds a single `ps` invocation. Short and fixed (R6):
// the probe must never meaningfully slow the 60s scan tick or a dispatch pass.
const sentinelProbeTimeout = 3 * time.Second

// probeSentinelLive scans the process table for a live process whose argv
// contains sentinel as an exact token — not a substring match (R6, Acceptance
// 6: a sentinel for "#2966" must not match "#29660"). It shells out to `ps`
// directly via exec.CommandContext (no shell — R6) with a short timeout, so a
// hung or missing `ps` degrades to sentinelProbeResult.Err rather than
// blocking the caller indefinitely.
//
// `-eo pid=,args=` prints just PID and the full argument list with no header
// row (the trailing "=" on each keyword suppresses the column header, which
// would otherwise itself be a spurious "row" to parse). `-w -w` (equivalent
// to BSD ps's "-ww") is appended unconditionally: on BSD/macOS ps this
// disables the command-column's truncation entirely; on GNU/Linux ps it is a
// harmless, cumulative "widen" flag. Without it, a long claude invocation's
// --name value — appended near the end of a long argv — risks being silently
// truncated away on a `ps` dialect that does truncate even non-interactively,
// which would report "not found" for a genuinely live worker (a false
// negative — exactly the bug this mechanism exists to fix).
func probeSentinelLive(sentinel string) sentinelProbeResult {
	if sentinel == "" {
		return sentinelProbeResult{Err: fmt.Errorf("sentinel probe: empty sentinel")}
	}
	ctx, cancel := context.WithTimeout(context.Background(), sentinelProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-eo", "pid=,args=", "-w", "-w").Output()
	if err != nil {
		return sentinelProbeResult{Err: fmt.Errorf("sentinel probe: ps: %w", err)}
	}

	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pidStr, argv := fields[0], fields[1:]
		for _, tok := range argv {
			if tok == sentinel {
				pid, perr := strconv.Atoi(pidStr)
				if perr != nil {
					continue
				}
				return sentinelProbeResult{Live: true, PID: pid}
			}
		}
	}
	if serr := scanner.Err(); serr != nil {
		return sentinelProbeResult{Err: fmt.Errorf("sentinel probe: scanning ps output: %w", serr)}
	}
	return sentinelProbeResult{Live: false}
}

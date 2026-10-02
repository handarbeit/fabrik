//go:build !linux && !darwin && !windows

package sessionreap

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// listPIDs is the portable fallback: one bounded `ps` call listing PIDs only.
func listPIDs() ([]int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-axo", "pid=").Output()
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, f := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(f); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func commOf(int) string { return "" }

// isZombie cannot be determined without a platform API; treat as live.
func isZombie(int) bool { return false }

// listProcs is the portable fallback: one bounded `ps` call listing PID and PPID.
func listProcs() ([]Proc, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=").Output()
	if err != nil {
		return nil, err
	}
	var procs []Proc
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		if e1 == nil && e2 == nil && pid > 0 {
			procs = append(procs, Proc{PID: pid, PPID: ppid})
		}
	}
	return procs, nil
}

// startToken is unavailable without a platform API; an empty token means a
// live leader is never trusted as a previously sampled one (fail closed).
func startToken(int) string { return "" }

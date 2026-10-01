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

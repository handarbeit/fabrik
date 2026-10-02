//go:build linux

package sessionreap

import (
	"os"
	"strconv"
	"strings"
)

func listPIDs() ([]int, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	pids := make([]int, 0, len(ents))
	for _, e := range ents {
		if pid, err := strconv.Atoi(e.Name()); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func commOf(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// isZombie reports whether pid is a dead-but-unreaped process (state Z in
// /proc/<pid>/stat). Unreadable means "not known to be a zombie".
func isZombie(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// comm (field 2) is parenthesised and may itself contain ')' or spaces, so
	// the state is the first field after the LAST ')'.
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		return false
	}
	f := strings.Fields(string(b)[i+1:])
	return len(f) > 0 && f[0] == "Z"
}

// listProcs lists every process with its parent. It reads /proc/<pid>/stat
// once per process; callers that only need PIDs use listPIDs.
func listProcs() ([]Proc, error) {
	pids, err := listPIDs()
	if err != nil {
		return nil, err
	}
	out := make([]Proc, 0, len(pids))
	for _, pid := range pids {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			continue // exited since the directory listing
		}
		// ppid is the second field after the LAST ')' (comm may contain ')').
		str := string(b)
		i := strings.LastIndexByte(str, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(str[i+1:])
		if len(f) < 2 {
			continue
		}
		ppid, err := strconv.Atoi(f[1])
		if err != nil {
			continue
		}
		out = append(out, Proc{PID: pid, PPID: ppid})
	}
	return out, nil
}

// startToken is the process start time in clock ticks since boot (field 22 of
// /proc/<pid>/stat) — stable for a process's life and different for a process
// that later reuses its PID. "" when unreadable.
func startToken(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ""
	}
	str := string(b)
	i := strings.LastIndexByte(str, ')')
	if i < 0 {
		return ""
	}
	f := strings.Fields(str[i+1:])
	// f[0] is field 3 (state), so field 22 is f[19].
	if len(f) < 20 {
		return ""
	}
	return f[19]
}

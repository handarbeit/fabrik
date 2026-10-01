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

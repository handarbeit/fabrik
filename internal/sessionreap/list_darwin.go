//go:build darwin

package sessionreap

import (
	"golang.org/x/sys/unix"
)

func listPIDs() ([]int, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	pids := make([]int, 0, len(procs))
	for i := range procs {
		if pid := int(procs[i].Proc.P_pid); pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func commOf(pid int) string {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return ""
	}
	b := make([]byte, 0, len(kp.Proc.P_comm))
	for _, c := range kp.Proc.P_comm {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}

// szomb is the BSD p_stat value of a dead-but-unreaped process (SZOMB).
const szomb = 5

// isZombie reports whether pid is a dead-but-unreaped process. An unreadable
// entry means "not known to be a zombie".
func isZombie(pid int) bool {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return false
	}
	return kp.Proc.P_stat == szomb
}

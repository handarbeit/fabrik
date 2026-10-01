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

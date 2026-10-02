//go:build darwin

package gate

import "golang.org/x/sys/unix"

func probeLoadAvg() (float64, bool) {
	b, err := unix.SysctlRaw("vm.loadavg")
	if err != nil {
		return 0, false
	}
	return parseSysctlLoadavg(b)
}

//go:build linux

package gate

import "os"

func probeLoadAvg() (float64, bool) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, false
	}
	return parseProcLoadavg(string(data))
}

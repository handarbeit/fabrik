package gate

import (
	"encoding/binary"
	"strconv"
	"strings"
)

// The host's 1-minute load average is archived at the start and end of every
// leg (R7): host load caused sim timeouts and -race crashes in the 0.0.83 gate,
// and a stalled leg is easier to explain with the load beside it. The parsers
// are untagged so they are unit-tested on every platform; the probes
// (loadavg_linux.go, loadavg_darwin.go, loadavg_other.go) are per-OS and degrade
// to "unavailable" — a probe never fails a leg.

// parseProcLoadavg reads the first field of /proc/loadavg ("0.52 0.58 0.59 ...").
func parseProcLoadavg(data string) (float64, bool) {
	f := strings.Fields(data)
	if len(f) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}

// parseSysctlLoadavg decodes darwin's `vm.loadavg` struct:
// { fixpt_t ldavg[3] (uint32); <4 bytes padding>; long fscale (int64) }.
func parseSysctlLoadavg(b []byte) (float64, bool) {
	if len(b) < 24 {
		return 0, false
	}
	fscale := int64(binary.LittleEndian.Uint64(b[16:24]))
	if fscale <= 0 {
		return 0, false
	}
	return float64(binary.LittleEndian.Uint32(b[0:4])) / float64(fscale), true
}

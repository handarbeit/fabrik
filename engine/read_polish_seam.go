package engine

import (
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
)

// readPolishOff holds one bit per requirement of the MCP read-surface polish
// (#2127, R1-R8). A set bit neutralises that requirement's behaviour, restoring
// what the daemon did before, so each requirement's test can show it fails
// without the change and is not vacuous. Production code never sets a bit.
var readPolishOff atomic.Uint32

// readPolishDisabled reports whether requirement n (1-8) is neutralised.
func readPolishDisabled(n int) bool { return readPolishOff.Load()&(1<<uint(n)) != 0 }

// SetReadPolishDisabledForTest neutralises (or restores) the #2127 requirement
// named req ("R1" ... "R8"), returning the function that restores it.
func SetReadPolishDisabledForTest(req string) (restore func()) {
	n, err := strconv.Atoi(strings.TrimPrefix(strings.ToUpper(req), "R"))
	if err != nil || n < 1 || n > 8 {
		panic(fmt.Sprintf("SetReadPolishDisabledForTest: unknown requirement %q", req))
	}
	bit := uint32(1) << uint(n)
	for {
		old := readPolishOff.Load()
		if readPolishOff.CompareAndSwap(old, old|bit) {
			break
		}
	}
	return func() {
		for {
			old := readPolishOff.Load()
			if readPolishOff.CompareAndSwap(old, old&^bit) {
				return
			}
		}
	}
}

//go:build windows

package workerowner

import "testing"

func deadPID(t *testing.T) int { t.Skip("no process-existence probe on windows"); return 0 }

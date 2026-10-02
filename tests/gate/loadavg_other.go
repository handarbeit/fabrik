//go:build !linux && !darwin

package gate

func probeLoadAvg() (float64, bool) { return 0, false }

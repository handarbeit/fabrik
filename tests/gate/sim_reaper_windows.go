//go:build windows

package gate

// The gate drives macOS/Linux hosts; there is no reaper on Windows.
func (g *Gate) simReaperOps() *simReaperOps { return nil }

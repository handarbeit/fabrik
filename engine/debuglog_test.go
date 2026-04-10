package engine

import (
	"testing"
)

// TestDebugLog_NoServer verifies that debugLog silently ignores connection errors
// when no debug server is running (the normal production case).
func TestDebugLog_NoServer(t *testing.T) {
	// debugLog connects to localhost:9876 which won't be listening in tests.
	// The function must not panic or return an error — it's best-effort only.
	debugLog("test message", map[string]interface{}{"key": "value"})
	debugLog("nil fields", nil)
	debugf("formatted %s", "message")
	// If we reach here without panic, the test passes.
}

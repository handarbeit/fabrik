package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// debugLog sends a JSON message to a local HTTP debug server.
// Silently no-ops if the server isn't running.
func debugLog(msg string, fields map[string]interface{}) {
	if fields == nil {
		fields = make(map[string]interface{})
	}
	fields["msg"] = msg
	fields["time"] = time.Now().Format("15:04:05.000")

	data, err := json.Marshal(fields)
	if err != nil {
		return
	}

	client := &http.Client{Timeout: 100 * time.Millisecond}
	resp, err := client.Post("http://localhost:9876/debug", "application/json", bytes.NewReader(data))
	if err != nil {
		return // server not running, silently ignore
	}
	resp.Body.Close()
}

func debugf(format string, args ...interface{}) {
	debugLog(fmt.Sprintf(format, args...), nil)
}

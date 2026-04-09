// Copyright (c) 2026 Fabrik Contributors. All rights reserved.

package cmd

import (
	"fmt"
	"io"
	"net/http"
	"os"
)

// RunDebugServer starts a simple HTTP server that receives debug log messages
// and prints them to stdout. Run with: fabrik _debug-server
func RunDebugServer() {
	http.HandleFunc("/debug", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(os.Stdout, "%s\n", body)
		w.WriteHeader(200)
	})
	fmt.Println("Debug server listening on :9876")
	if err := http.ListenAndServe(":9876", nil); err != nil {
		fmt.Fprintf(os.Stderr, "debug server error: %v\n", err)
	}
}

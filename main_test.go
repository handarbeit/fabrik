// Copyright (c) 2026 Fabrik Contributors. All rights reserved.

package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestMain_Help(t *testing.T) {
	// Build the binary and run with invalid args to exercise main()
	if os.Getenv("TEST_MAIN_RUN") == "1" {
		main()
		return
	}

	// Build a clean environment that strips all FABRIK_* vars so the subprocess
	// sees no config and exits with an error (missing required flags).
	// This prevents the test from hanging when a .env file is present.
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "FABRIK_") || strings.HasPrefix(kv, "GITHUB_TOKEN") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "TEST_MAIN_RUN=1")

	cmd := exec.Command(os.Args[0], "-test.run=TestMain_Help")
	cmd.Env = env
	err := cmd.Run()
	// main() should exit with error (no required flags)
	if err == nil {
		t.Fatal("expected non-zero exit for missing flags")
	}
}

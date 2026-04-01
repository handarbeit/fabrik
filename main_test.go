package main

import (
	"os"
	"os/exec"
	"testing"
)

func TestMain_Help(t *testing.T) {
	// Build the binary and run with invalid args to exercise main()
	if os.Getenv("TEST_MAIN_RUN") == "1" {
		main()
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestMain_Help")
	cmd.Env = append(os.Environ(), "TEST_MAIN_RUN=1")
	err := cmd.Run()
	// main() should exit with error (no args)
	if err == nil {
		t.Fatal("expected non-zero exit for missing flags")
	}
}

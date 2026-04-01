package main

import (
	"fmt"
	"os"

	"github.com/verveguy/fabrik/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

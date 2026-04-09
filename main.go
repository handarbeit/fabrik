// Copyright (c) 2026 Fabrik Contributors. All rights reserved.

package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/handarbeit/fabrik/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

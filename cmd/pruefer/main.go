package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/handarbeit/fabrik/pruefer"
)

func main() {
	if err := pruefer.Execute(); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

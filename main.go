package main

import (
	"errors"
	"fmt"
	"os"
)

func main() {
	code := run(os.Args[1:], os.Stdout, os.Stderr)
	os.Exit(code)
}

func run(args []string, stdout, stderr *os.File) int {
	cfg, err := parseCLI(args)
	if err != nil {
		if errors.Is(err, errUsage) {
			fmt.Fprintln(stderr, usageText())
			return 2
		}
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}

	report, exitCode, err := Execute(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitCode
	}

	if report != "" {
		fmt.Fprint(stdout, report)
	}
	return exitCode
}

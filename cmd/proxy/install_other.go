//go:build !darwin

package main

import (
	"fmt"
	"os"
	"runtime"
)

func runInstall(_ []string) int {
	fmt.Fprintf(os.Stderr, "install: only supported on darwin (current GOOS=%s)\n", runtime.GOOS)
	return 2
}

func runUninstall(_ []string) int {
	fmt.Fprintf(os.Stderr, "uninstall: only supported on darwin (current GOOS=%s)\n", runtime.GOOS)
	return 2
}

func runStatus(_ []string) int {
	fmt.Fprintf(os.Stderr, "status: only supported on darwin (current GOOS=%s)\n", runtime.GOOS)
	return 2
}

func runRegister(_ []string) int {
	fmt.Fprintf(os.Stderr, "register: only supported on darwin (current GOOS=%s)\n", runtime.GOOS)
	return 2
}

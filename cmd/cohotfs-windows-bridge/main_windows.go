//go:build windows

package main

import (
	"fmt"
	"os"

	"github.com/gosuda/cohotfs/internal/windowsbridge"
)

func main() {
	if err := windowsbridge.Execute(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

//go:build !conformance

package main

import (
	"fmt"
	"os"
)

func main() {
	deps, err := productionDepsE()
	if err != nil {
		fmt.Fprintln(os.Stderr, "maildo: invalid application wiring")
		os.Exit(2)
	}
	app, err := newAppE(deps)
	if err != nil {
		fmt.Fprintln(os.Stderr, "maildo: invalid application wiring")
		os.Exit(2)
	}
	os.Exit(app.RunIO(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

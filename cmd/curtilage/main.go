// Command curtilage is the server: it watches Frigate over MQTT,
// decides which events are news, and tells the household (DESIGN.md).
// Placeholder until phase 1; it exists so the presubmits have a
// package to check.
package main

import (
	"flag"
	"fmt"
	"os"
)

// version is set by the build (-ldflags "-X main.version=...").
var version = "dev"

func main() {
	fs := flag.NewFlagSet("curtilage", flag.ContinueOnError)
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(os.Args[1:]); err != nil {
		if err == flag.ErrHelp {
			os.Exit(0)
		}
		os.Exit(2)
	}
	if *showVersion {
		fmt.Println(versionString())
		return
	}
	fmt.Fprintln(os.Stderr, "curtilage: nothing to do yet (see docs/DESIGN.md)")
	os.Exit(2)
}

func versionString() string { return "curtilage " + version }

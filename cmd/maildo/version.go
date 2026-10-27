package main

import "runtime"

// version and commit are set by the release build through -ldflags.
var (
	version = "dev"
	commit  = "unknown"
)

func versionString() string {
	return version + " (commit " + commit + ", " + runtime.Version() + ")"
}

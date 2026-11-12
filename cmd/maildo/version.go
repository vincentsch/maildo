package main

import (
	"runtime"
	"runtime/debug"
	"strings"
)

// version and commit are set by the release build through -ldflags.
var (
	version = "dev"
	commit  = "unknown"
)

// A binary from "go install ...@vX.Y.Z" has no -ldflags, so it takes the
// version from the module instead of reporting dev.
func init() {
	if version != "dev" {
		return
	}
	if info, ok := debug.ReadBuildInfo(); ok && strings.HasPrefix(info.Main.Version, "v") {
		version = strings.TrimPrefix(info.Main.Version, "v")
	}
}

func versionString() string {
	return version + " (commit " + commit + ", " + runtime.Version() + ")"
}

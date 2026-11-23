package version

import (
	"fmt"
	"runtime"
)

// Build-time metadata injected via -ldflags.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String returns a human-readable version summary.
func String() string {
	return fmt.Sprintf("wecom-push %s (commit %s, built %s, %s/%s)", Version, Commit, Date, runtime.GOOS, runtime.GOARCH)
}

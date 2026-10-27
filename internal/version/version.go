// Package version holds build metadata injected at release time.
package version

var (
	// Version is the release version, for example "0.1.0".
	Version = "dev"
	// Commit is the git commit the binary was built from.
	Commit = "none"
	// Date is the build date.
	Date = "unknown"
)

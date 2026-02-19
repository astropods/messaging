package version

// Build-time variables set via ldflags
var (
	// Version is the semantic version (e.g., "0.1.0")
	Version = "dev"

	// Commit is the git commit SHA
	Commit = "unknown"

	// BuildDate is the date the binary was built
	BuildDate = "unknown"
)

// Info returns version information as a formatted string
func Info() string {
	return "astro-messaging " + Version + " (commit: " + Commit + ", built: " + BuildDate + ")"
}

// Package build carries version metadata injected at link time via
//
//	-ldflags "-X postra/internal/platform/build.Version=v0.3.0"
//
// It is the single source of truth for the version reported by the CLI
// (`postra version`), the MCP server implementation, and the build_info
// metric, so a release cannot report mismatched versions.
package build

// Version is the release version. "dev" indicates an unstamped local build.
var Version = "dev"

// Commit and BuildTime say which source produced this binary. A running
// process that cannot answer that turns every incident into guesswork.
// "unknown" is deliberate: an empty string would not distinguish "not stamped"
// from "failed to read".
var (
	Commit    = "unknown"
	BuildTime = "unknown"
)

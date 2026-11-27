// Package skills embeds the agent-facing skill documentation that ships with
// the mitt binary.
package skills

import "embed"

// FS holds the bundled skill directories, one subdirectory per skill. Adding
// a new skill directory requires adding it to the embed directive below.
//
//go:embed mitt
var FS embed.FS

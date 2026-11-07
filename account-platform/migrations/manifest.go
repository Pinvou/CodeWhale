// Package migrationmanifest exposes the existing immutable SQL inventory to
// the migrator binary. It is package identity, not a database history backfill.
package migrationmanifest

import _ "embed"

//go:embed manifest.json
var inventory string

// JSON returns the published inventory. Strings keep callers from mutating the
// embedded baseline; the existing manifest guard remains its update authority.
func JSON() string { return inventory }

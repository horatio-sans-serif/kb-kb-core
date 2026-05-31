// Package migrations embeds the SQL migration files so the daemon binary is
// self-contained (no migrations directory needed at runtime).
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS

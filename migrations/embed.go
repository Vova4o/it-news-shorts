// Package migrations embeds SQL migration files for PostgreSQL.
package migrations

import "embed"

// FS contains all SQL migration files ordered by version.
//
//go:embed *.sql
var FS embed.FS

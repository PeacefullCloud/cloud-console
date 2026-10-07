// Package migrations embeds the SQL schema files so the console ships as a
// single binary with no external assets.
package migrations

import "embed"

// FS holds every .sql migration, applied in filename order.
//
//go:embed *.sql
var FS embed.FS

// Package migrations exposes the SQL migration files as an embedded filesystem.
// The server calls db.RunMigrations on startup — no psql binary required.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS

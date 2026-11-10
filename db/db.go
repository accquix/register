// Package db embeds the SQL migrations so that the binary carries its own schema.
package db

import "embed"

// Migrations holds db/migrations/*.sql (goose format).
//
//go:embed migrations/*.sql
var Migrations embed.FS

// Package migrations embeds the SQL files, so a binary can migrate a database
// from an image that has no shell and no files of its own.
package migrations

import "embed"

// FS holds every migration.
//
//go:embed *.sql
var FS embed.FS

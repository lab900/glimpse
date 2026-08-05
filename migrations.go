// Package glimpse exposes the embedded SQL migrations so the server
// binary can migrate its own database on startup.
package glimpse

import "embed"

//go:embed sql/migrations/*.sql
var Migrations embed.FS

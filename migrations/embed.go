// Package migrations embeds the SQL migration files so the API binary can run
// them at startup without shipping a migrations directory alongside it. This is
// what lets the distroless runtime image contain nothing but the binary.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS

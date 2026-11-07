// Package migrations holds the SQL schema, embedded into the binary so a deploy never
// depends on a file being copied next to it.
//
// Policy: expand/contract only. Swarm's update_config rolls back to the previous binary
// when a healthcheck fails, and that binary must still be able to read the schema. So a
// migration adds columns/tables/indexes first, and drops them in a later release.
//
// Add a migration as a new numbered file (goose orders by the numeric prefix):
//
//	server/migrations/00003_add_scenes.sql
//
// then `go test ./internal/store` runs it against a fresh database. Never edit a migration
// that has shipped: Ken Thompson's "durable" rule applies to databases too.
package migrations

import "embed"

// FS is the migration set, rooted at this directory. goose walks it with `Up(db, ".")`.
//
//go:embed *.sql
var FS embed.FS

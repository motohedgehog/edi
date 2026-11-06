package migrations

import "embed"

// FS contains the Goose migrations shipped with the binary.
//
//go:embed *.sql
var FS embed.FS

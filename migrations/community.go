// Package migrations embeds the goose SQL migrations that ship inside the
// binary and run at startup.
//
// The Community baseline is deliberately small: 0001 owns identity, access,
// settings and shared infrastructure; 0002 owns the gateway domain.
//
// # Why the files are flat instead of one directory per segment
//
// goose collects migrations with `fs.Glob(fsys, "*.sql")`, which matches the
// root of the filesystem only and does not recurse (measured on v3.27).
// A directory per segment would need a merging FS in front of the provider;
// flat files plus one embed pattern per set costs nothing and keeps the glob
// working as-is.
//
// Community embeds both public baselines and no integration-owned migrations.
package migrations

import "embed"

//go:embed 0001_core.sql 0002_gateway.sql
var Community embed.FS

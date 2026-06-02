//go:build !sqlite_fts5

package main

// The daemon's FTS5 search migration (0004_fts.sql) runs
// `CREATE VIRTUAL TABLE ... USING fts5`, a module compiled into the cgo SQLite
// driver only with the sqlite_fts5 build tag. Without the tag the binary builds
// but fails at first migration ("no such module: fts5"). This deliberate compile
// error prevents shipping such a binary — build with `make build` or
// `go build -tags sqlite_fts5`.
const _ = kbRequiresSqliteFTS5BuildTag

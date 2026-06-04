//go:build crsqlite

// The cr-sqlite driver registration — gated behind the `crsqlite` build tag because
// it loads a native extension. A default build cannot reach this code, so it can
// never load native code; cr-sqlite is therefore an explicit opt-in at build time,
// on top of the runtime KB_CRSQLITE_PATH opt-in.
package crsync

import (
	"database/sql"
	"fmt"
	"sync"

	sqlite3 "github.com/mattn/go-sqlite3"
)

// driverName is the registered database/sql driver that loads the cr-sqlite
// extension on each connection. initEntrypoint is cr-sqlite's extension init symbol.
const (
	driverName     = "sqlite3_crsqlite"
	initEntrypoint = "sqlite3_crsqlite_init"
)

var (
	registerOnce  sync.Once
	extensionPath string // captured by the connect hook; first Register/Open wins
)

func registerDriver(extPath string) {
	extensionPath = extPath
	registerOnce.Do(func() {
		sql.Register(driverName, &sqlite3.SQLiteDriver{
			ConnectHook: func(conn *sqlite3.SQLiteConn) error {
				return conn.LoadExtension(extensionPath, initEntrypoint)
			},
		})
	})
}

// Register registers the cr-sqlite database/sql driver (loading the extension from
// extPath on every connection) and returns its name, for use with
// store.OpenWithDriver. It verifies the extension actually loads, returning
// ErrUnavailable otherwise so the daemon falls back to single-node. extPath == "" is
// ErrUnavailable.
func Register(extPath string) (string, error) {
	if extPath == "" {
		return "", ErrUnavailable
	}
	registerDriver(extPath)
	if !available() {
		return "", fmt.Errorf("crsync: extension at %q did not load: %w", extPath, ErrUnavailable)
	}
	return driverName, nil
}

// Available reports whether the cr-sqlite extension at extPath loads and responds —
// the runtime half of the deciding test.
func Available(extPath string) bool {
	if extPath == "" {
		return false
	}
	registerDriver(extPath)
	return available()
}

// available opens a throwaway in-memory database via the registered driver and
// confirms a cr-sqlite function responds.
func available() bool {
	db, err := sql.Open(driverName, ":memory:")
	if err != nil {
		return false
	}
	defer db.Close()
	var v int64
	return db.QueryRow(`SELECT crsql_db_version()`).Scan(&v) == nil
}

// Open opens a database whose every connection loads the cr-sqlite extension from
// extPath. Exposed for the engine tests; the daemon uses Register +
// store.OpenWithDriver so the connection carries the standard DSN pragmas.
func Open(extPath, dsn string) (*sql.DB, error) {
	registerDriver(extPath)
	return sql.Open(driverName, dsn)
}

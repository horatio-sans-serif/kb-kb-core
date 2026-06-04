//go:build !crsqlite

// The default build: no cr-sqlite. Register/Available are stubs so the daemon
// compiles and links without the native extension and runs single-node. The
// changeset operations in changes.go still compile (plain SQL), but the daemon never
// calls them because Register returns ErrUnavailable.
package crsync

// Register reports that cr-sqlite is not compiled into this binary.
func Register(extPath string) (string, error) {
	return "", ErrUnavailable
}

// Available is always false without the `crsqlite` build tag.
func Available(extPath string) bool {
	return false
}

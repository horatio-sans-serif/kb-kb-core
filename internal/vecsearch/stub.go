//go:build !vec0

package vecsearch

import "database/sql"

// Enable is a no-op without the `vec0` build tag (no native sqlite-vec compiled in).
func Enable() {}

// New reports that vec0 is not available in this build; search falls back to the
// brute-force cosine scan.
func New(write, read *sql.DB, dim int) (Index, error) {
	return nil, ErrUnavailable
}

package store

import "github.com/oklog/ulid/v2"

// NewID returns a new locally-minted ULID. ULIDs are lexicographically sortable
// by creation time and monotonic within a process, so any replica can create
// records offline without coordination (design 6.0).
func NewID() string {
	return ulid.Make().String()
}

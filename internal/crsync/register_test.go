package crsync

import (
	"errors"
	"testing"
)

// These run in both builds. In the default build Register/Available are the stubs;
// in the crsqlite build they are the real driver registration. The contract for an
// empty / unloadable path is identical: cr-sqlite is unavailable.

func TestRegister_EmptyPathUnavailable(t *testing.T) {
	driver, err := Register("")
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("Register(\"\") err = %v, want ErrUnavailable", err)
	}
	if driver != "" {
		t.Errorf("Register(\"\") driver = %q, want empty", driver)
	}
}

func TestAvailable_EmptyPathFalse(t *testing.T) {
	if Available("") {
		t.Error("Available(\"\") = true, want false")
	}
}

func TestSyncedAndLocalDisjoint(t *testing.T) {
	local := map[string]bool{}
	for _, l := range LocalTables() {
		local[l] = true
	}
	for _, s := range SyncedTables() {
		if local[s] {
			t.Errorf("table %q is classified as both synced and local", s)
		}
		if !IsSynced(s) {
			t.Errorf("IsSynced(%q) = false, want true", s)
		}
	}
}

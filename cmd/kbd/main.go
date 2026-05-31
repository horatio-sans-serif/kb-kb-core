// Command kbd is the KB Core daemon. Phase 0: open the database and apply
// pending migrations. Later phases add the job runner, event bus, and HTTP API.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"kb/internal/store"
)

func main() {
	dbPath := flag.String("db", "", "path to the SQLite database (required)")
	flag.Parse()

	if *dbPath == "" {
		fmt.Fprintln(os.Stderr, "kbd: --db is required")
		os.Exit(2)
	}

	if err := run(*dbPath); err != nil {
		log.Fatalf("kbd: %v", err)
	}
}

// run opens the database and applies migrations. Split out from main so that
// deferred cleanup runs (main's log.Fatalf would skip defers) and so the
// startup path is testable.
func run(dbPath string) error {
	db, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	applied, err := store.Migrate(db)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	log.Printf("kbd: ready (migrations applied this run: %d, db=%s)", applied, dbPath)
	return nil
}

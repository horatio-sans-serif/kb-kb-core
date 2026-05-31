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

	db, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("kbd: %v", err)
	}
	defer db.Close()

	applied, err := store.Migrate(db)
	if err != nil {
		log.Fatalf("kbd: migrate: %v", err)
	}
	log.Printf("kbd: ready (migrations applied this run: %d, db=%s)", applied, *dbPath)
}

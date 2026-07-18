// checkschema prints the list of tables in the mbgw SQLite store, so you
// can verify that InitSchema provisioned every expected table
// (readings_current, readings_history, archive_hourly, device_passport).
//
// Usage:
//
//	go run ./tools/checkschema [path]     // default path: storage.json
//
// It opens the DB through the normal sqlite driver (read-only intent) and
// queries sqlite_master. It does NOT call InitSchema, so it reports the
// file's real current state rather than creating anything.
package main

import (
	"database/sql"
	"fmt"
	"os"

	_ "modernc.org/sqlite"
)

func main() {
	path := "storage.json"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}

	db, err := sql.Open("sqlite", path+"?_busy_timeout=5000")
	if err != nil {
		fmt.Printf("open error: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
	if err != nil {
		fmt.Printf("query error: %v\n", err)
		os.Exit(1)
	}
	defer rows.Close()

	fmt.Printf("tables in %s:\n", path)
	found := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			fmt.Printf("scan error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("  - %s\n", name)
		found[name] = true
	}

	fmt.Println("\nexpected M4 tables:")
	for _, want := range []string{"readings_current", "readings_history", "archive_hourly", "device_passport"} {
		mark := "MISSING"
		if found[want] {
			mark = "ok"
		}
		fmt.Printf("  [%s] %s\n", mark, want)
	}
}

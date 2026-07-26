// Command fixarchive finds and removes bad rows in archive_hourly whose
// ts_hour is unrealistically far in the future (e.g. a decode glitch that
// produced "07.06.2044" from a corrupted single-row archive read). Such a
// row permanently shadows all real data under GetHourlyArchiveDesc's
// "ORDER BY ts_hour DESC LIMIT 1" — a well-formed but wrong future
// timestamp always sorts as "newest".
//
// This is a stand-in for a proper `sqlite3` console, which is not
// available on the target Windows Server. Build it with the same
// go1.20.14 toolchain used for mbgw.exe/akronbackfill.exe and copy the
// resulting .exe to the server alongside them.
//
// Usage:
//
//	fixarchive --db mbgw.db --device akron_real                  // list only (safe, default)
//	fixarchive --db mbgw.db --device akron_real --delete          // actually delete
//	fixarchive --db mbgw.db --device akron_real --days 1          // change the "future" threshold (default 1 day ahead of now)
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"time"

	_ "modernc.org/sqlite"
)

func main() {
	dbPath := flag.String("db", "", "SQLite path (required) — same mbgw.db the gateway uses")
	deviceID := flag.String("device", "", "device id (required) — matches config.yaml's devices[].id, e.g. akron_real")
	days := flag.Int("days", 1, "flag any row whose ts_hour is more than this many days ahead of now")
	doDelete := flag.Bool("delete", false, "actually delete the flagged rows (default: list only, changes nothing)")
	flag.Parse()

	if *dbPath == "" || *deviceID == "" {
		fmt.Fprintln(os.Stderr, "fixarchive: --db and --device are required")
		os.Exit(2)
	}

	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		fatal("открытие БД", err)
	}
	defer db.Close()

	cutoff := time.Now().AddDate(0, 0, *days)

	rows, err := db.Query(`
SELECT ts_hour, value, quality
FROM archive_hourly
WHERE device_id = ? AND ts_hour > ?
ORDER BY ts_hour DESC
`, *deviceID, cutoff)
	if err != nil {
		fatal("запрос", err)
	}

	type bad struct {
		ts      string
		value   float64
		quality string
	}
	var found []bad
	for rows.Next() {
		var b bad
		if err := rows.Scan(&b.ts, &b.value, &b.quality); err != nil {
			rows.Close()
			fatal("чтение строки", err)
		}
		found = append(found, b)
	}
	rows.Close()

	if len(found) == 0 {
		fmt.Printf("Подозрительных строк (позже %s) для %s не найдено — база чистая.\n",
			cutoff.Format("02.01.2006 15:04"), *deviceID)
		return
	}

	fmt.Printf("Найдено %d подозрительных строк для %s (дата позже %s):\n\n",
		len(found), *deviceID, cutoff.Format("02.01.2006 15:04"))
	for _, b := range found {
		fmt.Printf("  ts_hour=%s value=%v quality=%q\n", b.ts, b.value, b.quality)
	}

	if !*doDelete {
		fmt.Println("\nЭто был только просмотр — ничего не удалено.")
		fmt.Println("Чтобы удалить эти строки, добавь флаг --delete и запусти снова.")
		return
	}

	res, err := db.Exec(`
DELETE FROM archive_hourly
WHERE device_id = ? AND ts_hour > ?
`, *deviceID, cutoff)
	if err != nil {
		fatal("удаление", err)
	}
	n, _ := res.RowsAffected()
	fmt.Printf("\nУдалено строк: %d\n", n)
}

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "fixarchive: %s: %v\n", what, err)
	os.Exit(1)
}

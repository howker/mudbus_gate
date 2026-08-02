// Command fixarchive inspects and repairs archive_hourly in mbgw.db. Two
// modes:
//
//  1. Default: find (and optionally --delete) bad rows whose ts_hour is
//     unrealistically far in the future — e.g. a decode glitch that
//     produced "07.06.2044" from a corrupted single-row read, which then
//     permanently shadows all real data under GetHourlyArchiveDesc's
//     "ORDER BY ts_hour DESC LIMIT 1".
//
//  2. --show-from/--show-to: just LIST the rows stored in a time window,
//     newest first. Read-only; use it to check what the gateway actually
//     has for a given hour (e.g. confirm whether 29.07 23:00 was really
//     collected) without a sqlite3 console, which the target Windows
//     Server doesn't have.
//
// Build with the same go1.20.14 toolchain as mbgw.exe and copy alongside.
//
// Usage:
//
//	fixarchive --db mbgw.db --device akron_real                              // list suspicious future rows (safe)
//	fixarchive --db mbgw.db --device akron_real --delete                     // delete them
//	fixarchive --db mbgw.db --device akron_real --days 1                     // change the "future" threshold
//	fixarchive --db mbgw.db --device akron_real --show-from "2026-07-29 22:00" --show-to "2026-07-30 01:00"  // list a window
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"time"

	_ "modernc.org/sqlite"
)

// showLayouts are the timestamp formats accepted for --show-from/--show-to,
// tried in order. Local time (no zone) is assumed, matching how ts_hour is
// stored (meter wall-clock).
var showLayouts = []string{
	"2006-01-02 15:04",
	"2006-01-02T15:04",
	"02.01.2006 15:04",
	"2006-01-02",
}

func main() {
	dbPath := flag.String("db", "", "SQLite path (required) — same mbgw.db the gateway uses")
	deviceID := flag.String("device", "", "device id (required) — matches config.yaml's devices[].id, e.g. akron_real")
	days := flag.Int("days", 1, "flag any row whose ts_hour is more than this many days ahead of now")
	doDelete := flag.Bool("delete", false, "actually delete the flagged rows (default: list only, changes nothing)")
	showFrom := flag.String("show-from", "", "LIST mode: window start (e.g. \"2026-07-29 22:00\"). Read-only.")
	showTo := flag.String("show-to", "", "LIST mode: window end (e.g. \"2026-07-30 01:00\"). Read-only.")
	vkmRaw := flag.Bool("vkm-raw", false, "с --show-from/--show-to: вместо archive_hourly (S/ST) показать сырые строки архива ВКМ из archive_vkm_raw")
	vkmPipe := flag.Int("pipe", 1, "с --vkm-raw: номер трубы (по умолчанию 1)")
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

	// LIST mode takes precedence and never modifies anything.
	if *vkmRaw && (*showFrom != "" || *showTo != "") {
		listVKMRaw(db, *deviceID, *vkmPipe, *showFrom, *showTo)
		return
	}
	if *showFrom != "" || *showTo != "" {
		listWindow(db, *deviceID, *showFrom, *showTo)
		return
	}

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

// listWindow prints every archive_hourly row for the device in the given
// window, newest first, plus flags gaps between consecutive stored hours
// so a missing hour (like a skipped 23:00) is obvious. Read-only.
func listWindow(db *sql.DB, deviceID, fromStr, toStr string) {
	from := parseShowTime(fromStr, "--show-from", time.Time{})
	to := parseShowTime(toStr, "--show-to", time.Now().Add(time.Hour))

	rows, err := db.Query(`
SELECT ts_hour, param, value, unit, quality
FROM archive_hourly
WHERE device_id = ? AND ts_hour >= ? AND ts_hour <= ?
ORDER BY ts_hour DESC
`, deviceID, from, to)
	if err != nil {
		fatal("запрос диапазона", err)
	}
	defer rows.Close()

	type row struct {
		ts      time.Time
		param   string
		value   float64
		unit    string
		quality string
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.ts, &r.param, &r.value, &r.unit, &r.quality); err != nil {
			fatal("чтение строки диапазона", err)
		}
		got = append(got, r)
	}

	fmt.Printf("Строки archive_hourly для %s в период %s .. %s (новые сверху):\n\n",
		deviceID, from.Format("02.01.2006 15:04"), to.Format("02.01.2006 15:04"))
	if len(got) == 0 {
		fmt.Println("  (ничего не найдено — за этот период в базе нет ни одной строки)")
		return
	}
	for _, r := range got {
		fmt.Printf("  %s  %s=%v %s  [%s]\n",
			r.ts.Format("02.01.2006 15:04"), r.param, r.value, r.unit, r.quality)
	}
	fmt.Printf("\nВсего строк: %d\n", len(got))
	fmt.Println("Если какого-то часа тут нет — значит он в базе отсутствует (пропуск),")
	fmt.Println("и его надо добрать (кнопка «Опросить сейчас» на дашборде или перезапуск).")
}

// listVKMRaw prints every archive_vkm_raw row for the device/pipe in the
// given window, newest first. Read-only — used to inspect the exact text
// the device sent for a period without needing another live capture.
func listVKMRaw(db *sql.DB, deviceID string, pipe int, fromStr, toStr string) {
	from := parseShowTime(fromStr, "--show-from", time.Time{})
	to := parseShowTime(toStr, "--show-to", time.Now().Add(time.Hour))

	rows, err := db.Query(`
SELECT ts_hour, raw_string
FROM archive_vkm_raw
WHERE device_id = ? AND pipe = ? AND ts_hour >= ? AND ts_hour <= ?
ORDER BY ts_hour DESC
`, deviceID, pipe, from, to)
	if err != nil {
		fatal("запрос диапазона (archive_vkm_raw)", err)
	}
	defer rows.Close()

	type row struct {
		ts  time.Time
		raw string
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.ts, &r.raw); err != nil {
			fatal("чтение строки диапазона (archive_vkm_raw)", err)
		}
		got = append(got, r)
	}

	fmt.Printf("Сырые строки archive_vkm_raw для %s (труба %d) в период %s .. %s (новые сверху):\n\n",
		deviceID, pipe, from.Format("02.01.2006 15:04"), to.Format("02.01.2006 15:04"))
	if len(got) == 0 {
		fmt.Println("  (ничего не найдено — за этот период в базе нет ни одной строки)")
		return
	}
	for _, r := range got {
		fmt.Printf("  %s:\n    %s\n\n", r.ts.Format("02.01.2006 15:04"), r.raw)
	}
	fmt.Printf("Всего строк: %d\n", len(got))
}

func parseShowTime(s, flagName string, fallback time.Time) time.Time {
	if s == "" {
		return fallback
	}
	for _, layout := range showLayouts {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t
		}
	}
	fatal("разбор времени "+flagName, fmt.Errorf("не распознан формат %q (примеры: \"2026-07-29 23:00\", \"29.07.2026 23:00\")", s))
	return time.Time{}
}

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "fixarchive: %s: %v\n", what, err)
	os.Exit(1)
}

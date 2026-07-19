// seedakron creates a fresh SQLite database pre-filled with realistic
// Akron data for the upstream carrier smoke test on the Энергосфера
// server. Run once on the dev machine, then copy the resulting DB file
// to the server alongside mbgw.exe.
//
// Usage:
//
//	go run ./tools/seedakron [output.db]     // default: akron_smoke.db
//
// The seeded data:
//   - device passport (serial 12345, type 0x00, firmware 3.7)
//   - current readings (V, Q, acc_time — same fixture values as the sim)
//   - 48 consecutive hourly archive rows, newest = 1 hour ago, stepping
//     back in time — so rows always fall inside Энергосфера's ОИ debt
//     window regardless of when the carrier is started on the server.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"mbgw/internal/storage"
	sqliterepo "mbgw/internal/storage/sqlite"
)

func main() {
	path := "akron_smoke.db"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	// Remove stale file so we start clean.
	_ = os.Remove(path)

	repo, err := sqliterepo.New(path)
	if err != nil {
		fmt.Printf("open: %v\n", err)
		os.Exit(1)
	}
	ctx := context.Background()
	if err := repo.InitSchema(ctx); err != nil {
		fmt.Printf("schema: %v\n", err)
		os.Exit(1)
	}

	deviceID := "acron_1"
	now := time.Now()

	// 1. Passport
	if err := repo.SaveDevicePassport(ctx, storage.DevicePassport{
		DeviceID:   deviceID,
		Serial:     12345,
		DeviceType: 0x00,
		Firmware:   "3.7",
		UpdatedAt:  now,
	}); err != nil {
		fmt.Printf("passport: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("passport: serial=12345 type=0x00 fw=3.7")

	// 2. Current readings
	for _, pt := range []struct {
		name, val, unit string
	}{
		{"V", "0.483", "m/s"},
		{"Q", "28.966", "m3/h"},
		{"acc_time", "1710", "min"},
	} {
		if err := repo.SaveReadingCurrent(ctx, storage.ReadingCurrent{
			DeviceID:  deviceID,
			PointID:   pt.name,
			Value:     pt.val,
			Unit:      pt.unit,
			Timestamp: now,
		}); err != nil {
			fmt.Printf("reading %s: %v\n", pt.name, err)
			os.Exit(1)
		}
		fmt.Printf("reading: %s = %s %s\n", pt.name, pt.val, pt.unit)
	}

	// 3. Hourly archive: 200 rows (~8.3 days), newest = 1h ago, stepping
	// back. Covers a 7-day (168h) ЭС debt window with margin — the debt
	// window is operator-configurable in Энергосфера, and a real Akron's
	// own ring buffer holds up to 1925 hours (~80 days) per the protocol
	// doc, so 200 is a conservative smoke-test depth, not a hard limit.
	// Volume starts at 582.7 and decreases by 0.1 per hour (older = less
	// accumulated), giving visually distinct values per row.
	base := now.Truncate(time.Hour).Add(-1 * time.Hour) // latest row = previous full hour
	const rowCount = 200
	saved := 0
	for i := 0; i < rowCount; i++ {
		ts := base.Add(-time.Duration(i) * time.Hour)
		vol := 582.7 - float64(i)*0.1
		if err := repo.SaveHourlyArchive(ctx, storage.HourlyArchiveRecord{
			DeviceID: deviceID,
			Channel:  "",
			Param:    "V",
			TsHour:   ts,
			Value:    vol,
			Unit:     "m3",
		}); err != nil {
			fmt.Printf("archive row %d: %v\n", i, err)
			continue
		}
		saved++
	}
	fmt.Printf("archive: %d hourly rows (%s .. %s)\n", saved,
		base.Add(-(rowCount-1)*time.Hour).Format("02.01.2006 15:04"),
		base.Format("02.01.2006 15:04"))

	n, _ := repo.CountHourlyArchive(ctx, deviceID, "", "V")
	fmt.Printf("\nГотово: %s (%d часовок, паспорт, текущие)\n", path, n)
	fmt.Println("Скопируй на сервер ЭС вместе с mbgw.exe")
}

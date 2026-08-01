package device

import (
	"context"
	"fmt"
	"log"
	"time"

	"mbgw/internal/archive"
	"mbgw/internal/profile"
	"mbgw/internal/storage"
)

// vkmDefaultBackfillDepthHours is how far back a VKM startup/manual
// backfill reaches by default (opts.MaxDepthHours overrides). Kept far
// shallower than Akron's 648h default: Akron's archive is one cheap
// indexed-paged read for many hours at once, but VKM's
// mb_request_poll_string strategy returns ONE aggregate per request —
// CONFIRMED against real hardware (2026-08-01): S/ST scale linearly with
// the requested period length (a 24h request returned ~25x a 1h
// request's totals), i.e. there is no per-hour breakdown to page
// through. Getting hourly granularity out of VKM means one full
// write/poll/read request PER HOUR (each taking real wall-clock seconds
// — see mb_request_poll_string.go's vkmArchCollectTimeout), so reaching
// back 648h the way Akron does would mean ~648 sequential device
// round-trips at startup. 24h (one day) is a sane default; deeper reaches
// are opt-in via config, not the default.
const vkmDefaultBackfillDepthHours = 24

// vkmHourlyParams are the fields persisted to archive_hourly from one VKM
// archive result — the period-INTEGRATED totals, not the instantaneous
// snapshots. CONFIRMED against real hardware: S (mass) and ST (thermal
// energy) scale linearly with the requested period length, exactly what
// a per-hour archive row needs; Pi/Pbar/T/dP/H are end-of-period
// instantaneous readings and are deliberately NOT saved here.
var vkmHourlyParams = []string{"S", "ST"}

// persistVKMHourly saves vkmHourlyParams from one VKM archive result as
// archive_hourly rows tagged to hourStart (the specific hour the request's
// From/To window covered — NOT time.Now(), since a backfilled hour is by
// definition in the past).
func persistVKMHourly(ctx context.Context, d *Device, hourStart time.Time, rec archive.ArchiveRecord) int {
	saved := 0
	for _, param := range vkmHourlyParams {
		v, ok := fieldFloat(rec.Fields, param)
		if !ok {
			log.Printf("[%s] VKM час %s: поле %s отсутствует в ответе прибора (поля=%v)\n",
				d.ID, hourStart.Format("02.01.2006 15:00"), param, rec.Fields)
			continue
		}
		unit, _ := rec.Fields[param+"_unit"].(string)

		r := storage.HourlyArchiveRecord{
			DeviceID: d.ID,
			Channel:  "",
			Param:    param,
			TsHour:   hourStart,
			Value:    v,
			Unit:     unit,
		}
		if err := d.Repo.SaveHourlyArchive(ctx, r); err != nil {
			log.Printf("[%s] VKM час %s: ошибка сохранения %s: %v\n",
				d.ID, hourStart.Format("02.01.2006 15:00"), param, err)
			continue
		}
		saved++
	}
	return saved
}

// collectVKMHour issues ONE archive request/wait/read dance for the
// window [hourStart, hourStart+1h) and persists whatever comes back.
// Returns how many of vkmHourlyParams were actually saved (0 without an
// error is a legitimate outcome — e.g. the device had no data for that
// hour).
func (d *Device) collectVKMHour(ctx context.Context, a profile.Archive, hourStart time.Time) (int, error) {
	reader, ok := archive.Get(a.Strategy)
	if !ok {
		return 0, fmt.Errorf("неизвестная стратегия %s", a.Strategy)
	}

	q := archive.ArchiveQuery{
		DeviceID:  d.ID,
		ArchiveID: a.ID,
		Instance:  1,
		From:      hourStart,
		To:        hourStart.Add(time.Hour),
		Params:    a.Params,
	}

	release, leaseErr := d.Lease.Acquire(ctx, d.ID, a.ID, 30*time.Second)
	if leaseErr != nil {
		return 0, fmt.Errorf("lease: %w", leaseErr)
	}
	defer release()

	records, err := reader.Read(ctx, sessionAdapter{d.Sess}, d.Client, q)
	if err != nil {
		return 0, err
	}
	if len(records) == 0 {
		return 0, nil
	}

	return persistVKMHourly(ctx, d, hourStart, records[0]), nil
}

// pollVKMHourlyLatest is VKM's regular per-cycle archive poll — the
// counterpart to Akron's hourly PollArchives tick. Requests exactly the
// most recently COMPLETED hour (now truncated to the hour, minus one) —
// paired with the archive_at_minute scheduling anchor (config.yaml's
// devices[].backfill.archive_at_minute), this naturally lands the request
// shortly after that hour closes, the same timing fix already applied for
// Akron's "ЭС reads before the hour is collected" bug.
func (d *Device) pollVKMHourlyLatest(ctx context.Context, a profile.Archive) {
	hourStart := time.Now().Truncate(time.Hour).Add(-time.Hour)

	saved, err := d.collectVKMHour(ctx, a, hourStart)
	if err != nil {
		log.Printf("[%s] VKM архив %s: час %s: ошибка: %v\n",
			d.ID, a.ID, hourStart.Format("02.01.2006 15:00"), err)
		return
	}
	log.Printf("[%s] VKM архив %s: час %s: сохранено полей: %d/%d\n",
		d.ID, a.ID, hourStart.Format("02.01.2006 15:00"), saved, len(vkmHourlyParams))
}

// backfillVKMHourly is VKM's counterpart to Akron's deep startup sweep —
// but one archive REQUEST per missing hour (not one cheap indexed page
// covering many hours), so depth defaults much shallower
// (vkmDefaultBackfillDepthHours) and every hour costs a real
// multi-second round-trip. Only reaches back over FULLY completed hours
// (never the current, still-open hour).
func (d *Device) backfillVKMHourly(ctx context.Context, a profile.Archive, opts BackfillOptions) {
	depthLimit := vkmDefaultBackfillDepthHours
	if opts.MaxDepthHours > 0 {
		depthLimit = opts.MaxDepthHours
	}

	toBoundary := time.Now().Truncate(time.Hour).Add(-time.Hour) // last fully-completed hour
	fromBoundary := toBoundary.Add(-time.Duration(depthLimit-1) * time.Hour)

	missing, err := d.Repo.MissingHours(ctx, d.ID, "", "S", fromBoundary, toBoundary)
	if err != nil {
		log.Printf("[%s] VKM дозабор %s: не удалось вычислить пропуски: %v\n", d.ID, a.ID, err)
		return
	}
	if len(missing) == 0 {
		log.Printf("[%s] VKM дозабор %s: пропусков в пределах %dч нет\n", d.ID, a.ID, depthLimit)
		return
	}

	log.Printf("[%s] VKM дозабор %s: старт, пропущено часов: %d из %d (по одному запросу на час — дороже, чем у Акрона, наберись терпения)\n",
		d.ID, a.ID, len(missing), depthLimit)

	hoursFilled := 0
	for _, hour := range missing {
		select {
		case <-ctx.Done():
			log.Printf("[%s] VKM дозабор %s: прервано контекстом (заполнено часов: %d/%d)\n",
				d.ID, a.ID, hoursFilled, len(missing))
			return
		default:
		}

		saved, err := d.collectVKMHour(ctx, a, hour)
		if err != nil {
			log.Printf("[%s] VKM дозабор %s: час %s: ошибка: %v\n",
				d.ID, a.ID, hour.Format("02.01.2006 15:00"), err)
			continue
		}
		if saved > 0 {
			hoursFilled++
		}
		log.Printf("[%s] VKM дозабор %s: час %s: сохранено полей: %d/%d\n",
			d.ID, a.ID, hour.Format("02.01.2006 15:00"), saved, len(vkmHourlyParams))
	}
	log.Printf("[%s] VKM дозабор %s: готово, заполнено часов: %d/%d\n", d.ID, a.ID, hoursFilled, len(missing))
}

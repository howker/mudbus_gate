package device

import (
	"context"
	"log"
	"time"

	"mbgw/internal/archive"
	"mbgw/internal/profile"
)

// BackfillOptions controls one archive catch-up run. Zero values mean
// "use the profile/strategy defaults" (variant "В", no extra cap), so a
// caller with no configuration still gets correct default behaviour.
type BackfillOptions struct {
	// MaxDepthHours: 0 → variant "В" (grab everything the DB is missing,
	// bounded only by the device's physical buffer). >0 → variant "Б"
	// (never reach further back than this many hours).
	MaxDepthHours int
}

// BackfillArchives performs a deep, paged history catch-up for every
// index-addressable archive in the profile (currently the Akron hourly
// archive). It is the штатный replacement for the manual tools/akronbackfill
// run: called once at process startup, it walks the device's ring buffer
// from newest to oldest in pages of the profile's max_rows_per_request,
// persisting every valid row, and stops at the first of:
//
//   - an empty/short page  → the device's archive ends here (also the
//     empty/freshly-installed-meter case: first page returns nothing, we
//     log "архив прибора пуст" and return without writing anything);
//   - reaching a row at-or-older-than what the DB already has (variant "В"),
//     so re-runs only fetch genuinely new history and overlap is cheap;
//   - reaching opts.MaxDepthHours back (variant "Б", when configured);
//   - the profile's buffer_depth_hours safety cap (prevents an endless
//     loop if a device keeps answering past its real buffer).
//
// Rows with an invalid BCD date or missing volume are skipped by
// persistAkronHourly (same guard the poller uses), so a corrupted or
// zeroed device never poisons the store.
func (d *Device) BackfillArchives(ctx context.Context, opts BackfillOptions) {
	for _, a := range d.Profile.Archives {
		if a.Strategy != "akron_archive" {
			continue // only the index-addressable Akron archive for now
		}
		kind, _ := a.Params["archive_kind"].(string)
		if kind != "hourly" {
			continue
		}
		d.backfillAkronHourly(ctx, a, opts)
	}
}

func (d *Device) backfillAkronHourly(ctx context.Context, a profile.Archive, opts BackfillOptions) {
	reader, ok := archive.Get(a.Strategy)
	if !ok {
		log.Printf("[%s] дозабор %s: неизвестная стратегия %s\n", d.ID, a.ID, a.Strategy)
		return
	}
	layout := layoutFromProfile(a)

	pageSize := a.MaxRowsOrDefault()
	bufferDepth := a.BufferDepthOrDefault()

	// Depth limit in hours: the tighter of the physical buffer cap and the
	// configured variant-"Б" cap (if any). Variant "В" (opts.MaxDepthHours
	// == 0) leaves it at the buffer cap and relies on the "already have it"
	// stop below.
	depthLimit := bufferDepth
	if opts.MaxDepthHours > 0 && opts.MaxDepthHours < depthLimit {
		depthLimit = opts.MaxDepthHours
	}

	// Variant "В": don't reach past what we already stored. A row whose
	// hour is <= newestHave means we (and everything older) already have
	// it, so we can stop.
	newestHave, haveAny, err := d.Repo.LatestHourlyArchiveTS(ctx, d.ID, "", "V")
	if err != nil {
		log.Printf("[%s] дозабор %s: не удалось узнать последнюю сохранённую строку: %v\n", d.ID, a.ID, err)
		// Non-fatal: fall through and just use the depth cap.
		haveAny = false
	}

	log.Printf("[%s] дозабор %s: старт (страница=%d строк, предел=%dч, %s)\n",
		d.ID, a.ID, pageSize, depthLimit,
		map[bool]string{true: "добираем новее последней сохранённой", false: "база пуста — тянем всё до конца буфера"}[haveAny])

	totalSaved := 0
	reachedEnd := false
	for from := 0; from < depthLimit; from += pageSize {
		to := from + pageSize - 1
		if to >= depthLimit {
			to = depthLimit - 1
		}

		release, leaseErr := d.Lease.Acquire(ctx, d.ID, a.ID, 30*time.Second)
		if leaseErr != nil {
			log.Printf("[%s] дозабор %s: не удалось занять lease: %v\n", d.ID, a.ID, leaseErr)
			return
		}
		q := archive.ArchiveQuery{
			DeviceID:     d.ID,
			ArchiveID:    a.ID,
			Instance:     1,
			FromIndex:    from,
			ToIndex:      to,
			RecordLayout: layout,
			WordOrder32:  d.Profile.Codec.WordOrder32,
			WordOrder64:  d.Profile.Codec.WordOrder64,
			Params:       a.Params,
		}
		records, err := reader.Read(ctx, sessionAdapter{d.Sess}, d.Client, q)
		release()

		if err != nil {
			log.Printf("[%s] дозабор %s: страница i=%d..%d: ошибка чтения: %v — остановка (дальше в буфере, видимо, пусто)\n",
				d.ID, a.ID, from+1, to+1, err)
			reachedEnd = true
			break
		}
		if len(records) == 0 {
			log.Printf("[%s] дозабор %s: страница i=%d..%d: пусто — архив прибора закончился\n",
				d.ID, a.ID, from+1, to+1)
			reachedEnd = true
			break
		}

		saved := persistAkronHourly(ctx, d.Repo, d.ID, a, records)
		totalSaved += saved
		log.Printf("[%s] дозабор %s: страница i=%d..%d: получено %d, сохранено %d\n",
			d.ID, a.ID, from+1, to+1, len(records), saved)

		// Variant "В" stop: if the OLDEST row on this page is already
		// at-or-older-than what we had before this run, everything deeper
		// is already stored — stop paging.
		if haveAny && oldestReachedOrOlder(records, newestHave) {
			log.Printf("[%s] дозабор %s: дошли до уже сохранённых данных (<= %s) — остановка\n",
				d.ID, a.ID, newestHave.Format("02.01.2006 15:00"))
			reachedEnd = true
			break
		}
	}

	if !reachedEnd {
		log.Printf("[%s] дозабор %s: достигнут предел глубины %dч — остановка по лимиту\n",
			d.ID, a.ID, depthLimit)
	}
	log.Printf("[%s] дозабор %s: готово, сохранено строк: %d\n", d.ID, a.ID, totalSaved)
}

// GapScan patches individual missing hours in the recent window
// [now-windowHours, now]. Unlike the startup backfill (a contiguous deep
// sweep), this targets holes that can appear from a single failed hourly
// read without a full outage. It reads each missing hour's neighbourhood
// by index and lets SaveHourlyArchive's upsert fill only what's absent.
//
// Implementation note: the Akron archive is index- not time-addressed, so
// we can't ask "give me hour X" directly. Instead, when gaps exist in the
// window we re-run a bounded backfill over just that window's depth — the
// upsert makes re-fetching already-present hours harmless (idempotent),
// and the missing ones get filled. This keeps one code path for the actual
// device I/O while still being driven by the precise gap list.
func (d *Device) GapScan(ctx context.Context, windowHours int) {
	if windowHours <= 0 {
		return
	}
	now := time.Now()
	fromHour := now.Add(-time.Duration(windowHours) * time.Hour).Truncate(time.Hour)
	toHour := now.Truncate(time.Hour)

	missing, err := d.Repo.MissingHours(ctx, d.ID, "", "V", fromHour, toHour)
	if err != nil {
		log.Printf("[%s] gap-scan: ошибка поиска пропусков: %v\n", d.ID, err)
		return
	}
	if len(missing) == 0 {
		return // nothing to patch, stay quiet
	}
	log.Printf("[%s] gap-scan: найдено пропущенных часов в последних %dч: %d — латаю\n",
		d.ID, windowHours, len(missing))

	// Re-sweep just the window depth; upsert fills the holes, overlap is
	// cheap. +2 pages of slack so a gap right at the window edge is still
	// reachable given index granularity.
	d.BackfillArchives(ctx, BackfillOptions{MaxDepthHours: windowHours + 2})
}

// oldestReachedOrOlder reports whether the oldest record in the page is at
// or older than the given timestamp — the variant-"В" stop condition.
func oldestReachedOrOlder(records []archive.ArchiveRecord, newestHave time.Time) bool {
	for _, rec := range records {
		ts, ok := akronRowTime(rec.Fields)
		if !ok {
			continue
		}
		if !ts.After(newestHave) {
			return true
		}
	}
	return false
}

// layoutFromProfile converts a profile Archive's record layout into the
// archive package's layout type — the same mapping PollArchives does
// inline, factored out so backfill and the poller stay in lockstep.
func layoutFromProfile(a profile.Archive) []archive.RecordLayoutField {
	layout := make([]archive.RecordLayoutField, 0, len(a.RecordLayout))
	for _, f := range a.RecordLayout {
		layout = append(layout, archive.RecordLayoutField{
			Offset: f.Offset,
			Name:   f.Name,
			Type:   f.Type,
			Unit:   f.Unit,
			Scale:  f.Scale,
			CRC:    f.CRC,
			Epoch:  f.Epoch,
		})
	}
	return layout
}

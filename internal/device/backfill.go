package device

import (
	"context"
	"log"
	"time"

	"mbgw/internal/archive"
	"mbgw/internal/health"
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

	// SkipLatest is used only by the scheduled IVK-TЭР path after it has
	// already performed the mandatory live read of the newest completed hour.
	// Startup/manual backfill leaves this false.
	SkipLatest bool
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
//   - every hour the DB was missing in the candidate window has now been
//     covered (variant "В") — computed as an explicit missing-hours set
//     up front, NOT inferred from "older than the previously-newest row":
//     that inference breaks whenever a gap sits BEHIND fresher data (a
//     live poll can keep the newest hour current while a multi-day outage
//     leaves a hole further back — the exact 27.07–29.07 incident this
//     fixes), so it must not be used as the stop signal;
//   - reaching opts.MaxDepthHours back (variant "Б", when configured);
//   - the profile's buffer_depth_hours safety cap (prevents an endless
//     loop if a device keeps answering past its real buffer).
//
// Rows with an invalid BCD date or missing volume are skipped by
// persistAkronHourly (same guard the poller uses), so a corrupted or
// zeroed device never poisons the store.
func (d *Device) BackfillArchives(ctx context.Context, opts BackfillOptions) {
	// KindBackfill is queued by the scheduler without per-task options.
	// In that path use the device's configured startup depth. Explicit
	// callers (for example GapScan) still win by passing MaxDepthHours.
	effective := opts
	if effective.MaxDepthHours <= 0 && d.BackfillMaxDepthHours > 0 {
		effective.MaxDepthHours = d.BackfillMaxDepthHours
	}

	for _, a := range d.Profile.Archives {
		switch a.Strategy {
		case "akron_archive":
			kind, _ := a.Params["archive_kind"].(string)
			if kind != "hourly" {
				continue
			}
			d.backfillAkronHourly(ctx, a, effective)
		case "mb_request_poll_string":
			d.backfillVKMHourly(ctx, a, effective)
		case "mb_func65":
			d.backfillFunc65Hourly(ctx, a, effective)
		default:
			continue // strategy has no backfill path (yet)
		}
	}
}

func (d *Device) backfillAkronHourly(ctx context.Context, a profile.Archive, opts BackfillOptions) {
	reader, ok := archive.Get(a.Strategy)
	if !ok {
		log.Printf("[%s] восстановление архива %s: неизвестный способ чтения %s\n", d.ID, a.ID, a.Strategy)
		return
	}
	layout := layoutFromProfile(a)

	pageSize := a.MaxRowsOrDefault()
	bufferDepth := a.BufferDepthOrDefault()

	// Depth limit in hours: the tighter of the physical buffer cap and the
	// configured variant-"Б" cap (if any). Variant "В" (opts.MaxDepthHours
	// == 0) leaves it at the buffer cap.
	depthLimit := bufferDepth
	if opts.MaxDepthHours > 0 && opts.MaxDepthHours < depthLimit {
		depthLimit = opts.MaxDepthHours
	}

	// What's actually missing across the whole candidate window, computed
	// ONCE up front. This is the correct stop signal for variant "В" — NOT
	// "we paged past the previously-newest row", which breaks as soon as
	// there's a gap OLDER than data that's already fresh (exactly the
	// 27.07–29.07 outage case: the newest row is fresh from live polling,
	// but there's a multi-day hole behind it that a single page never
	// reaches if we stop at "older than newest"). See PROJECT log
	// 2026-07-29 for the incident this fixes.
	// fromBoundary/toBoundary must span exactly depthLimit hourly slots —
	// indices 0..depthLimit-1, the same set the paging loop below can
	// actually reach (from < depthLimit). Using -depthLimit hours here
	// would make the range inclusive of depthLimit+1 slots (a fencepost
	// mismatch caught by TestBackfillArchives_ReachesGapBehindFreshData:
	// the oldest hour in that wider range is never fetchable, so it always
	// shows up as a permanently "missing" hour even on a fully successful
	// run).
	now := time.Now()
	fromBoundary := now.Add(-time.Duration(depthLimit-1) * time.Hour).Truncate(time.Hour)
	toBoundary := now.Truncate(time.Hour)
	missingList, err := d.Repo.MissingHours(ctx, d.ID, "", "V", fromBoundary, toBoundary)
	if err != nil {
		log.Printf("[%s] восстановление архива %s: не удалось определить отсутствующие часы (%v); проверяю историю до доступного предела\n", d.ID, a.ID, err)
	}
	missing := make(map[int64]bool, len(missingList))
	for _, t := range missingList {
		missing[t.Unix()] = true
	}

	if err == nil && len(missing) == 0 {
		log.Printf("[%s] восстановление архива %s: за последние %d ч отсутствующих часов нет\n", d.ID, a.ID, depthLimit)
		return
	}

	log.Printf("[%s] восстановление архива %s: начинаю проверку; за один запрос до %d записей, глубина до %d ч, отсутствующих часов %d\n",
		d.ID, a.ID, pageSize, depthLimit, len(missing))

	totalSaved := 0
	reachedEnd := false
	for from := 0; from < depthLimit; from += pageSize {
		to := from + pageSize - 1
		if to >= depthLimit {
			to = depthLimit - 1
		}

		release, leaseErr := d.Lease.Acquire(ctx, d.ID, a.ID, 30*time.Second)
		if leaseErr != nil {
			log.Printf("[%s] восстановление архива %s: прибор занят другим опросом; не удалось дождаться доступа: %v\n", d.ID, a.ID, leaseErr)
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
			log.Printf("[%s] восстановление архива %s: ошибка чтения позиций %d..%d: %v; восстановление остановлено\n",
				d.ID, a.ID, from+1, to+1, err)
			reachedEnd = true
			break
		}
		if len(records) == 0 {
			log.Printf("[%s] восстановление архива %s: позиции %d..%d пусты — достигнут конец доступного архива прибора\n",
				d.ID, a.ID, from+1, to+1)
			reachedEnd = true
			break
		}

		saved := persistAkronHourly(ctx, d.Repo, d.ID, a, records)
		totalSaved += saved
		health.MarkPollProgress(d.ID, time.Now())
		log.Printf("[%s] восстановление архива %s: позиции %d..%d — получено %d записей, сохранено %d\n",
			d.ID, a.ID, from+1, to+1, len(records), saved)

		// Cross off every hour this page actually covered — including
		// hours outside the original missing-set (harmless, upsert is
		// idempotent) so a re-check below only cares about what's left.
		for _, rec := range records {
			ts, ok := akronRowTime(rec.Fields)
			if !ok {
				continue
			}
			delete(missing, ts.Truncate(time.Hour).Unix())
		}

		if len(missing) == 0 {
			log.Printf("[%s] восстановление архива %s: все обнаруженные отсутствующие часы восстановлены\n", d.ID, a.ID)
			reachedEnd = true
			break
		}
	}

	if !reachedEnd {
		log.Printf("[%s] восстановление архива %s: достигнута глубина %d ч; осталось отсутствующих часов: %d\n",
			d.ID, a.ID, depthLimit, len(missing))
	}
	log.Printf("[%s] восстановление архива %s завершено: сохранено записей %d\n", d.ID, a.ID, totalSaved)
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
	fromHour := now.Add(-time.Duration(windowHours-1) * time.Hour).Truncate(time.Hour)
	toHour := now.Truncate(time.Hour)

	missing, err := d.Repo.MissingHours(ctx, d.ID, "", "V", fromHour, toHour)
	if err != nil {
		log.Printf("[%s] контроль полноты архива: не удалось определить отсутствующие часы: %v\n", d.ID, err)
		return
	}
	if len(missing) == 0 {
		return // nothing to patch, stay quiet
	}
	log.Printf("[%s] контроль полноты архива: за последние %d ч найдено отсутствующих часов: %d; начинаю восстановление\n",
		d.ID, windowHours, len(missing))

	// Re-sweep just the window depth; upsert fills the holes, overlap is
	// cheap. +2 pages of slack so a gap right at the window edge is still
	// reachable given index granularity.
	d.BackfillArchives(ctx, BackfillOptions{MaxDepthHours: windowHours + 2})
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

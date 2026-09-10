package device

import (
	"context"
	"fmt"
	"log"
	"time"

	"mbgw/internal/archive"
	"mbgw/internal/health"
	"mbgw/internal/profile"
	"mbgw/internal/storage"
)

// func65StorageHour converts the device record timestamp to the archive
// period marker used by MBGW. Some VZLET archives (confirmed live for
// IVK-TER on 2026-09-10) timestamp an hourly record at HH:59:59, i.e. at
// the END of the hour. Storing Truncate(time.Hour) used to label that record
// one hour too early. The rule is profile-driven so other function-65 devices
// keep their current semantics until verified.
func func65StorageHour(a profile.Archive, ts time.Time) time.Time {
	boundary := time.Date(ts.Year(), ts.Month(), ts.Day(), ts.Hour(), 0, 0, 0, ts.Location())
	semantics, _ := a.Params["timestamp_semantics"].(string)
	if semantics == "period_end" && ts.After(boundary) {
		return boundary.Add(time.Hour)
	}
	return boundary
}

// persistFunc65Hourly сохраняет профильные числовые поля одной записи
// VZLET function 65 в общий archive_hourly. archive_time становится
// меткой часа, а не отдельным измеряемым параметром.
func persistFunc65Hourly(ctx context.Context, repo storage.Repo, deviceID string, a profile.Archive, records []archive.ArchiveRecord) int {
	units := make(map[string]string, len(a.RecordLayout))
	for _, f := range a.RecordLayout {
		units[f.Name] = f.Unit
	}

	saved := 0
	for _, rec := range records {
		if rec.RecordTS.IsZero() {
			log.Printf("[%s] архив %s: запись функции 65 без корректной метки времени пропущена\n", deviceID, a.ID)
			continue
		}
		quality := "VALID"
		if !rec.CRCOK {
			quality = "INVALID"
		}
		for name, raw := range rec.Fields {
			if name == "archive_time" {
				continue
			}
			value, ok := archiveNumber(raw)
			if !ok {
				continue
			}
			if err := repo.SaveHourlyArchive(ctx, storage.HourlyArchiveRecord{
				DeviceID: deviceID,
				Channel:  "",
				Param:    name,
				TsHour:   func65StorageHour(a, rec.RecordTS),
				Value:    value,
				Unit:     units[name],
				Quality:  quality,
			}); err != nil {
				log.Printf("[%s] архив %s: ошибка сохранения поля %s за %s: %v\n",
					deviceID, a.ID, name, rec.RecordTS.Format("02.01.2006 15:04"), err)
				continue
			}
			saved++
		}
	}
	return saved
}

func archiveNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	default:
		return 0, false
	}
}

func func65RepresentativeParam(a profile.Archive) string {
	for _, f := range a.RecordLayout {
		if f.Name == "archive_time" {
			continue
		}
		switch f.Type {
		case "float", "double", "int16", "uint16", "int32", "uint32", "scaled_int", "bitfield":
			return f.Name
		}
	}
	return ""
}

// backfillFunc65Hourly идёт от вершины кольцевого архива вглубь по одному
// индексу. Function 65 возвращает одну запись на запрос, поэтому page-size
// здесь намеренно равен одному.
func (d *Device) backfillFunc65Hourly(ctx context.Context, a profile.Archive, opts BackfillOptions) {
	reader, ok := archive.Get(a.Strategy)
	if !ok {
		log.Printf("[%s] восстановление архива %s: неизвестный способ чтения %s\n", d.ID, a.ID, a.Strategy)
		return
	}
	param := func65RepresentativeParam(a)
	if param == "" {
		log.Printf("[%s] восстановление архива %s: в профиле нет контрольного числового поля\n", d.ID, a.ID)
		return
	}

	depth := a.BufferDepthOrDefault()
	if opts.MaxDepthHours > 0 && opts.MaxDepthHours < depth {
		depth = opts.MaxDepthHours
	}
	if depth <= 0 {
		return
	}
	now := time.Now().Truncate(time.Hour)
	from := now.Add(-time.Duration(depth-1) * time.Hour)
	missingList, err := d.Repo.MissingHours(ctx, d.ID, "", param, from, now)
	if err != nil {
		log.Printf("[%s] восстановление архива %s: не удалось определить отсутствующие часы (%v); проверяю архив прибора до доступного предела\n", d.ID, a.ID, err)
	}
	missing := make(map[int64]bool, len(missingList))
	for _, t := range missingList {
		missing[t.Unix()] = true
	}
	if err == nil && len(missing) == 0 {
		return
	}

	layout := layoutFromProfile(a)
	totalSaved := 0
	log.Printf("[%s] восстановление архива %s ИВК-ТЭР: обнаружено %d отсутствующих часовых периодов; проверяю до %d часов назад\n", d.ID, a.ID, len(missing), depth)
	for index := 0; index < depth; index++ {
		select {
		case <-ctx.Done():
			return
		default:
		}

		release, leaseErr := d.acquireLeaseWithRetry(ctx, a.ID, 30*time.Second)
		if leaseErr != nil {
			log.Printf("[%s] восстановление архива %s: прибор занят другим опросом; не удалось дождаться доступа: %v\n", d.ID, a.ID, leaseErr)
			return
		}
		records, readErr := reader.Read(ctx, sessionAdapter{d.Sess}, d.Client, archive.ArchiveQuery{
			DeviceID: d.ID, ArchiveID: a.ID, Instance: 1,
			FromIndex: index, ToIndex: index,
			RecordLayout: layout, WordOrder32: d.Profile.Codec.WordOrder32, WordOrder64: d.Profile.Codec.WordOrder64,
			Params: a.Params,
		})
		release()
		if readErr != nil {
			log.Printf("[%s] восстановление архива %s: ошибка чтения позиции %d в архиве прибора: %v\n", d.ID, a.ID, index, readErr)
			return
		}
		if len(records) == 0 {
			log.Printf("[%s] восстановление архива %s: достигнут конец доступного архива прибора (позиция %d)\n", d.ID, a.ID, index)
			break
		}
		saved := persistFunc65Hourly(ctx, d.Repo, d.ID, a, records)
		totalSaved += saved
		health.MarkPollProgress(d.ID, time.Now())
		for _, rec := range records {
			if !rec.RecordTS.IsZero() {
				delete(missing, func65StorageHour(a, rec.RecordTS).Unix())
			}
		}
		if err == nil && len(missing) == 0 {
			break
		}
	}
	log.Printf("[%s] восстановление архива %s ИВК-ТЭР завершено: сохранено показателей %d\n", d.ID, a.ID, totalSaved)
}

func func65LatestQuery(d *Device, a profile.Archive) archive.ArchiveQuery {
	return archive.ArchiveQuery{
		DeviceID: d.ID, ArchiveID: a.ID, Instance: 1,
		FromIndex: 0, ToIndex: 0,
		RecordLayout: layoutFromProfile(a), WordOrder32: d.Profile.Codec.WordOrder32, WordOrder64: d.Profile.Codec.WordOrder64,
		Params: a.Params,
	}
}

func validateFunc65Record(rec archive.ArchiveRecord) error {
	if rec.RecordTS.IsZero() {
		return fmt.Errorf("в записи нет archive_time")
	}
	return nil
}

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
	if semantics == "period_end" {
		// Live IVK-TER verification showed that the raw archive timestamp
		// can arrive exactly on HH:00:00 while the vendor UI presents the
		// same record as HH:59:59. In both forms the row belongs to the
		// hourly period that closes at the NEXT hour boundary. Therefore an
		// exact boundary must be shifted too; checking ts.After(boundary)
		// left real IVK-TER rows one hour early.
		return boundary.Add(time.Hour)
	}
	return boundary
}

// func65WallClockHour normalizes only the calendar components of a time value.
// This is deliberate for IVK-TER: archive_time is decoded by the generic
// profile codec into a UTC-located time.Time, while the device field itself
// represents local wall-clock calendar fields. Absolute-instant comparisons
// would therefore introduce the server timezone offset.
func func65WallClockHour(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, time.UTC)
}

// func65QueryTimeForStorageHour converts the hour shown/stored by MBGW into
// the raw archive period requested from the VZLET device. For period_end
// archives (live-confirmed IVK-TER), MBGW storage 15:00 corresponds to the
// device archive record belonging to 14:00..15:00, so function 65 must be
// asked for raw 14:00.
func func65QueryTimeForStorageHour(a profile.Archive, storageHour time.Time) time.Time {
	queryHour := func65WallClockHour(storageHour)
	semantics, _ := a.Params["timestamp_semantics"].(string)
	if semantics == "period_end" {
		return queryHour.Add(-time.Hour)
	}
	return queryHour
}

// func65ReloadQuery always uses function-65 TIME access for operator-forced
// range rereads. Index access remains appropriate for startup backfill, but
// an operator-selected calendar interval must not depend on ring-buffer
// position/order.
func func65ReloadQuery(d *Device, a profile.Archive, storageHour time.Time) archive.ArchiveQuery {
	return archive.ArchiveQuery{
		DeviceID: d.ID, ArchiveID: a.ID, Instance: 1,
		From:         func65QueryTimeForStorageHour(a, storageHour),
		RecordLayout: layoutFromProfile(a), WordOrder32: d.Profile.Codec.WordOrder32, WordOrder64: d.Profile.Codec.WordOrder64,
		Params: a.Params,
	}
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

// ForceReloadFunc65Hourly повторно читает часовой архив VZLET function 65
// за выбранный оператором диапазон и делает обычный upsert в archive_hourly.
// В отличие от startup-backfill здесь цель — не только заполнить пропуски,
// а ПЕРЕПРОЧИТАТЬ уже существующие часы и заменить их свежими значениями.
//
// Ручной диапазон читается штатным TIME-доступом function 65, а не
// сканированием кольцевого буфера по индексам. Это соответствует смыслу UI
// "с/по", не зависит от положения вершины кольца и не смешивает UTC location
// generic-декодера с локальными календарными полями ИВК-ТЭР.
//
// Диапазон заранее НЕ удаляется: каждый успешно прочитанный час сразу
// перезаписывается UPSERT-ом. При ошибке связи уже обновлённые часы остаются,
// а ещё не прочитанные старые строки не уничтожаются.
func (d *Device) ForceReloadFunc65Hourly(ctx context.Context, from, to time.Time, onProgress func(done, total int)) (int, error) {
	if d == nil || d.Profile == nil {
		return 0, fmt.Errorf("профиль прибора не загружен")
	}
	if to.IsZero() {
		to = time.Now()
	}

	// Для ИВК-ТЭР сравниваем календарные часы, а не абсолютные instants:
	// generic archive decoder ставит Location=UTC, хотя само поле прибора
	// представляет локальный wall-clock.
	fromHour := func65WallClockHour(from)
	toHour := func65WallClockHour(to)
	if toHour.Before(fromHour) {
		return 0, fmt.Errorf("верхняя граница переопроса %s раньше нижней %s",
			toHour.Format("02.01.2006 15:04"), fromHour.Format("02.01.2006 15:04"))
	}

	var target *profile.Archive
	for i := range d.Profile.Archives {
		if d.Profile.Archives[i].Strategy == "mb_func65" {
			target = &d.Profile.Archives[i]
			break
		}
	}
	if target == nil {
		return 0, fmt.Errorf("в профиле прибора нет часового архива function 65")
	}
	a := *target

	reader, ok := archive.Get(a.Strategy)
	if !ok {
		return 0, fmt.Errorf("неизвестный способ чтения архива %q", a.Strategy)
	}

	total := int(toHour.Sub(fromHour)/time.Hour) + 1
	if total <= 0 {
		return 0, fmt.Errorf("пустой диапазон переопроса")
	}

	periodsSaved := 0
	valuesSaved := 0
	missingPeriods := 0

	log.Printf("[%s] принудительный переопрос архива %s ИВК-ТЭР: диапазон %s..%s, периодов %d\n",
		d.ID, a.ID, fromHour.Format("02.01.2006 15:04"), toHour.Format("02.01.2006 15:04"), total)

	done := 0
	for wantedHour := fromHour; !wantedHour.After(toHour); wantedHour = wantedHour.Add(time.Hour) {
		select {
		case <-ctx.Done():
			return periodsSaved, ctx.Err()
		default:
		}

		query := func65ReloadQuery(d, a, wantedHour)
		release, leaseErr := d.acquireLeaseWithRetry(ctx, a.ID, 30*time.Second)
		if leaseErr != nil {
			return periodsSaved, fmt.Errorf("прибор занят другим опросом: %w", leaseErr)
		}
		records, readErr := reader.Read(ctx, sessionAdapter{d.Sess}, d.Client, query)
		release()

		done++
		if onProgress != nil {
			onProgress(done, total)
		}
		if readErr != nil {
			return periodsSaved, fmt.Errorf("ошибка чтения часа %s архива %s: %w",
				wantedHour.Format("02.01.2006 15:04"), a.ID, readErr)
		}
		if len(records) == 0 {
			missingPeriods++
			log.Printf("[%s] принудительный переопрос ИВК-ТЭР: в приборе нет записи за час %s\n",
				d.ID, wantedHour.Format("02.01.2006 15:04"))
			continue
		}

		matched := false
		for _, rec := range records {
			if rec.RecordTS.IsZero() {
				continue
			}

			gotHour := func65WallClockHour(func65StorageHour(a, rec.RecordTS))
			if !gotHour.Equal(wantedHour) {
				// TIME-запрос должен вернуть именно запрошенный период.
				// Соседнюю запись не сохраняем: при forced reload безопаснее
				// остановиться и показать расхождение, чем переписать не тот час.
				return periodsSaved, fmt.Errorf(
					"при запросе часа %s прибор вернул запись за %s; запись не сохранена",
					wantedHour.Format("02.01.2006 15:04"),
					gotHour.Format("02.01.2006 15:04"),
				)
			}

			matched = true
			saved := persistFunc65Hourly(ctx, d.Repo, d.ID, a, []archive.ArchiveRecord{rec})
			if saved > 0 {
				periodsSaved++
				valuesSaved += saved
				health.MarkPollProgress(d.ID, time.Now())
				log.Printf("[%s] принудительный переопрос ИВК-ТЭР: час %s обновлён, сохранено показателей %d\n",
					d.ID, wantedHour.Format("02.01.2006 15:04"), saved)
			}
		}
		if !matched {
			missingPeriods++
		}
	}

	if periodsSaved == 0 {
		return 0, fmt.Errorf("в доступном архиве прибора не найдено записей за диапазон %s..%s",
			fromHour.Format("02.01.2006 15:04"), toHour.Format("02.01.2006 15:04"))
	}

	log.Printf("[%s] принудительный переопрос ИВК-ТЭР завершён: обновлено часовых периодов %d, сохранено показателей %d, без записи в приборе %d\n",
		d.ID, periodsSaved, valuesSaved, missingPeriods)
	return periodsSaved, nil
}

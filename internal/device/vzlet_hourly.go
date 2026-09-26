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

// backfillFunc65Hourly восстанавливает отсутствующие часы ИВК-ТЭР через
// function-65 TIME-доступ. Live-проверка показала, что индекс 0 не является
// вершиной/самой свежей записью архива, поэтому индексный проход здесь давал
// ложное ощущение успешного дозабора и мог многократно перечитывать старые
// периоды. Для каждого реально отсутствующего storage-hour теперь выполняется
// точный календарный запрос тем же способом, что и при принудительном
// переопросе диапазона.
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

	// ИВК-ТЭР кодирует локальные календарные поля, но generic decoder
	// помещает их в UTC location. Поэтому и границы поиска пропусков держим
	// в том же wall-clock представлении, без абсолютного timezone-сдвига.
	now := func65WallClockHour(time.Now())
	from := now.Add(-time.Duration(depth-1) * time.Hour)
	missingList, err := d.Repo.MissingHours(ctx, d.ID, "", param, from, now)
	if err != nil {
		log.Printf("[%s] восстановление архива %s: не удалось определить отсутствующие часы (%v); проверяю весь доступный диапазон\n", d.ID, a.ID, err)
		missingList = missingList[:0]
		for h := from; !h.After(now); h = h.Add(time.Hour) {
			missingList = append(missingList, h)
		}
	}
	if opts.SkipLatest {
		filtered := missingList[:0]
		for _, h := range missingList {
			if func65WallClockHour(h).Equal(now) {
				continue
			}
			filtered = append(filtered, h)
		}
		missingList = filtered
	}
	if len(missingList) == 0 {
		return
	}

	totalSaved := 0
	periodsSaved := 0
	log.Printf("[%s] восстановление архива %s ИВК-ТЭР: отсутствует %d часовых периодов; запрашиваю их по времени\n", d.ID, a.ID, len(missingList))

	// Сначала самые свежие пропуски: если связь оборвётся на глубине,
	// актуальные часы успеют восстановиться первыми.
	for i := len(missingList) - 1; i >= 0; i-- {
		select {
		case <-ctx.Done():
			return
		default:
		}

		wantedHour := func65WallClockHour(missingList[i])
		query := func65ReloadQuery(d, a, wantedHour)

		release, leaseErr := d.acquireLeaseWithRetry(ctx, a.ID, 30*time.Second)
		if leaseErr != nil {
			log.Printf("[%s] восстановление архива %s: прибор занят другим опросом; не удалось дождаться доступа: %v\n", d.ID, a.ID, leaseErr)
			return
		}
		records, readErr := reader.Read(ctx, sessionAdapter{d.Sess}, d.Client, query)
		release()
		if readErr != nil {
			log.Printf("[%s] восстановление архива %s: ошибка чтения часа %s: %v\n", d.ID, a.ID, wantedHour.Format("02.01.2006 15:04"), readErr)
			return
		}

		matched := false
		for _, rec := range records {
			if err := validateFunc65Record(rec); err != nil {
				log.Printf("[%s] восстановление архива %s: некорректная запись за час %s: %v\n", d.ID, a.ID, wantedHour.Format("02.01.2006 15:04"), err)
				continue
			}
			gotHour := func65WallClockHour(func65StorageHour(a, rec.RecordTS))
			if !gotHour.Equal(wantedHour) {
				log.Printf("[%s] восстановление архива %s: при запросе часа %s прибор вернул %s; запись не сохранена\n",
					d.ID, a.ID, wantedHour.Format("02.01.2006 15:04"), gotHour.Format("02.01.2006 15:04"))
				continue
			}

			saved := persistFunc65Hourly(ctx, d.Repo, d.ID, a, []archive.ArchiveRecord{rec})
			if saved > 0 {
				totalSaved += saved
				periodsSaved++
				matched = true
			}
		}
		if !matched && len(records) == 0 {
			log.Printf("[%s] восстановление архива %s: в приборе нет записи за час %s\n", d.ID, a.ID, wantedHour.Format("02.01.2006 15:04"))
		}
		health.MarkPollProgress(d.ID, time.Now())
	}

	log.Printf("[%s] восстановление архива %s ИВК-ТЭР завершено: обновлено периодов %d, сохранено показателей %d\n", d.ID, a.ID, periodsSaved, totalSaved)
}

// pollFunc65Latest performs one real TIME-based read of the newest completed
// IVK-TЭР storage hour on every scheduled tick. This keeps poll status tied to
// live device communication even when startup backfill has already filled all
// gaps in SQLite.
func (d *Device) pollFunc65Latest(ctx context.Context, a profile.Archive) {
	reader, ok := archive.Get(a.Strategy)
	if !ok {
		log.Printf("[%s] архив %s: неизвестный способ чтения %s\n", d.ID, a.ID, a.Strategy)
		return
	}
	now := time.Now()
	wantedHour := func65WallClockHour(now)
	q := func65LatestQueryAt(d, a, now)
	release, err := d.acquireLeaseWithRetry(ctx, a.ID, 30*time.Second)
	if err != nil {
		log.Printf("[%s] архив %s: не удалось получить доступ для чтения последнего часа: %v\n", d.ID, a.ID, err)
		return
	}
	records, readErr := reader.Read(ctx, sessionAdapter{d.Sess}, d.Client, q)
	release()
	health.MarkPollProgress(d.ID, time.Now())
	if readErr != nil {
		log.Printf("[%s] архив %s: ошибка чтения последнего часа %s: %v\n", d.ID, a.ID, wantedHour.Format("02.01.2006 15:04"), readErr)
		return
	}
	if len(records) == 0 {
		// A successful protocol response with no row still proves the device is
		// reachable; the archive may simply not have closed this hour yet.
		health.MarkArchiveSuccess(d.ID, time.Now())
		log.Printf("[%s] архив %s: прибор ответил, запись за последний час %s ещё не сформирована\n", d.ID, a.ID, wantedHour.Format("02.01.2006 15:04"))
		return
	}

	matched := false
	for _, rec := range records {
		if err := validateFunc65Record(rec); err != nil {
			continue
		}
		gotHour := func65WallClockHour(func65StorageHour(a, rec.RecordTS))
		if !gotHour.Equal(wantedHour) {
			continue
		}
		if persistFunc65Hourly(ctx, d.Repo, d.ID, a, []archive.ArchiveRecord{rec}) > 0 {
			matched = true
		}
	}
	if matched {
		health.MarkArchiveSuccess(d.ID, time.Now())
	} else {
		log.Printf("[%s] архив %s: прибор ответил, но не вернул ожидаемую запись за %s\n", d.ID, a.ID, wantedHour.Format("02.01.2006 15:04"))
	}
}

// func65LatestQueryAt строит запрос для последнего завершившегося часового
// периода на момент now. ВАЖНО: это TIME-доступ, а не индекс 0. Для
// timestamp_semantics=period_end storage-hour 10:00 запрашивается как raw
// device-hour 09:00 и после декодирования снова сохраняется с меткой 10:00.
func func65LatestQueryAt(d *Device, a profile.Archive, now time.Time) archive.ArchiveQuery {
	return func65ReloadQuery(d, a, func65WallClockHour(now))
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

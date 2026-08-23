package device

import (
	"context"
	"fmt"
	"time"

	"mbgw/internal/archive"
	"mbgw/internal/profile"
)

// ForceReloadAkronHourly принудительно перечитывает часовой архив Akron
// у САМОГО ПРИБОРА, начиная с указанного момента времени, и ПЕРЕЗАПИСЫВАЕТ
// уже сохранённые записи в нашей базе — в отличие от обычного дозабора
// (BackfillArchives), который трогает только ОТСУТСТВУЮЩИЕ часы и молча
// пропускает часы, где строка УЖЕ ЕСТЬ, даже если её значение ошибочное.
//
// Зачем это нужно (реальный случай, 2026-08-23): помеха на линии RS-485
// дала испорченное показание для одного часа; оно прошло проверку даты
// (akronRowTime), но не проверку правдоподобия значения (добавлена в
// persistAkronHourly в тот же день) — успело уйти в Энергосферу ДО того,
// как эта проверка была добавлена в код. Обычный дозабор такую ошибку
// НЕ исправит (строка для этого часа уже существует, значит для него
// "нечего добирать"). Единственный способ починить уже испорченную
// историю — заставить систему заново спросить прибор за нужный период и
// перезаписать то, что там сейчас лежит.
//
// fromTime — с какого момента начинать (обычно "начало сегодняшних
// суток" или конкретный час, который нужно перечитать). Метод сам
// считает, на сколько часов назад это от текущего момента, и переводит
// в индексы, которые понимает протокол Akron (i=1 — самая свежая
// запись, дальше вглубь).
func (d *Device) ForceReloadAkronHourly(ctx context.Context, fromTime time.Time) (int, error) {
	var a profile.Archive
	found := false
	for _, cand := range d.Profile.Archives {
		if cand.Strategy != "akron_archive" {
			continue
		}
		kind, _ := cand.Params["archive_kind"].(string)
		if kind != "hourly" {
			continue
		}
		a = cand
		found = true
		break
	}
	if !found {
		return 0, fmt.Errorf("у прибора %s нет часового архива Akron в профиле", d.ID)
	}

	reader, ok := archive.Get(a.Strategy)
	if !ok {
		return 0, fmt.Errorf("неизвестная стратегия архива %s", a.Strategy)
	}
	layout := layoutFromProfile(a)
	pageSize := a.MaxRowsOrDefault()
	bufferDepth := a.BufferDepthOrDefault()

	// Сколько часов назад от текущего момента находится fromTime —
	// столько записей (вглубь от вершины индекса) и нужно перечитать.
	depthHours := int(time.Since(fromTime).Hours()) + 1
	if depthHours < 1 {
		depthHours = 1
	}
	if depthHours > bufferDepth {
		depthHours = bufferDepth // не глубже физического буфера прибора
	}

	totalSaved := 0
	for from := 0; from < depthHours; from += pageSize {
		to := from + pageSize - 1
		if to >= depthHours {
			to = depthHours - 1
		}

		release, leaseErr := d.Lease.Acquire(ctx, d.ID, a.ID, 30*time.Second)
		if leaseErr != nil {
			return totalSaved, fmt.Errorf("не удалось занять доступ к линии связи: %w", leaseErr)
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
			return totalSaved, fmt.Errorf("страница i=%d..%d: ошибка чтения у прибора: %w", from+1, to+1, err)
		}
		if len(records) == 0 {
			break // архив прибора закончился раньше запрошенной глубины
		}

		// persistAkronHourly сам перезапишет существующие строки
		// (SaveHourlyArchive — upsert) и заодно применит проверку
		// правдоподобия значения — если на линии СЕЙЧАС тоже помеха,
		// заведомо плохое новое чтение не заменит собой хорошее старое.
		totalSaved += persistAkronHourly(ctx, d.Repo, d.ID, a, records)
	}

	return totalSaved, nil
}

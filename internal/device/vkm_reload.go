package device

import (
	"context"
	"fmt"
	"time"

	"mbgw/internal/profile"
)

// ForceReloadVKMHourly принудительно перечитывает архив ВКМ у самого
// прибора за период [from, to] и ПЕРЕЗАПИСЫВАЕТ уже сохранённые записи —
// аналог ForceReloadAkronHourly (см. akron_reload.go, там же подробное
// объяснение, зачем вообще нужна такая функция), но для ВКМ она проще:
// архив ВКМ адресуется НАПРЯМУЮ по времени (собственные поля From/To
// в запросе, см. collectVKMPeriod), поэтому не нужно, в отличие от
// Akron, пересчитывать "сколько часов назад" в индекс — достаточно
// просто перебрать получасовые периоды в указанном диапазоне и заново
// запросить каждый.
//
// collectVKMPeriod уже сохраняет результат через upsert (перезаписывает
// существующую строку) — обычный дозабор (backfillVKMHourly) просто
// заранее ОТФИЛЬТРОВЫВАЕТ периоды, для которых строка уже есть, эта же
// функция намеренно идёт по ВСЕМ периодам диапазона без такого фильтра.
//
// onProgress, если не nil, вызывается после каждого успешно обработанного
// запроса (трубопровод, получасовой период): done — сколько запросов уже
// обработано, total — сколько их всего по всем активным трубопроводам.
// Используется веб-интерфейсом для реального прогресса длительного переопроса.
func (d *Device) ForceReloadVKMHourly(ctx context.Context, from, to time.Time, onProgress func(done, total int)) (int, error) {
	var a profile.Archive
	found := false
	for _, cand := range d.Profile.Archives {
		if cand.Strategy != "mb_request_poll_string" {
			continue
		}
		a = cand
		found = true
		break
	}
	if !found {
		return 0, fmt.Errorf("у прибора %s нет архива ВКМ в профиле", d.ID)
	}

	lastCompletedPeriodStart := time.Now().Truncate(vkmArchivePeriod).Add(-vkmArchivePeriod)
	if to.After(lastCompletedPeriodStart) {
		to = lastCompletedPeriodStart
	}

	periodStart := from.Truncate(vkmArchivePeriod)
	periods := int(to.Sub(periodStart)/vkmArchivePeriod) + 1
	if periods < 0 {
		periods = 0
	}
	pipes := d.vkmActivePipes(ctx)
	totalRequests := periods * len(pipes)

	totalSaved := 0
	doneCount := 0
	for !periodStart.After(to) {
		for _, pipe := range pipes {
			select {
			case <-ctx.Done():
				return totalSaved, ctx.Err()
			default:
			}

			saved, err := d.collectVKMPeriod(ctx, a, pipe, periodStart)
			if err != nil {
				return totalSaved, fmt.Errorf("трубопровод %d, период %s: ошибка чтения у прибора: %w", pipe, periodStart.Format("02.01.2006 15:04"), err)
			}
			totalSaved += saved
			doneCount++
			if onProgress != nil {
				onProgress(doneCount, totalRequests)
			}
		}
		periodStart = periodStart.Add(vkmArchivePeriod)
	}

	return totalSaved, nil
}

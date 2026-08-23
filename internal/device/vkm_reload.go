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
func (d *Device) ForceReloadVKMHourly(ctx context.Context, from, to time.Time) (int, error) {
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

	periodStart := from.Truncate(vkmArchivePeriod)
	totalSaved := 0
	for !periodStart.After(to) {
		select {
		case <-ctx.Done():
			return totalSaved, ctx.Err()
		default:
		}

		saved, err := d.collectVKMPeriod(ctx, a, periodStart)
		if err != nil {
			return totalSaved, fmt.Errorf("период %s: ошибка чтения у прибора: %w", periodStart.Format("02.01.2006 15:04"), err)
		}
		totalSaved += saved
		periodStart = periodStart.Add(vkmArchivePeriod)
	}

	return totalSaved, nil
}

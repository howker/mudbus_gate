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
// onProgress, если не nil, вызывается после каждого обработанного периода
// (done — сколько уже сделано, total — сколько всего) — используется
// веб-интерфейсом, чтобы показывать реальный прогресс длительного
// переопроса вместо "тишины" на много минут (добавлено 2026-08-27).
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

	// Удаляем всё, что уже есть в базе за диапазон, ПЕРЕД повторным
	// сбором — иначе, если старые строки размечены другим соглашением
	// (например, старым "начало периода" вместо нынешнего "конец
	// периода" — см. подробности в vkm_hourly.go, фикс 2026-08-27), их
	// метки не совпадут с новыми, и они останутся висеть в базе рядом со
	// свежими, никем не замеченные. Берём диапазон с запасом в один
	// период в обе стороны — гарантированно захватывает и старую, и
	// новую разметку границ. Если repo не поддерживает удаление (узкая
	// заглушка в тестах) — переопрос честно завершается ошибкой, а не
	// тихо продолжает без очистки: молчаливо оставить дубликаты хуже,
	// чем явно сообщить, что не смогли почистить.
	// ВАЖНО: диапазон заранее НЕ удаляем. Каждая успешно перечитанная запись
	// заменяет существующую через SaveHourlyArchive (UPSERT). Если связь оборвётся,
	// старые строки ещё не прочитанных периодов останутся в БД.
	lastCompletedPeriodStart := time.Now().Truncate(vkmArchivePeriod).Add(-vkmArchivePeriod)
	if to.After(lastCompletedPeriodStart) {
		to = lastCompletedPeriodStart
	}

	periodStart := from.Truncate(vkmArchivePeriod)
	totalPeriods := int(to.Sub(periodStart)/vkmArchivePeriod) + 1
	if totalPeriods < 0 {
		totalPeriods = 0
	}
	totalSaved := 0
	doneCount := 0
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
		doneCount++
		if onProgress != nil {
			onProgress(doneCount, totalPeriods)
		}
		periodStart = periodStart.Add(vkmArchivePeriod)
	}

	return totalSaved, nil
}

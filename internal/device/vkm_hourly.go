package device

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"mbgw/internal/archive"
	"mbgw/internal/dbg"
	"mbgw/internal/profile"
	"mbgw/internal/storage"
)

// vkmArchivePeriod — длина одного периода сбора архива ВКМ. Подтверждено
// живым захватом (2026-08-02, vkm_live.jsonl): реальный драйвер ЭС (УВП-280)
// запрашивает архив ИМЕННО получасовыми окнами (например 19:00:00-19:30:00),
// а не часовыми — этот параметр в самой ЭС не настраивается, так жёстко
// сделан драйвер. Раньше здесь был час (по аналогии с Акроном) — из-за
// этого ЭС получала строку с Time=...19:00-20:00, не совпадающую с тем,
// что сама запросила, и просто бесконечно повторяла один и тот же запрос
// с новым id, не продвигаясь дальше. Все периоды/границы в этом файле
// теперь считаются кратными этому значению, а не часу.
const vkmArchivePeriod = 30 * time.Minute

// vkmDefaultBackfillDepthHours — насколько глубоко назад стартовый/ручной
// дозабор ВКМ идёт по умолчанию (opts.MaxDepthHours переопределяет).
// Значение в ЧАСАХ (для совместимости с конфигом — то же поле, что и у
// Акрона), внутри переводится в количество получасовых периодов
// (глубина_в_часах * 2). Каждый период — это отдельный полный запрос
// (запись/ожидание/чтение), поэтому 648ч, как у Акрона, здесь неуместны:
// 24 часа = 48 периодов, уже несколько минут работы при старте.
const vkmDefaultBackfillDepthHours = 24

// vkmHourlyParams — поля, которые сохраняются в archive_hourly из одного
// результата архива ВКМ — интегральные за период суммы, а не мгновенные
// значения. Подтверждено на реальном приборе: S (масса) и ST (тепловая
// энергия) растут пропорционально длине запрошенного периода; Pi/Pbar/T/
// dP/H — мгновенные показания на конец периода и сюда сознательно не
// попадают.
var vkmHourlyParams = []string{"S", "ST"}

// persistVKMHourly сохраняет vkmHourlyParams из одного результата архива
// ВКМ как строки archive_hourly, привязанные к periodStart (начало периода,
// который реально покрывало окно From/To запроса — не time.Now(), так как
// дозабираемый период всегда в прошлом).
func persistVKMHourly(ctx context.Context, d *Device, periodStart time.Time, rec archive.ArchiveRecord) int {
	saved := 0
	for _, param := range vkmHourlyParams {
		v, ok := fieldFloat(rec.Fields, param)
		if !ok {
			log.Printf("[%s] VKM период %s: поле %s отсутствует в ответе прибора (поля=%v)\n",
				d.ID, periodStart.Format("02.01.2006 15:04"), param, rec.Fields)
			continue
		}
		unit, _ := rec.Fields[param+"_unit"].(string)

		r := storage.HourlyArchiveRecord{
			DeviceID: d.ID,
			Channel:  "",
			Param:    param,
			TsHour:   periodStart,
			Value:    v,
			Unit:     unit,
		}
		if err := d.Repo.SaveHourlyArchive(ctx, r); err != nil {
			log.Printf("[%s] VKM период %s: ошибка сохранения %s: %v\n",
				d.ID, periodStart.Format("02.01.2006 15:04"), param, err)
			continue
		}
		saved++
	}
	return saved
}

// isVKMTimeAnomalous — ИСТОРИЧЕСКАЯ функция, была источником главной
// ошибки дня (2026-08-10): считала "секундный" формат Time
// ("839089620-839089800сек") браком и заставляла collectVKMPeriod
// переспрашивать период, пока прибор не даст "датный" формат
// ("DD/MM/YY..."). Прозрачный сетевой прокси-эксперимент между ЭС и
// реальным прибором доказал обратное: именно "секундный" формат (вместе с
// полной точностью чисел, которую он несёт) ЭС принимает как достоверное
// значение (State=0); "датный" формат идёт с урезанной точностью и
// экспоненциальной записью и ЭС его бракует (State=1). Другими словами —
// не брак прибора, а два ЗАКОННЫХ формата ответа, и мы весь день
// систематически отбрасывали тот, который реально нужен, добиваясь
// переспросом того, который не работает. Оставлена только как
// исторический маркер; больше нигде не вызывается.
func isVKMTimeAnomalous(raw string) bool {
	idx := strings.Index(raw, "Time=")
	if idx < 0 {
		return false // нет поля Time вообще — не наш случай, не трогаем
	}
	rest := raw[idx+len("Time="):]
	value := rest
	if semi := strings.Index(rest, ";"); semi >= 0 {
		value = rest[:semi]
	}
	return !strings.Contains(value, "/")
}

// collectVKMPeriod делает ОДИН полный танец запись/ожидание/чтение архива
// для окна [periodStart, periodStart+vkmArchivePeriod) и сохраняет то, что
// пришло. Возвращает, сколько из vkmHourlyParams реально сохранено (0 без
// ошибки — законный исход: у прибора не было данных за этот период).
//
// ВАЖНО (2026-08-10): раньше здесь был цикл переспроса при "аномальном"
// (секундном) формате Time — см. doc-комментарий isVKMTimeAnomalous. Он
// убран: секундный формат — не брак, а именно то, что нужно сохранить
// как есть, с первой же попытки, без всякого переспроса.
func (d *Device) collectVKMPeriod(ctx context.Context, a profile.Archive, periodStart time.Time) (int, error) {
	reader, ok := archive.Get(a.Strategy)
	if !ok {
		return 0, fmt.Errorf("неизвестная стратегия %s", a.Strategy)
	}

	q := archive.ArchiveQuery{
		DeviceID:  d.ID,
		ArchiveID: a.ID,
		Instance:  1,
		From:      periodStart,
		To:        periodStart.Add(vkmArchivePeriod),
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

	// Сырую строку сохраняем отдельно от разобранных полей — она нужна
	// northbound-у, чтобы отдавать в ЭС ровно то, что прислал прибор (и с
	// тем же временным диапазоном, что реально запросили — иначе ЭС видит
	// несовпадение периода в ответе и не продвигается дальше, это и было
	// найдено живьём 2026-08-02). Ошибка сохранения сырой строки не должна
	// ронять сохранение S/ST — это две независимые вещи.
	//
	// records[0].Raw сохраняется здесь БУКВАЛЬНО как пришло от прибора
	// (parseTaggedString ничего в нём не меняет, кроме обрезки нулевых
	// байт) — northbound должен отдавать его в ЭС так же нетронуто, без
	// собственных текстовых преобразований (strip_headers/expand_exponent/
	// field_scale и т.п. — см. историю в vkm_config.go).
	if err := d.Repo.SaveVKMRawString(ctx, d.ID, q.Instance, periodStart, string(records[0].Raw)); err != nil {
		log.Printf("[%s] VKM период %s: ошибка сохранения сырой строки: %v\n",
			d.ID, periodStart.Format("02.01.2006 15:04"), err)
	}

	return persistVKMHourly(ctx, d, periodStart, records[0]), nil
}

// pollVKMHourlyLatest — обычный плановый опрос архива ВКМ (аналог часового
// тика PollArchives у Акрона). Запрашивает последний ПОЛНОСТЬЮ завершённый
// получасовой период.
func (d *Device) pollVKMHourlyLatest(ctx context.Context, a profile.Archive) {
	periodStart := time.Now().Truncate(vkmArchivePeriod).Add(-vkmArchivePeriod)

	saved, err := d.collectVKMPeriod(ctx, a, periodStart)
	if err != nil {
		log.Printf("[%s] VKM архив %s: период %s: ошибка: %v\n",
			d.ID, a.ID, periodStart.Format("02.01.2006 15:04"), err)
		return
	}
	log.Printf("[%s] VKM архив %s: период %s: сохранено полей: %d/%d\n",
		d.ID, a.ID, periodStart.Format("02.01.2006 15:04"), saved, len(vkmHourlyParams))
}

// missingVKMPeriods находит получасовые периоды в [from, to], для которых
// ещё нет строки archive_hourly с param="S" — собственный расчёт (не через
// storage.Repo.MissingHours, который жёстко считает по часу) специально
// под получасовой шаг ВКМ.
func missingVKMPeriods(ctx context.Context, repo storage.Repo, deviceID string, from, to time.Time) ([]time.Time, error) {
	// Разумный запас по лимиту — покрывает несколько лет получасовых
	// записей; для наших объёмов (месяцы работы одного прибора) с большим
	// запасом достаточно.
	existing, err := repo.GetHourlyArchiveDesc(ctx, deviceID, "", "S", 0, 200000)
	if err != nil {
		return nil, fmt.Errorf("чтение существующих записей: %w", err)
	}
	present := make(map[int64]bool, len(existing))
	for _, r := range existing {
		present[r.TsHour.Unix()] = true
	}

	var missing []time.Time
	for t := from; !t.After(to); t = t.Add(vkmArchivePeriod) {
		if !present[t.Unix()] {
			missing = append(missing, t)
		}
	}
	return missing, nil
}

// backfillVKMHourly — аналог глубокого стартового дозабора Акрона, но один
// ЗАПРОС на каждый пропущенный получасовой период (не одно дешёвое
// индексное чтение на много часов разом), поэтому глубина по умолчанию
// заметно меньше акроновской (vkmDefaultBackfillDepthHours), и каждый
// период стоит настоящего многосекундного обращения к прибору. Достаёт
// только полностью завершённые периоды (никогда текущий, ещё не закрытый).
func (d *Device) backfillVKMHourly(ctx context.Context, a profile.Archive, opts BackfillOptions) {
	depthHours := vkmDefaultBackfillDepthHours
	if opts.MaxDepthHours > 0 {
		depthHours = opts.MaxDepthHours
	}
	periodsCount := depthHours * 2 // 2 получасовых периода на час

	toBoundary := time.Now().Truncate(vkmArchivePeriod).Add(-vkmArchivePeriod) // последний завершённый период
	fromBoundary := toBoundary.Add(-time.Duration(periodsCount-1) * vkmArchivePeriod)

	missing, err := missingVKMPeriods(ctx, d.Repo, d.ID, fromBoundary, toBoundary)
	if err != nil {
		log.Printf("[%s] VKM дозабор %s: не удалось вычислить пропуски: %v\n", d.ID, a.ID, err)
		return
	}
	if len(missing) == 0 {
		// Тихо — это штатный, часто повторяющийся исход ("нечего
		// добирать"), а не событие, интересное при обычной работе.
		// Полная детализация доступна через dbg (отладочный лог).
		dbg.Printf("[%s] VKM дозабор %s: пропусков в пределах %dч (%d периодов по 30 мин) нет\n",
			d.ID, a.ID, depthHours, periodsCount)
		return
	}

	log.Printf("[%s] VKM дозабор %s: старт, пропущено периодов: %d из %d\n",
		d.ID, a.ID, len(missing), periodsCount)

	periodsFilled := 0
	for _, period := range missing {
		select {
		case <-ctx.Done():
			log.Printf("[%s] VKM дозабор %s: прервано контекстом (заполнено периодов: %d/%d)\n",
				d.ID, a.ID, periodsFilled, len(missing))
			return
		default:
		}

		saved, err := d.collectVKMPeriod(ctx, a, period)
		if err != nil {
			log.Printf("[%s] VKM дозабор %s: период %s: ошибка: %v\n",
				d.ID, a.ID, period.Format("02.01.2006 15:04"), err)
			continue
		}
		if saved > 0 {
			periodsFilled++
		}
		// Построчный прогресс дозабора (до полусотни строк за один запуск)
		// — это диагностическая детализация, не нужна при обычной работе,
		// только итоговая сводка ниже. Полный построчный вывод доступен
		// через отладочный лог (вкладка «Настройки» в /admin).
		dbg.Printf("[%s] VKM дозабор %s: период %s: сохранено полей: %d/%d\n",
			d.ID, a.ID, period.Format("02.01.2006 15:04"), saved, len(vkmHourlyParams))
	}
	log.Printf("[%s] VKM дозабор %s: готово, заполнено периодов: %d/%d\n", d.ID, a.ID, periodsFilled, len(missing))
}

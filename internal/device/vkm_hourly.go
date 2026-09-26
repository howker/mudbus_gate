package device

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"mbgw/internal/archive"
	"mbgw/internal/dbg"
	"mbgw/internal/errs"
	"mbgw/internal/health"
	"mbgw/internal/profile"
	"mbgw/internal/storage"
)

// vkmArchivePeriod — длина одного периода сбора архива ВКМ. Подтверждено
// живым захватом (2026-08-02, vkm_live.jsonl): реальный драйвер ЭС (УВП-280)
// Р·Р°РїСЂР°С€РёРІР°РµС‚ Р°СЂС…РёРІ РРњР•РќРќРћ РїРѕР»СѓС‡Р°СЃРѕРІС‹РјРё РѕРєРЅР°РјРё (РЅР°РїСЂРёРјРµСЂ 19:00:00-19:30:00),
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
// результата архива ВКМ. S и ST — интегральные за период суммы (растут
// пропорционально длине запрошенного периода); T и Pi — мгновенные
// показания на конец периода, а не суммы.
//
// ИСПРАВЛЕНО (2026-08-29, найдено оператором): T и Pi раньше были
// сознательно исключены отсюда именно из-за этого различия — опасались,
// что при агрегации «по суткам»/«по месяцам» в api_archive.go они будут
// СУММИРОВАТЬСЯ так же, как S/ST, что для температуры/давления physически
// бессмысленно (сумма температур за сутки — не то же самое, что средняя
// температура). Это привело к другому, более заметному багу: вкладка
// «Архивы» в UI показывала только 2 параметра из 4 (масса и тепло),
// давление и температура не отображались вообще, хотя в саму ЭС они
// исправно уходят (см. energosphere_sync.go — тот путь читает их
// отдельно, напрямую из сырой строки, минуя archive_hourly, поэтому
// работал всегда). Правильное решение — не прятать T/Pi из архива
// целиком, а агрегировать их иначе: см. paramAverage в api_archive.go
// (усреднение вместо суммирования для «по суткам»/«по месяцам»; на
// самой мелкой группировке — «как хранится» — агрегации нет вообще, там
// разницы между суммой и средним для одной записи не существует).
var vkmHourlyParams = []string{"S", "ST", "T", "Pi"}

const (
	vkmMinPipe = 1
	vkmMaxPipe = 10
)

// vkmPipeChannel maps a VKM pipe to archive_hourly.channel. Pipe 1 keeps the
// historical empty channel for full backward compatibility with existing
// databases, UI queries and ES mappings. Additional pipes use their decimal
// number ("2".."10"), so identical parameter names from different pipes
// never collide on archive_hourly's primary key.
func vkmPipeChannel(pipe int) string {
	if pipe <= 1 {
		return ""
	}
	return strconv.Itoa(pipe)
}

type vkmActivePipeStore interface {
	GetVKMActivePipes(ctx context.Context, deviceID string) ([]int, error)
}

type vkmRawRangeStore interface {
	GetVKMRawStringsRange(ctx context.Context, deviceID string, pipe int, fromTs, toTs time.Time) ([]storage.VKMRawRow, error)
}

type vkmNoRecordStore interface {
	MarkVKMNoRecords(ctx context.Context, deviceID string, pipe int, ts time.Time, retryAfter time.Time) error
	VKMNoRecordsSuppressed(ctx context.Context, deviceID string, pipe int, ts, now time.Time) (bool, error)
	ClearVKMNoRecords(ctx context.Context, deviceID string, pipe int, ts time.Time) error
}

// vkmActivePipes reads the durable active-pipe set on every archive operation
// so a discovery/configuration change can take effect without a service
// restart. Old installations have no rows and therefore remain pipe-1-only.
func (d *Device) vkmActivePipes(ctx context.Context) []int {
	store, ok := d.Repo.(vkmActivePipeStore)
	if !ok {
		return []int{1}
	}
	pipes, err := store.GetVKMActivePipes(ctx, d.ID)
	if err != nil {
		log.Printf("[%s] VKM: не удалось прочитать список активных трубопроводов: %v; использую трубопровод 1\n", d.ID, err)
		return []int{1}
	}
	if len(pipes) == 0 {
		return []int{1}
	}
	valid := make([]int, 0, len(pipes))
	for _, pipe := range pipes {
		if pipe >= vkmMinPipe && pipe <= vkmMaxPipe {
			valid = append(valid, pipe)
		}
	}
	if len(valid) == 0 {
		log.Printf("[%s] VKM: список активных трубопроводов не содержит номеров 1..10; использую трубопровод 1\n", d.ID)
		return []int{1}
	}
	return valid
}

// persistVKMHourly сохраняет vkmHourlyParams из одного результата архива
// ВКМ как строки archive_hourly, привязанные к periodLabel — МЕТКЕ
// КОНЦА периода (например, для окна [09:30, 10:00) метка — 10:00), а не
// его началу.
//
// ВАЖНО (исправлено 2026-08-27): раньше здесь передавалось начало
// периода (periodStart) — то есть то же самое окно, что у Akron
// подписывается концом (10:00), у ВКМ подписывалось началом (9:30).
// Внешне это выглядело как "данные опаздывают на полчаса-час": оператор,
// сверяя с родным ПО прибора (там строки подписаны концом интервала —
// "09:00-10:00"), видел в нашей системе/в ЭС последнюю точку под меткой
// "9:30" вместо ожидаемой "10:00", хотя данные уже были полностью
// собраны и сохранены — просто НАЗВАНЫ иначе. Реальной задержки не было
// никогда, только несовпадение соглашения о подписи между Akron и ВКМ
// внутри нашей же системы. Теперь оба типа приборов подписывают архив
// одинаково — концом периода.
//
// ИСПРАВЛЕНО (2026-08-30, найдено оператором — лог разросся заметной
// частью из-за одного хронически неисправного прибора): раньше строка
// "поле X отсутствует (поля=...)" печаталась ОТДЕЛЬНО на каждое
// отсутствующее поле — для прибора с несколькими одновременно
// отсутствующими полями (например, оборванный датчик dP, из-за которого
// не считаются сразу и S, и T, и Pi) один и тот же полный дамп rec.Fields
// печатался по 2-3 раза подряд, почти без дополнительной пользы для
// диагностики. Теперь одна строка на период со списком всех
// отсутствующих полей сразу.
func persistVKMHourly(ctx context.Context, d *Device, pipe int, periodLabel time.Time, rec archive.ArchiveRecord) int {
	saved := 0
	var missing []string
	for _, param := range vkmHourlyParams {
		v, ok := fieldFloat(rec.Fields, param)
		if !ok {
			missing = append(missing, param)
			continue
		}
		unit, _ := rec.Fields[param+"_unit"].(string)

		r := storage.HourlyArchiveRecord{
			DeviceID: d.ID,
			Channel:  vkmPipeChannel(pipe),
			Param:    param,
			TsHour:   periodLabel,
			Value:    v,
			Unit:     unit,
		}
		if err := d.Repo.SaveHourlyArchive(ctx, r); err != nil {
			log.Printf("[%s] VKM трубопровод %d, период %s: ошибка сохранения %s: %v\n",
				d.ID, pipe, periodLabel.Format("02.01.2006 15:04"), param, err)
			continue
		}
		saved++
	}
	if len(missing) > 0 {
		log.Printf("[%s] VKM трубопровод %d, период %s: поля %s отсутствуют в ответе прибора (поля=%v)\n",
			d.ID, pipe, periodLabel.Format("02.01.2006 15:04"), strings.Join(missing, ", "), rec.Fields)
	}
	if saved > 0 {
		health.MarkArchiveSuccess(d.ID, time.Now())
	}
	return saved
}

// isVKMTimeAnomalous вЂ” РРЎРўРћР РР§Р•РЎРљРђРЇ С„СѓРЅРєС†РёСЏ, Р±С‹Р»Р° РёСЃС‚РѕС‡РЅРёРєРѕРј РіР»Р°РІРЅРѕР№
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

// vkmRawSecondsEpoch — опорная точка отсчёта для "секундного" формата
// поля Time= (см. doc-комментарий isVKMTimeAnomalous выше про сам
// формат).
//
// ИСПРАВЛЕНО (2026-08-30, первая попытка): раньше эпоха задавалась
// НАПРЯМУЮ через календарную дату 1999 года — time.Date(1999, 12, 31,
// 0, 30, 0, 0, time.Local). На реальном сервере это дало
// систематическое расхождение +1800 сек — заменено на вычисление через
// СЕГОДНЯШНЮЮ дату (см. ниже), чтобы Go не резолвил историческую
// дату 1999 года вообще.
//
// ИСПРАВЛЕНО ОКОНЧАТЕЛЬНО (2026-08-31, найдено оператором живьём: та же
// самая ошибка +1800 сек НЕ ИСЧЕЗЛА после первого исправления выше).
// Причина оказалась не в подходе (вычисление через сегодняшнюю дату —
// само по себе верно), а в том, что число секунд для якоря
// (841442400) было ОШИБОЧНО СЧИТАНО с экрана консоли ещё на самом
// первом шаге подбора эпохи — сдвинуто ровно на один период (полчаса)
// от истинного значения. Перепроверено заново по ЦЕПОЧКЕ из 5+
// последовательных периодов подряд (каждый следующий START в точности
// равен предыдущему END — так исключается ошибка чтения ОДНОЙ строки):
// период 29.08.2026 16:30 -> сырой конец периода 841422600, шаг +1800
// на каждые следующие полчаса. Верное число для 22:30 — 841444200, не
// 841442400. Проверено на 3 независимых точках цепочки одновременно,
// без единого расхождения.
var vkmRawSecondsEpoch = time.Date(2026, 8, 29, 22, 30, 0, 0, time.Local).Add(-841444200 * time.Second)

// parseVKMPeriodEndTime извлекает из поля Time= сырой архивной строки
// метку времени, которую СООБЩАЕТ САМ ПРИБОР как конец периода — нужно
// для мониторинга дрейфа времени (см. internal/devicestatus), добавлено
// 2026-08-29 по прямому запросу оператора ("мы никак не отслеживаем
// какое время сейчас в приборе").
//
// ИЗМЕНЕНО (2026-08-30): раньше "секундный" формат (см. doc-комментарий
// isVKMTimeAnomalous выше) считался непарсимым в принципе — числа не
// были подтверждены как секунды Unix-эпохи в какой-либо известной базе
// отсчёта, разбирать их как реальный момент значило бы молча придумать
// неверный дрейф. Теперь эпоха подобрана и подтверждена (см.
// vkmRawSecondsEpoch выше) — оба формата разбираются одинаково успешно.
// Возвращает ok=false только если поле Time отсутствует вовсе, или ни
// один из двух известных форматов не подошёл.
//
// Формат нормальной (датной) строки (оба варианта встречались живьём,
// см. TestIsVKMTimeAnomalous_RealExamples в vkm_hourly_test.go):
//
//	Time={Время  }28/07/26 15:00:00-28/07/26 15:30:00;...
//	Time=28/07/26 15:00:00-28/07/26 15:30:00;...
//
// Формат "голых секунд":
//
//	Time=841440600-841442400сек;...
//
// В обоих случаях берём вторую (правую) дату-время — конец периода, тот
// же момент, которым мы сами подписываем periodLabel в collectVKMPeriod.
func parseVKMPeriodEndTime(raw string) (time.Time, bool) {
	idx := strings.Index(raw, "Time=")
	if idx < 0 {
		return time.Time{}, false
	}
	rest := raw[idx+len("Time="):]
	value := rest
	if semi := strings.Index(rest, ";"); semi >= 0 {
		value = rest[:semi]
	}
	// Необязательный заголовок вида "{Время  }" перед самой датой —
	// убираем, если есть (у компактного формата его нет вообще).
	if brace := strings.Index(value, "}"); brace >= 0 {
		value = value[brace+1:]
	}

	dash := strings.Index(value, "-")
	if dash < 0 {
		return time.Time{}, false
	}
	endStr := strings.TrimSpace(value[dash+1:])

	if strings.Contains(value, "/") {
		t, err := time.ParseInLocation("02/01/06 15:04:05", endStr, time.Local)
		if err != nil {
			return time.Time{}, false
		}
		return t, true
	}

	// Секундный формат: endStr выглядит как "841442400сек" — отрезаем
	// суффикс единицы измерения и парсим как целое число секунд от
	// vkmRawSecondsEpoch.
	endStr = strings.TrimSuffix(strings.TrimSpace(endStr), "сек")
	secs, err := strconv.ParseInt(strings.TrimSpace(endStr), 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return vkmRawSecondsEpoch.Add(time.Duration(secs) * time.Second), true
}

// readVKMPeriodUnlocked performs one archive request for exactly one VKM pipe
// and one completed half-hour. The caller must already own the device lease.
// It only reads and decodes the response; persistence is deliberately left to
// collectVKMPeriod so discovery can probe pipe existence without creating
// normal archive rows or ES-healing work.
func (d *Device) readVKMPeriodUnlocked(ctx context.Context, a profile.Archive, pipe int, periodStart time.Time) (archive.ArchiveRecord, bool, error) {
	reader, ok := archive.Get(a.Strategy)
	if !ok {
		return archive.ArchiveRecord{}, false, fmt.Errorf("неизвестная стратегия %s", a.Strategy)
	}

	q := archive.ArchiveQuery{
		DeviceID:  d.ID,
		ArchiveID: a.ID,
		Instance:  pipe,
		From:      periodStart,
		To:        periodStart.Add(vkmArchivePeriod),
		Params:    a.Params,
	}

	records, err := reader.Read(ctx, sessionAdapter{d.Sess}, d.Client, q)
	if err != nil {
		return archive.ArchiveRecord{}, false, err
	}
	if len(records) == 0 {
		return archive.ArchiveRecord{}, false, nil
	}
	return records[0], true, nil
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
func (d *Device) collectVKMPeriod(ctx context.Context, a profile.Archive, pipe int, periodStart time.Time) (int, error) {
	// Keep the lease across BOTH the physical read and persistence, exactly as
	// the pre-discovery implementation did. This prevents another logical
	// operation for the same device from slipping between a successful read
	// and the corresponding archive write.
	release, leaseErr := d.acquireLeaseWithRetry(ctx, a.ID, 30*time.Second)
	if leaseErr != nil {
		return 0, fmt.Errorf("lease: %w", leaseErr)
	}
	defer release()

	rec, found, err := d.readVKMPeriodUnlocked(ctx, a, pipe, periodStart)
	periodLabel := periodStart.Add(vkmArchivePeriod)
	if err != nil {
		if errors.Is(err, archive.ErrVKMNoRecords) {
			if store, ok := d.Repo.(vkmNoRecordStore); ok {
				_ = store.MarkVKMNoRecords(ctx, d.ID, pipe, periodLabel, time.Now().Add(6*time.Hour))
			}
		}
		return 0, err
	}
	if !found {
		if store, ok := d.Repo.(vkmNoRecordStore); ok {
			_ = store.MarkVKMNoRecords(ctx, d.ID, pipe, periodLabel, time.Now().Add(6*time.Hour))
		}
		return 0, archive.ErrVKMNoRecords
	}
	if store, ok := d.Repo.(vkmNoRecordStore); ok {
		_ = store.ClearVKMNoRecords(ctx, d.ID, pipe, periodLabel)
	}

	// Сырую строку сохраняем отдельно от разобранных полей — она нужна
	// northbound-у, чтобы отдавать в ЭС ровно то, что прислал прибор (и с
	// тем же временным диапазоном, что реально запросили — иначе ЭС видит
	// несовпадение периода в ответе и не продвигается дальше, это и было
	// найдено живьём 2026-08-02). Ошибка сохранения сырой строки не должна
	// ронять сохранение S/ST — это две независимые вещи.
	//
	// Метка при сохранении (и сырой строки, и разобранных полей) —
	// КОНЕЦ периода (periodStart+vkmArchivePeriod), не его начало — см.
	// подробное объяснение в doc-комментарии persistVKMHourly. Сам
	// запрос к прибору по-прежнему построен от НАЧАЛА periodStart — это
	// два разных, не связанных использования одной переменной: одно для
	// окна запроса, другое для подписи результата.
	//
	// rec.Raw сохраняется здесь БУКВАЛЬНО как пришло от прибора
	// (parseTaggedString ничего в нём не меняет, кроме обрезки нулевых
	// байт) — northbound должен отдавать его в ЭС так же нетронуто, без
	// собственных текстовых преобразований (strip_headers/expand_exponent/
	// field_scale и т.п. — см. историю в vkm_config.go).
	if err := d.Repo.SaveVKMRawString(ctx, d.ID, pipe, periodLabel, string(rec.Raw)); err != nil {
		log.Printf("[%s] VKM трубопровод %d, период %s: ошибка сохранения сырой строки: %v\n",
			d.ID, pipe, periodLabel.Format("02.01.2006 15:04"), err)
	}

	// ВАЖНО: архивная метка Time= НЕ является чтением текущих часов
	// прибора. Сравнивать её с periodLabel и показывать результат как
	// "расхождение времени прибора" нельзя: именно это давало ложное
	// стабильное +0 сек на дашборде. До подтверждения безопасного
	// read-only чтения текущих часов ВКМ этот путь дрейф не публикует.
	return persistVKMHourly(ctx, d, pipe, periodLabel, rec), nil
}

// pollVKMHourlyLatest — обычный плановый опрос архива ВКМ (аналог часового
// тика PollArchives у Акрона). Запрашивает последний ПОЛНОСТЬЮ завершённый
// получасовой период.
type vkmCatchUpMissingFunc func(ctx context.Context, pipe int, from, to time.Time) ([]time.Time, error)
type vkmCatchUpCollectFunc func(ctx context.Context, pipe int, periodStart time.Time) (int, error)

// runVKMCatchUpAt contains the scheduler catch-up policy independently from
// physical I/O, making delayed-tick/no-record behaviour regression-testable.
func runVKMCatchUpAt(ctx context.Context, now time.Time, depthHours int, pipes []int, missingFn vkmCatchUpMissingFunc, collectFn vkmCatchUpCollectFunc) error {
	// Scheduled catch-up is deliberately capped at one day. The configured
	// BackfillMaxDepthHours may be hundreds of hours for startup/manual
	// recovery; reusing that depth on every scheduler tick would make an
	// unreachable VKM monopolize a shared bus for many minutes or hours.
	if depthHours <= 0 || depthHours > vkmDefaultBackfillDepthHours {
		depthHours = vkmDefaultBackfillDepthHours
	}
	last := now.Truncate(vkmArchivePeriod).Add(-vkmArchivePeriod)
	from := last.Add(-time.Duration(depthHours*2-1) * vkmArchivePeriod)
	for _, pipe := range pipes {
		missing, err := missingFn(ctx, pipe, from, last)
		if err != nil {
			return err
		}
		for _, periodStart := range missing {
			if err := ctx.Err(); err != nil {
				return err
			}
			_, err := collectFn(ctx, pipe, periodStart)
			if err != nil {
				if isVKMCatchUpCommunicationError(err) {
					// The device/line is unavailable. Stop this pipe immediately
					// instead of replaying every missing period through the same
					// transport timeout/retry cycle. Other pipes still get one try.
					break
				}
				// No-record and meter/data-level errors are period-local: leave
				// the gap visible (or suppressed by its marker) and continue so
				// one bad historical period does not hide later good periods.
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	return nil
}

// isVKMCatchUpCommunicationError identifies failures where retrying another
// historical period cannot help because the physical/protocol path itself is
// unavailable. Lower layers wrap the shared sentinels for normal I/O errors;
// the string fallbacks cover older protocol errors that predate those
// sentinels (notably CRC/frame/deadline messages) without changing their
// public contracts in this focused P0 patch.
func isVKMCatchUpCommunicationError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, errs.ErrTimeout) ||
		errors.Is(err, errs.ErrClosed) ||
		errors.Is(err, errs.ErrTransport) ||
		errors.Is(err, errs.ErrLease) ||
		errors.Is(err, errs.ErrAuth) ||
		errors.Is(err, errs.ErrBusy) ||
		errors.Is(err, errs.ErrCRC) ||
		errors.Is(err, errs.ErrFrame) {
		return true
	}
	s := strings.ToLower(err.Error())
	for _, marker := range []string{
		"timeout", "deadline exceeded", "invalid crc", "crc mismatch",
		"rtu frame", "tcp frame", "not open", "transport",
		"transaction id mismatch", "unit id mismatch", "modbus exception",
		"response too short", "byte count mismatch", "function mismatch",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// pollVKMHourlyLatest is now a bounded catch-up pass. A delayed scheduler tick
// therefore reads every missing completed half-hour in the configured depth,
// rather than only whichever period happens to be "latest" when it finally runs.
func (d *Device) pollVKMHourlyLatest(ctx context.Context, a profile.Archive) {
	depthHours := d.BackfillMaxDepthHours
	if depthHours <= 0 {
		depthHours = vkmDefaultBackfillDepthHours
	}
	pipes := d.vkmActivePipes(ctx)
	_ = runVKMCatchUpAt(ctx, time.Now(), depthHours, pipes,
		func(ctx context.Context, pipe int, from, to time.Time) ([]time.Time, error) {
			return missingVKMPeriods(ctx, d.Repo, d.ID, pipe, from, to)
		},
		func(ctx context.Context, pipe int, periodStart time.Time) (int, error) {
			saved, err := d.collectVKMPeriod(ctx, a, pipe, periodStart)
			health.MarkPollProgress(d.ID, time.Now())
			if err != nil {
				if errors.Is(err, archive.ErrVKMNoRecords) {
					dbg.Printf("[%s] VKM архив %s, трубопровод %d: период %s: прибор подтвердил отсутствие записи\n",
						d.ID, a.ID, pipe, periodStart.Add(vkmArchivePeriod).Format("02.01.2006 15:04"))
				} else {
					log.Printf("[%s] VKM архив %s, трубопровод %d: период %s: ошибка: %v\n",
						d.ID, a.ID, pipe, periodStart.Add(vkmArchivePeriod).Format("02.01.2006 15:04"), err)
				}
				return saved, err
			}
			log.Printf("[%s] VKM архив %s, трубопровод %d: период %s: сохранено полей: %d/%d\n",
				d.ID, a.ID, pipe, periodStart.Add(vkmArchivePeriod).Format("02.01.2006 15:04"), saved, len(vkmHourlyParams))
			return saved, nil
		},
	)
}

// missingVKMPeriods находит получасовые периоды в [from, to] (from/to —
// НАЧАЛА периодов), для которых нет сырой archive_vkm_raw записи именно
// этого трубопровода. Нельзя использовать параметр S как признак наличия:
// дополнительный (например газовый) трубопровод может быть полностью
// корректным и при этом вообще не содержать S.
//
// SaveVKMRawString хранит КОНЕЦ периода, тогда как этот helper и
// collectVKMPeriod оперируют НАЧАЛОМ. Поэтому при сверке с сохранёнными
// метками используется сдвиг +vkmArchivePeriod.
func missingVKMPeriods(ctx context.Context, repo storage.Repo, deviceID string, pipe int, from, to time.Time) ([]time.Time, error) {
	present := make(map[int64]bool)
	if rangeStore, ok := repo.(vkmRawRangeStore); ok {
		rows, err := rangeStore.GetVKMRawStringsRange(ctx, deviceID, pipe, from.Add(vkmArchivePeriod), to.Add(vkmArchivePeriod))
		if err != nil {
			return nil, fmt.Errorf("чтение существующих сырых периодов трубопровода %d: %w", pipe, err)
		}
		for _, row := range rows {
			present[row.TsHour.Unix()] = true
		}
	} else {
		// Narrow test doubles may expose only storage.Repo. Use exact lookups as
		// a compatibility fallback; production SQLite takes the range path above.
		for t := from; !t.After(to); t = t.Add(vkmArchivePeriod) {
			label := t.Add(vkmArchivePeriod)
			_, found, err := repo.GetVKMRawString(ctx, deviceID, pipe, label)
			if err != nil {
				return nil, fmt.Errorf("чтение сырого периода трубопровода %d %s: %w", pipe, label.Format("02.01.2006 15:04"), err)
			}
			if found {
				present[label.Unix()] = true
			}
		}
	}

	var missing []time.Time
	for t := from; !t.After(to); t = t.Add(vkmArchivePeriod) {
		label := t.Add(vkmArchivePeriod)
		if present[label.Unix()] {
			continue
		}
		if store, ok := repo.(vkmNoRecordStore); ok {
			suppressed, err := store.VKMNoRecordsSuppressed(ctx, deviceID, pipe, label, time.Now())
			if err != nil {
				return nil, err
			}
			if suppressed {
				continue
			}
		}
		missing = append(missing, t)
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

	for _, pipe := range d.vkmActivePipes(ctx) {
		missing, err := missingVKMPeriods(ctx, d.Repo, d.ID, pipe, fromBoundary, toBoundary)
		if err != nil {
			log.Printf("[%s] VKM дозабор %s, трубопровод %d: не удалось вычислить пропуски: %v\n", d.ID, a.ID, pipe, err)
			continue
		}
		if len(missing) == 0 {
			dbg.Printf("[%s] VKM дозабор %s, трубопровод %d: пропусков в пределах %dч (%d периодов по 30 мин) нет\n",
				d.ID, a.ID, pipe, depthHours, periodsCount)
			continue
		}

		log.Printf("[%s] VKM дозабор %s, трубопровод %d: старт, пропущено периодов: %d из %d\n",
			d.ID, a.ID, pipe, len(missing), periodsCount)

		periodsFilled := 0
		for _, period := range missing {
			select {
			case <-ctx.Done():
				log.Printf("[%s] VKM дозабор %s, трубопровод %d: прервано контекстом (заполнено периодов: %d/%d)\n",
					d.ID, a.ID, pipe, periodsFilled, len(missing))
				return
			default:
			}

			saved, err := d.collectVKMPeriod(ctx, a, pipe, period)
			if err != nil {
				if errors.Is(err, archive.ErrVKMNoRecords) {
					dbg.Printf("[%s] VKM дозабор %s, трубопровод %d: период %s: прибор подтвердил отсутствие записи\n",
						d.ID, a.ID, pipe, period.Add(vkmArchivePeriod).Format("02.01.2006 15:04"))
				} else {
					log.Printf("[%s] VKM дозабор %s, трубопровод %d: период %s: ошибка: %v\n",
						d.ID, a.ID, pipe, period.Add(vkmArchivePeriod).Format("02.01.2006 15:04"), err)
				}
				continue
			}
			if saved > 0 {
				periodsFilled++
			}
			health.MarkPollProgress(d.ID, time.Now())
			dbg.Printf("[%s] VKM дозабор %s, трубопровод %d: период %s: сохранено полей: %d/%d\n",
				d.ID, a.ID, pipe, period.Add(vkmArchivePeriod).Format("02.01.2006 15:04"), saved, len(vkmHourlyParams))
		}
		log.Printf("[%s] VKM дозабор %s, трубопровод %d: готово, заполнено периодов: %d/%d\n",
			d.ID, a.ID, pipe, periodsFilled, len(missing))
	}
}

package device

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"mbgw/internal/archive"
	"mbgw/internal/dbg"
	"mbgw/internal/devicestatus"
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
func persistVKMHourly(ctx context.Context, d *Device, periodLabel time.Time, rec archive.ArchiveRecord) int {
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
			Channel:  "",
			Param:    param,
			TsHour:   periodLabel,
			Value:    v,
			Unit:     unit,
		}
		if err := d.Repo.SaveHourlyArchive(ctx, r); err != nil {
			log.Printf("[%s] VKM период %s: ошибка сохранения %s: %v\n",
				d.ID, periodLabel.Format("02.01.2006 15:04"), param, err)
			continue
		}
		saved++
	}
	if len(missing) > 0 {
		log.Printf("[%s] VKM период %s: поля %s отсутствуют в ответе прибора (поля=%v)\n",
			d.ID, periodLabel.Format("02.01.2006 15:04"), strings.Join(missing, ", "), rec.Fields)
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
// формат). Подобрана ЭМПИРИЧЕСКИ 2026-08-30 сверкой 6 последовательных
// периодов архива boylernaya_par подряд (охват ~11 часов, через
// полночь) — вычисленное «эпоха + сырые секунды конца периода» СОВПАЛО
// С НАШЕЙ СОБСТВЕННОЙ меткой periodLabel ТОЧНО ДО СЕКУНДЫ на всех 6
// записях без единого расхождения. Это не совпадение — устойчивое
// повторение на 6 независимых точках такую вероятность практически
// исключает.
//
// ВНИМАНИЕ: подобрано по одной сессии наблюдения, без перехода через
// летнее/зимнее время и без данных за другие даты года. Если после
// этого фикса дрейф вдруг начнёт показывать резкие скачки или
// подозрительно круглые числа (например, ровно ±1ч в момент смены
// времени) — повод пересмотреть эту эпоху, возможно её расчёт зависит
// от даты сложнее, чем предполагается здесь.
var vkmRawSecondsEpoch = time.Date(1999, 12, 31, 0, 30, 0, 0, time.Local)

// parseVKMPeriodEndTime extracts the device-reported END-of-period
// timestamp from the raw archive string's Time= field — for time-drift
// monitoring (см. internal/devicestatus), добавлено 2026-08-29 по
// прямому запросу оператора ("мы никак не отслеживаем какое время
// сейчас в приборе").
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

// collectVKMPeriod РґРµР»Р°РµС‚ РћР”РРќ РїРѕР»РЅС‹Р№ С‚Р°РЅРµС† Р·Р°РїРёСЃСЊ/РѕР¶РёРґР°РЅРёРµ/С‡С‚РµРЅРёРµ Р°СЂС…РёРІР°
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
	// Метка при сохранении (и сырой строки, и разобранных полей) —
	// КОНЕЦ периода (periodStart+vkmArchivePeriod), не его начало — см.
	// подробное объяснение в doc-комментарии persistVKMHourly. Сам
	// запрос к прибору (q.From/q.To выше) по-прежнему построен от
	// НАЧАЛА periodStart — это два разных, не связанных использования
	// одной переменной: одно для окна запроса, другое для подписи
	// результата.
	//
	// records[0].Raw сохраняется здесь БУКВАЛЬНО как пришло от прибора
	// (parseTaggedString ничего в нём не меняет, кроме обрезки нулевых
	// байт) — northbound должен отдавать его в ЭС так же нетронуто, без
	// собственных текстовых преобразований (strip_headers/expand_exponent/
	// field_scale и т.п. — см. историю в vkm_config.go).
	periodLabel := periodStart.Add(vkmArchivePeriod)
	if err := d.Repo.SaveVKMRawString(ctx, d.ID, q.Instance, periodLabel, string(records[0].Raw)); err != nil {
		log.Printf("[%s] VKM период %s: ошибка сохранения сырой строки: %v\n",
			d.ID, periodLabel.Format("02.01.2006 15:04"), err)
	}

	// Мониторинг дрейфа часов прибора (см. internal/devicestatus) —
	// сравниваем время конца периода, которое НАЗЫВАЕТ САМ ПРИБОР в
	// своём ответе, с periodLabel (то же самое время, но по НАШИМ часам
	// сервера, от которого мы формировали запрос q.To). Расхождение
	// между ними и есть дрейф часов прибора относительно сервера.
	// Обновляем при КАЖДОМ успешном чтении архива, включая случаи, когда
	// формат не позволяет определить точное время (Reliable=false) — это
	// тоже полезная, актуальная информация ("сейчас не можем сказать"),
	// а не повод молча оставить старое, возможно уже устаревшее значение
	// висеть на дашборде.
	if deviceEnd, ok := parseVKMPeriodEndTime(string(records[0].Raw)); ok {
		devicestatus.Set(d.ID, devicestatus.TimeDrift{
			CheckedAt:    time.Now(),
			DriftSeconds: deviceEnd.Sub(periodLabel).Seconds(),
			Reliable:     true,
		})
	} else {
		devicestatus.Set(d.ID, devicestatus.TimeDrift{
			CheckedAt: time.Now(),
			Reliable:  false,
			Note:      "поле Time отсутствует в ответе прибора, или его формат не подошёл ни под один из двух известных вариантов",
		})
		// Логируем ТОЛЬКО фрагмент вокруг Time= (не всю сырую строку —
		// она может быть длинной), чтобы при следующем разборе "не
		// определено" на дашборде можно было сразу увидеть ПОЧЕМУ, не
		// гадая (найдено оператором 2026-08-30 — предыдущая версия
		// вообще не логировала эту причину).
		raw := string(records[0].Raw)
		snippet := raw
		if idx := strings.Index(raw, "Time="); idx >= 0 {
			end := idx + 80
			if end > len(raw) {
				end = len(raw)
			}
			snippet = raw[idx:end]
		}
		log.Printf("[%s] VKM период %s: дрейф времени не определён, фрагмент ответа: %q\n",
			d.ID, periodLabel.Format("02.01.2006 15:04"), snippet)
	}

	return persistVKMHourly(ctx, d, periodLabel, records[0]), nil
}

// pollVKMHourlyLatest — обычный плановый опрос архива ВКМ (аналог часового
// тика PollArchives у Акрона). Запрашивает последний ПОЛНОСТЬЮ завершённый
// получасовой период.
func (d *Device) pollVKMHourlyLatest(ctx context.Context, a profile.Archive) {
	periodStart := time.Now().Truncate(vkmArchivePeriod).Add(-vkmArchivePeriod)

	saved, err := d.collectVKMPeriod(ctx, a, periodStart)
	if err != nil {
		log.Printf("[%s] VKM архив %s: период %s: ошибка: %v\n",
			d.ID, a.ID, periodStart.Add(vkmArchivePeriod).Format("02.01.2006 15:04"), err)
		return
	}
	log.Printf("[%s] VKM архив %s: период %s: сохранено полей: %d/%d\n",
		d.ID, a.ID, periodStart.Add(vkmArchivePeriod).Format("02.01.2006 15:04"), saved, len(vkmHourlyParams))
}

// missingVKMPeriods находит получасовые периоды в [from, to] (from/to —
// НАЧАЛА периодов, тот же смысл, что и periodStart в остальном файле),
// для которых ещё нет строки archive_hourly с param="S" — собственный
// расчёт (не через storage.Repo.MissingHours, который жёстко считает по
// часу) специально под получасовой шаг ВКМ.
//
// ВАЖНО (2026-08-27): раз сохраняем теперь по МЕТКЕ КОНЦА периода (см.
// persistVKMHourly), а перебираем здесь диапазон НАЧАЛАМИ периодов
// (так исторически сложилось в backfillVKMHourly, менять не стал, чтобы
// не трогать лишнего) — проверка присутствия обязана сдвигать каждую
// проверяемую точку на +vkmArchivePeriod при сверке с уже сохранёнными
// метками, иначе решит, что ничего не сохранено, хотя на самом деле всё
// уже есть, просто под другой меткой.
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
		present[r.TsHour.Unix()] = true // r.TsHour — уже метка КОНЦА периода
	}

	var missing []time.Time
	for t := from; !t.After(to); t = t.Add(vkmArchivePeriod) {
		label := t.Add(vkmArchivePeriod) // t — начало периода, label — соответствующий ему конец
		if !present[label.Unix()] {
			missing = append(missing, t) // в missing по-прежнему кладём НАЧАЛО — collectVKMPeriod ждёт именно его
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
				d.ID, a.ID, period.Add(vkmArchivePeriod).Format("02.01.2006 15:04"), err)
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
			d.ID, a.ID, period.Add(vkmArchivePeriod).Format("02.01.2006 15:04"), saved, len(vkmHourlyParams))
	}
	log.Printf("[%s] VKM дозабор %s: готово, заполнено периодов: %d/%d\n", d.ID, a.ID, periodsFilled, len(missing))
}

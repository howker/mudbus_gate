package web

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"mbgw/internal/interval"
	"mbgw/internal/storage"
)

// api_archive.go implements the archive-viewer tab's backend: GET
// /api/archive (JSON, for the on-page table) and GET /api/archive/export
// (CSV download) — both share the same query/aggregation logic
// (loadArchiveTable), differing only in how the result is serialized.
//
// "По каждому каналу сразу" (2026-08-23 feedback): archive_hourly stores
// one row per (device, param, period) — Akron has one param ("V"), ВКМ
// has two ("S","ST"). This file PIVOTS those rows so each table row is
// one period with every param as its own column, instead of the raw
// per-param row shape GetHourlyArchiveRange returns.
//
// Granularity ("по часам/суткам/месяцам", 2026-08-23 feedback): the
// finest option is NOT literally forced to hour boundaries — Akron's
// native period is hourly, ВКМ's is 30 minutes (see vkm_hourly.go's
// vkmArchivePeriod comment on why: Энергосфера's own driver dictates this
// step, it isn't a choice made here) — the finest granularity shows
// exactly what's stored, at its own native step, labeled "Подробно (как
// хранится)" in the UI rather than promising an hour grid that wouldn't
// match ВКМ's real data. "По суткам"/"по месяцам" SUM every period
// falling in each calendar day/month — correct for these specific
// params, since both S/ST (ВКМ) and V (Akron) are interval sums
// (сколько прошло/натекло за период), not instantaneous readings — see
// vkm_hourly.go's vkmHourlyParams doc comment for why summing (not
// averaging) is the physically correct aggregation here.

// devicesParams maps a device kind to the archive_hourly params it
// stores — the fixed, small vocabulary this project's two device kinds
// use (see vkm_hourly.go's vkmHourlyParams, akron_hourly.go's Param: "V").
// A device kind outside this map (should not happen given handleDevices'
// own kind validation) yields an empty param list, not an error — an
// empty table is a safer failure than a 500.
//
// ИСПРАВЛЕНО (2026-08-29, найдено оператором): T и Pi раньше отсутствовали
// здесь — вкладка «Архивы» показывала для ВКМ только 2 параметра из 4
// (масса и тепло), температура и давление не отображались вообще, хотя
// в archive_hourly они (после фикса в vkm_hourly.go) уже сохраняются.
var devicesParams = map[string][]string{
	"vkm360": {"S", "ST", "T", "Pi"},
	"akron":  {"V"},
	"ivk-ter": {
		"v_plus", "v_minus", "q_avg", "resistance", "errors",
		"comm_fail_time", "flowmeter_type", "downtime", "power_loss_time",
	},
}

// paramLabels gives each raw param code a Russian display name for the
// table header (уже с учётом единиц ПОСЛЕ пересчёта — см. paramDisplayFactor).
var paramLabels = map[string]string{
	"S":               "Масса, т",
	"ST":              "Тепловая энергия, Гкал",
	"T":               "Температура, °C",
	"Pi":              "Давление, Па",
	"V":               "Расход за период, м³",
	"v_plus":          "Объём прямой, м³",
	"v_minus":         "Объём обратный, м³",
	"q_avg":           "Средний расход, л/мин",
	"resistance":      "Сопротивление, Ом",
	"errors":          "Код ошибок",
	"comm_fail_time":  "Нет связи, мин",
	"flowmeter_type":  "Тип расходомера (код)",
	"downtime":        "Простой, мин",
	"power_loss_time": "Нет питания, мин",
}

// paramDisplayFactor — множитель, который переводит СЫРОЕ хранимое
// значение (то, в чём его отдаёт сам прибор) в единицы, привычные
// оператору и совпадающие с тем, что показывает родная программа учёта
// прибора. Проверено сверкой (2026-08-23): наша сырая масса за час,
// делённая на 1000, совпадает с "т" родной программы с точностью до
// третьего знака; тепло переводится тем же коэффициентом, что уже
// подтверждён сверкой с официальным отчётом прибора в
// internal/integration/energosphere_sync.go (4.1868e9 Дж в одной Гкал).
// Отсутствие ключа = множитель 1 (без пересчёта) — так остаётся для "V"
// (Akron, уже в м³ как есть), для "T" (уже в °C, пересчёт не нужен) и
// для "Pi" (сырое значение — Паскали, показываем как есть; при желании
// более привычных единиц — см. аналогичный множитель для Pi в
// подсказках вкладки «Каналы ЭС», internal/web/api_admin_ui.go).
var paramDisplayFactor = map[string]float64{
	"S":  1.0 / 1000.0,   // кг -> т
	"ST": 1.0 / 4.1868e9, // Дж -> Гкал
}

// paramCumulative marks params whose STORED value is a running counter
// (общий накопленный объём с начала эксплуатации — как одометр), not an
// already-computed per-period amount. Confirmed live 2026-08-23: Akron's
// "V" values in archive_hourly climb by roughly ~110-150 between
// consecutive hourly rows — that DIFFERENCE matches the known-correct
// hourly flow (~140 м³/ч, verified earlier against physical plausibility
// checks), while the raw stored numbers themselves (millions) do not
// represent any single hour's flow at all. This was mistakenly treated
// as a data-corruption anomaly for a large part of this project's
// history (see docs/BACKFILL_DESIGN.md-adjacent investigation) — the
// underlying data was correct the whole time; the missing step was
// differencing consecutive readings, not summing them.
//
// ВКМ's S/ST are NOT cumulative — they are genuinely already
// per-period sums, confirmed by direct comparison against the meter's
// own printed report (see internal/integration/energosphere_sync.go's
// package doc for that verification) — summing them across a wider
// bucket (daily/monthly) is correct and stays unchanged.
var paramCumulative = map[string]bool{
	"V": true,
}

// paramAverage marks params whose STORED value is an INSTANTANEOUS
// reading at the end of each period (not an interval sum) — ВКМ's T
// (температура) and Pi (давление), see vkm_hourly.go's vkmHourlyParams
// doc comment for the full history. At the finest ("raw"/"как хранится")
// granularity this distinction doesn't matter — each bucket is exactly
// one stored period either way. It matters ONLY for "по суткам"/"по
// месяцам": summing a whole day of instantaneous temperature readings
// (like S/ST's additive path does) would be physically meaningless —
// AVERAGING them across the bucket is the correct aggregation instead
// (added 2026-08-29, together with adding T/Pi to devicesParams — see
// that doc comment for the bug this fixes).
var paramAverage = map[string]bool{
	"T":  true,
	"Pi": true,
}

type archiveRow struct {
	Period string             `json:"period"` // formatted per granularity — see formatPeriodLabel
	Values map[string]float64 `json:"values"` // param -> value for this row (delta for cumulative params, sum for additive ones)
}

type archiveResponse struct {
	DeviceID    string       `json:"device_id"`
	Pipe        int          `json:"pipe,omitempty"`
	Pipes       []int        `json:"pipes,omitempty"`
	Params      []string     `json:"params"`       // column order — stable, from devicesParams
	ParamLabels []string     `json:"param_labels"` // parallel to Params
	Rows        []archiveRow `json:"rows"`
}

// parseArchiveRangeBound accepts both the old date-only API form and the
// new minute-precise form used by the Archive tab. Keeping date-only support
// avoids breaking old bookmarks/scripts. A date-only upper bound means the
// whole day; a minute-precise upper bound includes that selected minute.
func parseArchiveRangeBound(value string, upper bool, loc *time.Location) (time.Time, error) {
	if loc == nil {
		loc = time.Local
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04", value, loc); err == nil {
		if upper {
			return t.Add(time.Minute - time.Nanosecond), nil
		}
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", value, loc); err == nil {
		if upper {
			return t.Add(24*time.Hour - time.Nanosecond), nil
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("ожидается ГГГГ-ММ-ДД или ГГГГ-ММ-ДДTЧЧ:ММ")
}

// loadArchiveTable does the shared work behind both the JSON and CSV
// endpoints: read query params, pull raw rows for every one of the
// device's params, pivot+aggregate, return a ready-to-render table.
func (s *Server) loadArchiveTable(r *http.Request) (archiveResponse, error) {
	deviceID := r.URL.Query().Get("device_id")
	if deviceID == "" {
		return archiveResponse{}, fmt.Errorf("параметр device_id обязателен")
	}
	fromStr := r.URL.Query().Get("from")
	toStr := r.URL.Query().Get("to")
	granularity := r.URL.Query().Get("granularity") // "raw" | "daily" | "monthly"
	if granularity == "" {
		granularity = "raw"
	}

	from, err := parseArchiveRangeBound(fromStr, false, time.Local)
	if err != nil {
		return archiveResponse{}, fmt.Errorf("параметр from: %w", err)
	}
	to, err := parseArchiveRangeBound(toStr, true, time.Local)
	if err != nil {
		return archiveResponse{}, fmt.Errorf("параметр to: %w", err)
	}
	if to.Before(from) {
		return archiveResponse{}, fmt.Errorf("конец периода не может быть раньше начала")
	}

	dev, found, err := s.repo.GetDevice(r.Context(), deviceID)
	if err != nil {
		return archiveResponse{}, fmt.Errorf("не удалось прочитать прибор: %w", err)
	}
	if !found {
		return archiveResponse{}, fmt.Errorf("прибор %q не найден", deviceID)
	}

	// Для ИВК-ТЭР в этом пакете выводим только исходные часовые записи.
	// Семантика суточной/месячной агрегации полей архива (особенно кодов
	// ошибок и типа расходомера) не должна выдумываться до live-проверки
	// реального прибора. Обычный опрос и дозабор при этом полностью работают.
	if dev.Kind == "ivk-ter" && granularity != "raw" {
		return archiveResponse{}, fmt.Errorf("для ИВК-ТЭР пока доступен только режим «Подробно (как хранится)»; группировка будет добавлена после live-проверки семантики архивных полей")
	}

	archiveChannel := ""
	selectedPipe := 0
	var activePipes []int
	if dev.Kind == "vkm360" {
		activePipes, err = configuredVKMPipes(r.Context(), s.repo, deviceID)
		if err != nil {
			return archiveResponse{}, fmt.Errorf("не удалось прочитать активные трубопроводы ВКМ: %w", err)
		}
		selectedPipe = activePipes[0]
		if pipeStr := r.URL.Query().Get("pipe"); pipeStr != "" {
			selectedPipe, err = strconv.Atoi(pipeStr)
			if err != nil || selectedPipe < 1 || selectedPipe > 10 {
				return archiveResponse{}, fmt.Errorf("параметр pipe должен быть номером трубопровода 1..10")
			}
		}
		if !containsPipe(activePipes, selectedPipe) {
			return archiveResponse{}, fmt.Errorf("трубопровод %d не отмечен активным для прибора %q", selectedPipe, deviceID)
		}
		archiveChannel = vkmArchiveChannel(selectedPipe)
	}

	params := devicesParams[dev.Kind]
	labels := make([]string, len(params))
	for i, p := range params {
		if l, ok := paramLabels[p]; ok {
			labels[i] = l
		} else {
			labels[i] = p
		}
	}

	// bucket key -> param -> value. Additive params (S/ST) accumulate by
	// SUM as rows are read (unchanged). Cumulative params (V) instead
	// keep the LAST raw reading seen in each bucket — a snapshot, not a
	// sum — which gets converted to a delta-from-previous-bucket in the
	// second pass below. Average params (T/Pi, добавлено 2026-08-29) тоже
	// накапливаются суммой здесь, как обычные аддитивные — но параллельно
	// bucketCounts считает, сколько сырых периодов попало в каждую
	// корзину, чтобы в конце поделить сумму на количество и получить
	// среднее (см. цикл ниже) — сумма мгновенных показаний сама по себе
	// физического смысла не имеет, только их среднее.
	buckets := make(map[string]map[string]float64)
	bucketCounts := make(map[string]map[string]int)
	var order []string

	for _, param := range params {
		// Для накопительных параметров нужна одна предыдущая СЫРАЯ
		// часовая запись. Дельта всегда считается между соседними
		// часовыми снимками ДО любой суточной/месячной группировки.
		queryFrom := from
		if paramCumulative[param] {
			queryFrom = from.Add(-time.Hour)
		}

		rows, err := s.repo.GetHourlyArchiveRange(r.Context(), deviceID, archiveChannel, param, queryFrom, to)
		if err != nil {
			return archiveResponse{}, fmt.Errorf("чтение архива (%s): %w", param, err)
		}

		if paramCumulative[param] {
			applyCumulativeDelta(rows, param, from, granularity, buckets, &order)
		} else {
			for _, row := range rows {
				key := formatPeriodLabel(row.TsHour, granularity)
				if _, ok := buckets[key]; !ok {
					buckets[key] = make(map[string]float64)
					order = append(order, key)
				}
				buckets[key][param] += row.Value
				if paramAverage[param] {
					if _, ok := bucketCounts[key]; !ok {
						bucketCounts[key] = make(map[string]int)
					}
					bucketCounts[key][param]++
				}
			}
		}
	}

	sort.Strings(order) // bucket labels are formatted so lexicographic sort == chronological
	seen := make(map[string]bool, len(order))
	uniqueOrder := order[:0]
	for _, k := range order {
		if !seen[k] {
			seen[k] = true
			uniqueOrder = append(uniqueOrder, k)
		}
	}

	out := make([]archiveRow, 0, len(uniqueOrder))
	for _, key := range uniqueOrder {
		// Применяем коэффициент пересчёта единиц (paramDisplayFactor)
		// один раз, здесь — единственное место, через которое проходят
		// ВСЕ итоговые значения, независимо от того, аддитивный параметр
		// или накопительный. Значения в buckets до этого момента остаются
		// в СЫРЫХ единицах прибора — так проще было считать сумму/разницу
		// выше, не путая единицы измерения с арифметикой.
		//
		// Для average-параметров (T/Pi, добавлено 2026-08-29) сумма из
		// buckets сначала делится на bucketCounts — превращая сумму
		// мгновенных показаний в их среднее — и только ПОСЛЕ этого
		// применяется paramDisplayFactor, тем же порядком, что и для
		// обычных аддитивных параметров.
		converted := make(map[string]float64, len(buckets[key]))
		for param, v := range buckets[key] {
			if paramAverage[param] {
				if c := bucketCounts[key][param]; c > 0 {
					v = v / float64(c)
				}
			}
			factor := paramDisplayFactor[param]
			if factor == 0 {
				factor = 1.0
			}
			converted[param] = v * factor
		}
		out = append(out, archiveRow{Period: key, Values: converted})
	}

	return archiveResponse{DeviceID: deviceID, Pipe: selectedPipe, Pipes: activePipes, Params: params, ParamLabels: labels, Rows: out}, nil
}

// formatPeriodLabel formats a stored timestamp into this row's bucket
// label, chosen so that a plain lexicographic string sort equals
// chronological order (no separate time parsing needed downstream).
func formatPeriodLabel(t time.Time, granularity string) string {
	switch granularity {
	case "hourly":
		// ИСПРАВЛЕНО 2026-08-27: раньше здесь было простое Truncate(1h)
		// (округление ВНИЗ) — это было верно, пока метка периода означала
		// НАЧАЛО интервала. После смены соглашения на "метка = КОНЕЦ
		// периода" (см. persistVKMHourly) простое Truncate стало
		// систематически неверным для меток, которые сами приходятся
		// РОВНО на границу часа: получасовка с меткой "17:00" на самом
		// деле означает интервал [16:30,17:00) — то есть ВТОРУЮ половину
		// часа [16:00,17:00) — но Truncate(1h) от "17:00" даёт "17:00",
		// а не "16:00", и эта получасовка улетала в СЛЕДУЮЩИЙ час на
		// экране. В итоге оба соседних часа показывали примерно ПОЛОВИНУ
		// правильной суммы (не хватало ровно одной из двух получасовок в
		// каждом из них) — подтверждено живьём, 2026-08-27, часы 16:00 и
		// 17:00 показывали ~1.49/~1.42 вместо ожидаемых ~3.0.
		//
		// Отступаем на 1 наносекунду ПЕРЕД округлением — так метка РОВНО
		// на границе часа (например "17:00:00.000") относится к
		// ПРЕДЫДУЩЕМУ часовому интервалу при округлении вниз, а затем
		// возвращаем полный час обратно, чтобы подпись корзины осталась
		// "концом часа" (17:00 = данные за [16:00,17:00)), как и везде
		// в системе. Для Akron (уже часовые метки, ровно на границах)
		// это не меняет результат — тот же час на входе, тот же час на
		// выходе.
		return t.Add(-time.Nanosecond).Truncate(time.Hour).Add(time.Hour).Format("2006-01-02 15:00")
	case "daily":
		return t.Format("2006-01-02")
	case "monthly":
		return t.Format("2006-01")
	default: // "raw" — native stored step (hourly for Akron, 30-min for ВКМ)
		return t.Format("2006-01-02 15:04")
	}
}

// applyCumulativeDelta converts raw cumulative counter snapshots into
// per-hour interval deltas using the SAME rule as Energosphere export:
// only two adjacent snapshots exactly one hour apart are valid, and a
// counter rollback is rejected.
//
// The important order is:
//  1. raw adjacent-hour counter delta;
//  2. assign that delta to the interval END timestamp;
//  3. only then aggregate valid interval deltas into raw/hourly/daily/
//     monthly UI buckets.
//
// This prevents a missing hour from being silently collapsed into one
// larger delta and keeps UI semantics identical to the ЭС path.
func applyCumulativeDelta(rows []storage.HourlyArchiveRecord, param string, from time.Time, granularity string, buckets map[string]map[string]float64, order *[]string) {
	if len(rows) < 2 {
		return
	}

	for i := 1; i < len(rows); i++ {
		prev := rows[i-1]
		current := rows[i]

		delta, ok := interval.CounterDelta(
			prev.TsHour, prev.Value,
			current.TsHour, current.Value,
			time.Hour,
		)
		if !ok {
			continue
		}

		// The current snapshot timestamp is the END of the interval whose
		// consumption is represented by delta.
		if current.TsHour.Before(from) {
			continue
		}

		key := formatPeriodLabel(current.TsHour, granularity)
		if _, ok := buckets[key]; !ok {
			buckets[key] = make(map[string]float64)
			*order = append(*order, key)
		}
		buckets[key][param] += delta
	}
}

func (s *Server) handleArchive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "используйте GET")
		return
	}
	resp, err := s.loadArchiveTable(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleArchiveExport streams the same table as CSV, semicolon-delimited
// (Excel's default list separator under a Russian locale) with a UTF-8
// BOM prefix so Cyrillic headers/values display correctly when opened
// directly in Excel instead of showing mojibake.
func (s *Server) handleArchiveExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "используйте GET")
		return
	}
	resp, err := s.loadArchiveTable(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	filename := fmt.Sprintf("archive_%s.csv", resp.DeviceID)
	if resp.Pipe > 0 {
		filename = fmt.Sprintf("archive_%s_pipe%d.csv", resp.DeviceID, resp.Pipe)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	_, _ = w.Write([]byte{0xEF, 0xBB, 0xBF}) // UTF-8 BOM

	cw := csv.NewWriter(w)
	cw.Comma = ';'

	header := append([]string{"Период"}, resp.ParamLabels...)
	_ = cw.Write(header)

	for _, row := range resp.Rows {
		record := make([]string, 0, len(resp.Params)+1)
		record = append(record, row.Period)
		for _, p := range resp.Params {
			v, ok := row.Values[p]
			if !ok {
				record = append(record, "")
				continue
			}
			record = append(record, fmt.Sprintf("%.5f", v)) // 5 знаков — соответствует точности родного ПО прибора, нужно для точной сверки, не 3 (было мало для диагностики расхождений, 2026-08-27)
		}
		_ = cw.Write(record)
	}
	cw.Flush()
}

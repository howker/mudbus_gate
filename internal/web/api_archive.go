package web

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"sort"
	"time"

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
var devicesParams = map[string][]string{
	"vkm360": {"S", "ST"},
	"akron":  {"V"},
}

// paramLabels gives each raw param code a Russian display name for the
// table header (уже с учётом единиц ПОСЛЕ пересчёта — см. paramDisplayFactor).
var paramLabels = map[string]string{
	"S":  "Масса, т",
	"ST": "Тепловая энергия, Гкал",
	"V":  "Расход за период, м³",
}

// paramDisplayFactor — множитель, который переводит СЫРОЕ хранимое
// значение (то, в чём его отдаёт сам прибор) в единицы, привычные
// оператору и совпадающие с тем, что показывает родная программа учёта
// прибора. Проверено сверкой (2026-08-23): наша сырая масса за час,
// делённая на 1000, совпадает с "т" родной программы с точностью до
// третьего знака; тепло переводится тем же коэффициентом, что уже
// подтверждён сверкой с официальным отчётом прибора в
// internal/integration/energosphere_sync.go (4.1868e9 Дж в одной Гкал).
// Отсутствие ключа = множитель 1 (без пересчёта) — так остаётся, например,
// для "V" (Akron), которое уже в м³ как есть.
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

type archiveRow struct {
	Period string             `json:"period"` // formatted per granularity — see formatPeriodLabel
	Values map[string]float64 `json:"values"` // param -> value for this row (delta for cumulative params, sum for additive ones)
}

type archiveResponse struct {
	DeviceID    string       `json:"device_id"`
	Params      []string     `json:"params"`       // column order — stable, from devicesParams
	ParamLabels []string     `json:"param_labels"` // parallel to Params
	Rows        []archiveRow `json:"rows"`
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

	from, err := time.ParseInLocation("2006-01-02", fromStr, time.Local)
	if err != nil {
		return archiveResponse{}, fmt.Errorf(`параметр from должен быть в формате ГГГГ-ММ-ДД`)
	}
	to, err := time.ParseInLocation("2006-01-02", toStr, time.Local)
	if err != nil {
		return archiveResponse{}, fmt.Errorf(`параметр to должен быть в формате ГГГГ-ММ-ДД`)
	}
	to = to.Add(24*time.Hour - time.Second) // inclusive through the end of the "to" day

	dev, found, err := s.repo.GetDevice(r.Context(), deviceID)
	if err != nil {
		return archiveResponse{}, fmt.Errorf("не удалось прочитать прибор: %w", err)
	}
	if !found {
		return archiveResponse{}, fmt.Errorf("прибор %q не найден", deviceID)
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
	// second pass below.
	buckets := make(map[string]map[string]float64)
	var order []string

	for _, param := range params {
		// Для накопительных параметров нужна ОДНА дополнительная запись
		// ДО начала окна — иначе для самой первой точки/суток/месяца в
		// выборке не с чем вычесть разницу. Расширяем окно запроса на
		// глубину периода назад и просто не включаем эту затравочную
		// точку в итоговые строки — она нужна только для вычитания.
		queryFrom := from
		if paramCumulative[param] {
			queryFrom = from.Add(-31 * 24 * time.Hour) // с запасом даже для "по месяцам"
		}

		rows, err := s.repo.GetHourlyArchiveRange(r.Context(), deviceID, "", param, queryFrom, to)
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
		converted := make(map[string]float64, len(buckets[key]))
		for param, v := range buckets[key] {
			factor := paramDisplayFactor[param]
			if factor == 0 {
				factor = 1.0
			}
			converted[param] = v * factor
		}
		out = append(out, archiveRow{Period: key, Values: converted})
	}

	return archiveResponse{DeviceID: deviceID, Params: params, ParamLabels: labels, Rows: out}, nil
}

// formatPeriodLabel formats a stored timestamp into this row's bucket
// label, chosen so that a plain lexicographic string sort equals
// chronological order (no separate time parsing needed downstream).
func formatPeriodLabel(t time.Time, granularity string) string {
	switch granularity {
	case "hourly":
		// Округляем ВНИЗ до начала часа — так получасовки 00:00 и 00:30
		// попадают в одну и ту же корзину "00:00" (для аддитивных
		// параметров — S/ST у ВКМ — их значения там же ниже просто
		// складываются, ровно как это делает родная программа учёта,
		// показывающая по умолчанию часовые, не получасовые, суммы).
		return t.Truncate(time.Hour).Format("2006-01-02 15:00")
	case "daily":
		return t.Format("2006-01-02")
	case "monthly":
		return t.Format("2006-01")
	default: // "raw" — native stored step (hourly for Akron, 30-min for ВКМ)
		return t.Format("2006-01-02 15:04")
	}
}

// applyCumulativeDelta превращает СЫРЫЕ показания накопительного счётчика
// (rows, по возрастанию времени, включая "затравочные" точки ДО from —
// см. вызов выше) в РАЗНИЦУ между соседними показаниями, и раскладывает
// результат по тем же корзинам (bucket), что и обычные суммируемые
// параметры — buckets/order изменяются на месте (передаются по указателю
// на срез, т.к. append может выделить новый массив).
//
// Логика: прибор хранит СНИМОК счётчика на момент наступления часа.
// Разница между снимком на 17:00 и снимком на 16:00 — это расход ЗА ЧАС
// С 16 ДО 17:00, и подписывается меткой СНИМКА-КОНЦА («17:00»), не
// началом интервала — так это устроено и в родной программе учёта
// прибора, и в самой Энергосфере: строка «17:00» показывает готовый
// расход только когда сам час [16:00,17:00) уже завершился и был заново
// опрошен; пока идёт ТЕКУЩИЙ, ещё не завершённый час — для него значения
// попросту ещё нет ни у нас, ни в ЭС.
//
// ВАЖНО (уточнено 2026-08-23 после ошибочной правки в этом же файле):
// метка НЕ сдвигается на предыдущий период ни для одной группировки —
// раньше здесь была неверная логика "raw -> метка начала интервала",
// отклонённая явно, отменена.
func applyCumulativeDelta(rows []storage.HourlyArchiveRecord, param string, from time.Time, granularity string, buckets map[string]map[string]float64, order *[]string) {
	if len(rows) == 0 {
		return
	}

	snapshots := make(map[string]float64)
	var snapshotOrder []string
	for _, row := range rows {
		key := formatPeriodLabel(row.TsHour, granularity)
		if _, ok := snapshots[key]; !ok {
			snapshotOrder = append(snapshotOrder, key)
		}
		snapshots[key] = row.Value
	}
	sort.Strings(snapshotOrder)

	fromKey := formatPeriodLabel(from, granularity)
	var prevValue float64
	havePrev := false
	for _, key := range snapshotOrder {
		current := snapshots[key]
		if key < fromKey {
			// затравочная точка ДО окна запроса — не показываем, только
			// запоминаем как базу для вычитания первой реальной точки
			prevValue = current
			havePrev = true
			continue
		}
		if havePrev {
			if _, ok := buckets[key]; !ok {
				buckets[key] = make(map[string]float64)
				*order = append(*order, key)
			}
			buckets[key][param] = current - prevValue
		}
		prevValue = current
		havePrev = true
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

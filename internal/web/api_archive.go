package web

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"sort"
	"time"
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
// table header.
var paramLabels = map[string]string{
	"S":  "Масса, т",
	"ST": "Тепловая энергия (как хранится)",
	"V":  "Расход, м³",
}

type archiveRow struct {
	Period string             `json:"period"` // formatted per granularity — see formatPeriodLabel
	Values map[string]float64 `json:"values"` // param -> summed/raw value for this row
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

	// bucket key -> param -> summed value. A plain map keyed by a
	// formatted string (not time.Time) sidesteps time-zone/truncation
	// edge cases entirely — the label IS the bucket identity.
	buckets := make(map[string]map[string]float64)
	var order []string

	for _, param := range params {
		rows, err := s.repo.GetHourlyArchiveRange(r.Context(), deviceID, "", param, from, to)
		if err != nil {
			return archiveResponse{}, fmt.Errorf("чтение архива (%s): %w", param, err)
		}
		for _, row := range rows {
			key := formatPeriodLabel(row.TsHour, granularity)
			if _, ok := buckets[key]; !ok {
				buckets[key] = make(map[string]float64)
				order = append(order, key)
			}
			buckets[key][param] += row.Value
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
		out = append(out, archiveRow{Period: key, Values: buckets[key]})
	}

	return archiveResponse{DeviceID: deviceID, Params: params, ParamLabels: labels, Rows: out}, nil
}

// formatPeriodLabel formats a stored timestamp into this row's bucket
// label, chosen so that a plain lexicographic string sort equals
// chronological order (no separate time parsing needed downstream).
func formatPeriodLabel(t time.Time, granularity string) string {
	switch granularity {
	case "daily":
		return t.Format("2006-01-02")
	case "monthly":
		return t.Format("2006-01")
	default: // "raw" — native stored step (hourly for Akron, 30-min for ВКМ)
		return t.Format("2006-01-02 15:04")
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
			record = append(record, fmt.Sprintf("%g", v))
		}
		_ = cw.Write(record)
	}
	cw.Flush()
}

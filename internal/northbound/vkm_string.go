package northbound

import (
	"fmt"
	"strings"
	"time"
)

// VKMParam is one parameter in an ЭЛЕМЕР-ВКМ-360 / УВП-280.01 archive
// result string.
type VKMParam struct {
	Tag    string // short latin/digit identifier, e.g. "M"
	Header string // archive form header text, goes in {curly braces}
	Value  string // value as text (used when opts bit3 is clear)
	// ValueFull is the max-precision rendering used when opts bit3
	// ("максимальное количество знаков") is set; falls back to Value if
	// empty.
	ValueFull string
	Unit      string // unit text; appended WITHOUT a space (per the doc)
}

// Option bits of request register 7914, per the ЭЛЕМЕР-ВКМ-360 register
// map. The result string's shape depends on these — found live: ЭС
// requests opts=12 (tags + max-precision/seconds, NO headers) and rejects
// a string that includes {headers} it never asked for ("Неверный формат
// пакета", vkm_live.jsonl 24.07.2026).
const (
	vkmOptUserUnits = 1 << 0 // 1: user-configured units; 0: standard units
	vkmOptHeaders   = 1 << 1 // include {header} info from the archive form
	vkmOptTags      = 1 << 2 // include parameter tags
	vkmOptMaxDigits = 1 << 3 // max float digits AND times as seconds
	vkmOptNoAbsTime = 1 << 4 // exclude absolute time marks
)

// BuildVKMArchiveString assembles an archive result string per the
// ЭЛЕМЕР-ВКМ-360 register map (p.7, "Чтение архивов"), SHAPED BY the
// options register (7914) from the master's own request:
//
//	[тэг][{шапка}]=<start>-<end>;[тэг][{шапка}]=<значение><ед.изм.>;…[NUL]
//
// Spec rules honored here:
//   - The FIRST parameter is the collection PERIOD — two timestamps
//     separated by "-". With opts bit3 set, timestamps are rendered as
//     seconds (Unix epoch — the doc says "время в секундах" without
//     pinning the epoch; overridable later if a real device shows
//     otherwise).
//   - {header} is included ONLY when opts bit1 requests it.
//   - The tag is included ONLY when opts bit2 requests it.
//   - Units follow the value with NO space.
//   - opts bit4 drops the absolute-time (period) parameter entirely.
//   - The string ends with a NUL byte, counted by register 8002.
func BuildVKMArchiveString(start, end time.Time, timeLayout string, params []VKMParam, opts uint16) string {
	if timeLayout == "" {
		timeLayout = VKMDefaultTimeLayout
	}
	var b strings.Builder

	writeParam := func(tag, header, value, unit string) {
		if opts&vkmOptTags != 0 {
			b.WriteString(tag)
		}
		if opts&vkmOptHeaders != 0 {
			b.WriteString("{")
			b.WriteString(header)
			b.WriteString("}")
		}
		b.WriteString("=")
		b.WriteString(value)
		b.WriteString(unit)
		b.WriteString(";")
	}

	if opts&vkmOptNoAbsTime == 0 {
		writeParam("T", "Период", vkmPeriodValue(start, end, timeLayout, opts), "")
	}

	for _, p := range params {
		v := p.Value
		if opts&vkmOptMaxDigits != 0 && p.ValueFull != "" {
			v = p.ValueFull
		}
		writeParam(p.Tag, p.Header, v, p.Unit)
	}

	b.WriteByte(0) // ANSI string terminator, counted in register 8002
	return b.String()
}

// VKMDefaultTimeLayout is the timestamp format used inside the archive
// string when opts bit3 (seconds) is not set. The register map does not
// pin the exact layout ("две записи времени"), so this Russian-
// conventional form is the default and is overridable from vkm_config.txt
// (time_layout) — letting alternatives be tried on the live server without
// a rebuild.
const VKMDefaultTimeLayout = "02.01.2006 15:04:05"

// defaultVKMParams is the stand-in data payload used until a real ВКМ is
// available to read from: mass and temperature, the two values that
// historically came through to Энергосфера from the real device.
func defaultVKMParams() []VKMParam {
	return []VKMParam{
		{Tag: "M", Header: "Масса", Value: "678.90", ValueFull: "678.900000", Unit: "кг"},
		{Tag: "t", Header: "Температура", Value: "45.6", ValueFull: "45.600000", Unit: "°C"},
	}
}

// vkmPeriodValue renders the period parameter's value.
//
// Option bit3 reads "выдавать максимальное количество знаков для значений
// с плавающей запятой и время в секундах". That trailing clause is
// ambiguous and the choice matters on the wire, so BOTH readings are
// implemented and selectable from vkm_config.txt (period_format), letting
// the alternative be tried on the live server without a rebuild:
//
//	period_format = datetime  (default) — timestamps to SECOND precision,
//	                                      i.e. "и время в секундах" means
//	                                      include the seconds field.
//	period_format = unix                — timestamps as Unix epoch seconds.
//
// "datetime" is the default because, read in context alongside "максимальное
// количество знаков", the clause most plausibly describes PRECISION rather
// than a change of epoch.
func vkmPeriodValue(start, end time.Time, timeLayout string, opts uint16) string {
	if loadVKMConfig().periodFormat == "unix" && opts&vkmOptMaxDigits != 0 {
		return fmt.Sprintf("%d-%d", start.Unix(), end.Unix())
	}
	return start.Format(timeLayout) + "-" + end.Format(timeLayout)
}

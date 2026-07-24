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
	Value  string // value as text
	Unit   string // unit text; appended WITHOUT a space (per the doc)
}

// BuildVKMArchiveString assembles an archive result string exactly as the
// ЭЛЕМЕР-ВКМ-360 register map specifies (p.7, "Чтение архивов"):
//
//	[тэг 1]{шапка}=<start> - <end>;[тэг 2]{шапка}=<значение><ед.изм.>;…[NUL]
//
// Three rules from that spec that our earlier hand-written test string all
// violated — and which are why Энергосфера read the string and rejected it:
//
//  1. The FIRST parameter is not data: it is the collection PERIOD, given
//     as two timestamps separated by "-". This is the archive record's
//     timestamp; without it the driver has no interval to file the data
//     under, so the ОИ debt never closes.
//  2. The unit follows the value with NO separating space.
//  3. The string terminates with a NUL byte (ANSI string convention), and
//     register 8002 counts that byte.
//
// The period is taken from the request the master actually wrote into
// 7902-7913, so the reply always describes the window that was asked for.
func BuildVKMArchiveString(start, end time.Time, timeLayout string, params []VKMParam) string {
	if timeLayout == "" {
		timeLayout = VKMDefaultTimeLayout
	}
	var b strings.Builder

	// Parameter 1 — the period. Tag/header are conventional; what matters
	// per the spec is that the value is "<time> - <time>".
	fmt.Fprintf(&b, "T{Период}=%s-%s;", start.Format(timeLayout), end.Format(timeLayout))

	for _, p := range params {
		fmt.Fprintf(&b, "%s{%s}=%s%s;", p.Tag, p.Header, p.Value, p.Unit)
	}

	b.WriteByte(0) // ANSI string terminator, counted in register 8002
	return b.String()
}

// VKMDefaultTimeLayout is the timestamp format used inside the archive
// string. The register map does not pin the exact layout ("две записи
// времени"), so this Russian-conventional form is the default and is
// overridable from vkm_config.txt (time_layout) — letting alternatives be
// tried on the live server without a rebuild.
const VKMDefaultTimeLayout = "02.01.2006 15:04:05"

// defaultVKMParams is the stand-in data payload used until a real ВКМ is
// available to read from: mass and temperature, the two values that
// historically came through to Энергосфера from the real device.
func defaultVKMParams() []VKMParam {
	return []VKMParam{
		{Tag: "M", Header: "Масса", Value: "678.90", Unit: "кг"},
		{Tag: "t", Header: "Температура", Value: "45.6", Unit: "°C"},
	}
}

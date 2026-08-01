package archive

import "strings"

// cp1251Decode converts Windows-1251 (Cyrillic ANSI) bytes to a Go (UTF-8)
// string. This is the reverse of internal/northbound's cp1251Encode (which
// exists for the outbound/simulator direction — encoding OUR strings to
// send to ЭС). We need the opposite direction here: the real device's
// archive result string (read via mb_request_poll_string) arrives as raw
// cp1251 bytes, and parseTaggedString needs readable UTF-8 to work with —
// CONFIRMED against real hardware (2026-08-01, tools/vkmprobe): the tag
// headers and unit strings came back as unrecognizable bytes ("?????" once
// naively treated as UTF-8), while the numeric/structural parts (which are
// plain ASCII: tag names like "Pi", "=", ";", digits) parsed fine — exactly
// the signature of an ANSI/cp1251 payload being misread as UTF-8.
//
// Uses the FULL standard cp1251 table (0x80-0xFF), not just the Cyrillic
// letter block — metrology unit strings routinely use characters from the
// 0xA0-0xBF range (° degree, ± plus-minus, µ micro, · middle dot, №
// numero) that a letters-only table would leave as unmapped garbage.
// Bytes with no assigned cp1251 meaning map to the Unicode replacement
// character rather than silently corrupting the string.
func cp1251Decode(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b))
	for _, c := range b {
		if c < 0x80 {
			sb.WriteByte(c)
			continue
		}
		sb.WriteRune(cp1251Table[c-0x80])
	}
	return sb.String()
}

// cp1251Table maps bytes 0x80-0xFF to their Unicode code points. Index 0
// corresponds to byte 0x80. Unassigned cp1251 positions use the Unicode
// replacement character (U+FFFD).
var cp1251Table = [128]rune{
	// 0x80-0x8F
	'\u0402', '\u0403', '\u201A', '\u0453', '\u201E', '\u2026', '\u2020', '\u2021',
	'\u20AC', '\u2030', '\u0409', '\u2039', '\u040A', '\u040C', '\u040B', '\u040F',
	// 0x90-0x9F
	'\u0452', '\u2018', '\u2019', '\u201C', '\u201D', '\u2022', '\u2013', '\u2014',
	'\uFFFD', '\u2122', '\u045A', '\u203A', '\u045C', '\u045E', '\u045F', '\uFFFD',
	// 0xA0-0xAF
	'\u00A0', '\u040E', '\u045E', '\u0408', '\u00A4', '\u0490', '\u00A6', '\u00A7',
	'\u0401', '\u00A9', '\u0404', '\u00AB', '\u00AC', '\u00AD', '\u00AE', '\u0407',
	// 0xB0-0xBF
	'\u00B0', '\u00B1', '\u0406', '\u0456', '\u0491', '\u00B5', '\u00B6', '\u00B7',
	'\u0451', '\u2116', '\u0454', '\u00BB', '\u0458', '\u0405', '\u0455', '\u0457',
	// 0xC0-0xCF (А-П)
	'\u0410', '\u0411', '\u0412', '\u0413', '\u0414', '\u0415', '\u0416', '\u0417',
	'\u0418', '\u0419', '\u041A', '\u041B', '\u041C', '\u041D', '\u041E', '\u041F',
	// 0xD0-0xDF (Р-Я)
	'\u0420', '\u0421', '\u0422', '\u0423', '\u0424', '\u0425', '\u0426', '\u0427',
	'\u0428', '\u0429', '\u042A', '\u042B', '\u042C', '\u042D', '\u042E', '\u042F',
	// 0xE0-0xEF (а-п)
	'\u0430', '\u0431', '\u0432', '\u0433', '\u0434', '\u0435', '\u0436', '\u0437',
	'\u0438', '\u0439', '\u043A', '\u043B', '\u043C', '\u043D', '\u043E', '\u043F',
	// 0xF0-0xFF (р-я)
	'\u0440', '\u0441', '\u0442', '\u0443', '\u0444', '\u0445', '\u0446', '\u0447',
	'\u0448', '\u0449', '\u044A', '\u044B', '\u044C', '\u044D', '\u044E', '\u044F',
}

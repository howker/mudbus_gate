package northbound

// cp1251Encode converts a Go (UTF-8) string to Windows-1251 bytes — the
// single-byte Cyrillic ANSI encoding old Windows software (like ПТК ЭКОМ /
// Энергосфера, and the УВП-280.01 protocol's "ANSI string" registers)
// expects on the wire, as opposed to Go's native UTF-8 (2 bytes per
// Cyrillic character).
//
// Found necessary by LIVE testing: sending the archive result string as
// raw UTF-8 bytes produced mojibake when decoded as cp1251 by the driver
// (vkm_live.jsonl, 24.07.2026) — the driver never showed any visible sign
// of rejecting it (it did read the string), but the content would be
// garbage without this conversion.
//
// Covers ASCII (unchanged) plus the standard Cyrillic block (А-Я, а-я,
// Ё/ё); characters outside that (there should be none in device archive
// strings) map to '?' rather than silently corrupting the byte stream.
func cp1251Encode(s string) []byte {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		switch {
		case r < 0x80:
			out = append(out, byte(r))
		case r == 0x401: // Ё
			out = append(out, 0xA8)
		case r == 0x451: // ё
			out = append(out, 0xB8)
		case r == 0xB0: // ° degree sign — cp1251 byte equals the Unicode
			// code point (0xB0), found missing live: "45.6°C" became
			// "45.6?C" in the archive string (vkm_live.jsonl, 24.07.2026).
			out = append(out, 0xB0)
		case r >= 0x410 && r <= 0x42F: // А-Я
			out = append(out, byte(r-0x410+0xC0))
		case r >= 0x430 && r <= 0x44F: // а-я
			out = append(out, byte(r-0x430+0xE0))
		default:
			out = append(out, '?')
		}
	}
	return out
}

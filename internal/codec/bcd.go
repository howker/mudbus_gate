package codec

import "fmt"

// DecodeBCDByte decodes a single byte holding two packed BCD decimal
// digits (high nibble = tens digit, low nibble = ones digit). This is the
// atomic BCD operation shared by every device seen so far that encodes
// time this way: Merkuriy's timedate field (dow-hh-mm-ss-dd-mon-yy, one
// BCD byte per component, CONTRACTS.md section 3) and Akron-01/02's
// real-time-clock registers (second/minute/hour/day_of_week/date/month/
// year-2000, table 2 of the manufacturer protocol document).
//
// Returns an error if either nibble is out of decimal range (0-9) - a
// byte like 0xAB is not valid BCD, and silently reinterpreting it (e.g.
// as raw hex 171, or masking off the invalid nibble) would produce a
// wrong-but-plausible-looking timestamp instead of a visible failure.
func DecodeBCDByte(b byte) (int, error) {
	hi := b >> 4
	lo := b & 0x0F
	if hi > 9 || lo > 9 {
		return 0, fmt.Errorf("invalid BCD byte 0x%02X: nibble out of decimal range (0-9)", b)
	}
	return int(hi)*10 + int(lo), nil
}

// EncodeBCDByte packs a decimal value 0-99 into a single BCD byte
// (high nibble = tens digit, low nibble = ones digit). This is needed for
// time correction (writing a corrected time back to a device) - every
// device's clock seen so far (Merkuriy, Akron) is BCD-encoded on the wire
// for both reading and writing.
func EncodeBCDByte(val int) (byte, error) {
	if val < 0 || val > 99 {
		return 0, fmt.Errorf("BCD value %d out of range (0-99)", val)
	}
	return byte((val/10)<<4 | (val % 10)), nil
}
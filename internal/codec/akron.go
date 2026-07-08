package codec

import (
	"fmt"
	"math"
)

// DecodeAkronVolume decodes Akron-01/02's composite volume encoding: a
// 4-byte signed raw counter value followed by a 1-byte scale exponent
// "Pu" (per "ВЗАИМОДЕЙСТВИЕ С КОНТРОЛЛЕРОМ СЕТИ MODBUS ПРИБОРОВ Акрон-01
// и Акрон-02-1", commands 102/104/105). The 4-byte raw value is
// little-endian on the wire like everything else on this device (order32
// applies to it); Pu is a single byte with no byte order.
//
// Scale formula: value = raw * 10^(Pu-3). Verified against the
// document's own worked example (command 102): raw=5827, Pu=0x02 ->
// 5827 * 10^(2-3) = 582.7 m3.
//
// NOTE on a documentation inconsistency: the document's field description
// for Pu says "lg(Ku)+3", but its own worked example computes the result
// using 10^(Pu-3) (minus, not plus) - and only the minus-3 formula
// reproduces the example's stated 582.7 result (verified numerically).
// This implementation follows the worked example (empirically confirmed
// correct), not the prose description (apparently a documentation typo).
func DecodeAkronVolume(data []byte, order32 string) (float64, error) {
	if len(data) < 5 {
		return 0, fmt.Errorf("akron volume requires 5 bytes, got %d", len(data))
	}
	raw, err := DecodeInt32(data[0:4], order32)
	if err != nil {
		return 0, err
	}
	pu := int(data[4])
	scale := math.Pow(10, float64(pu-3))
	return float64(raw) * scale, nil
}
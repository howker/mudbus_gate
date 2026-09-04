package interval

import "time"

// CounterDelta converts two consecutive counter snapshots into one
// interval value.
//
// A delta is valid only when snapshots are exactly one expectedStep
// apart and the cumulative counter did not decrease. This is the common
// rule used by both the archive UI and Energosphere export, so they
// cannot silently interpret the same counter differently.
func CounterDelta(prevTS time.Time, prevValue float64, currentTS time.Time, currentValue float64, expectedStep time.Duration) (float64, bool) {
	if currentTS.Sub(prevTS) != expectedStep {
		return 0, false
	}

	delta := currentValue - prevValue
	if delta < 0 {
		return 0, false
	}

	return delta, true
}

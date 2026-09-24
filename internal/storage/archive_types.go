package storage

import "time"

// HourlyArchiveRecord is one hourly archive point collected from a device
// and persisted locally, ready to be re-emitted upstream (e.g. to
// Энергосфера via the Akron command-104 carrier).
//
// It is intentionally device-agnostic. Channel/Param let the same table
// hold single-stream meters and, later, multichannel heat meters without
// a schema change:
//
//	Akron-01 (one pipe, volume): Channel="",  Param="V"
//	ВКМ-360 pipe 1:               Channel="",  Param="S"/"ST"/"T"/"Pi"
//	ВКМ-360 pipe 2..10:           Channel="2".."10", same param namespace
//
// TsHour is WALL-CLOCK time of the meter (the "16:00 17.07.2026" the meter
// itself stamps on the record), NOT a UTC-shifted instant. The upstream
// carrier encodes its calendar fields (Y/M/D/H) directly into the wire
// format, so it must be stored and read in the same (wall) frame end to
// end — otherwise the archive timestamp drifts and Энергосфера rejects the
// record (see the two archive-acceptance conditions in
// docs/M3_DISCOVERY_FINDINGS_akron.md).
//
// Value is the normalized quantity as the meter reported it (already
// unit-scaled, e.g. accumulated volume in m³). How that value is packed
// back into a specific carrier's wire format (Akron: U(4B)+Pu(1B)) is the
// carrier encoder's job, not this record's.
type HourlyArchiveRecord struct {
	DeviceID string
	Channel  string    // "" for single-channel devices (Akron)
	Param    string    // "V" (volume), "E" (energy), ... profile-defined
	TsHour   time.Time // start of the archived hour, meter wall-clock
	Value    float64   // normalized quantity as the meter reported it
	Unit     string
	Quality  string
}

// VKMRawRow is one stored ВКМ-360 archive period: the raw decoded string
// together with the period-end timestamp it belongs to. The name TsHour is
// kept for compatibility with archive_vkm_raw's historical column name even
// though VKM periods are currently 30 minutes.
type VKMRawRow struct {
	TsHour    time.Time
	RawString string
}

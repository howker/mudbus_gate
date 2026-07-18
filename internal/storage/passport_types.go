package storage

import "time"

// DevicePassport is a device's static identity ("паспорт"): the values a
// meter reports once via its identification command and that never change
// afterward — factory serial, device type, firmware version. mbgw reads
// this once at startup and stores it so the upstream carrier can answer an
// identification request (Akron command 101) with the real device's
// identity instead of a stub.
//
// This is deliberately a separate table from readings_current: those are
// time-stamped measurements with quality/units, whereas a passport is a
// single timeless record per device. Forcing it through the measurements
// table would leave the ts/quality/unit columns meaningless.
type DevicePassport struct {
	DeviceID   string
	Serial     uint32    // factory serial number
	DeviceType byte      // vendor device-type code (Akron-01: 0x00)
	Firmware   string    // human firmware version, e.g. "3.7"
	UpdatedAt  time.Time // when this passport was last read from the device
}

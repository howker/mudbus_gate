package device

import (
	"context"
	"log"
	"time"

	"mbgw/internal/protocol/akron"
	"mbgw/internal/storage"
)

// isAkron reports whether this device speaks the Akron User-Defined command
// set, detected structurally by the presence of an akron_archive strategy
// in its profile. This is the gate that keeps command 101 from ever being
// sent to a non-Akron device (Merkuriy, VKM, …), which would get a garbage
// or error reply.
func (d *Device) isAkron() bool {
	for _, a := range d.Profile.Archives {
		if a.Strategy == "akron_archive" {
			return true
		}
	}
	return false
}

// DetectPassport reads the device's static identity (Akron command 101)
// once and stores it, so the upstream carrier can later answer command 101
// with the real serial / type / firmware instead of a stub.
//
// It must be called once at startup from the device-build loop in
// cmd/mbgw/run.go — NOT from Device.Start, because the production poll path
// (internal/poller) drives Poll/PollArchives directly and never calls
// Start (the same reason detectFirmwareVariant currently never runs in
// production; noted for backlog).
//
// Non-fatal by design: a device that isn't an Akron is skipped; one that
// doesn't answer 101 or returns an unparseable reply simply leaves no
// passport, logged and ignored. What the upstream responder does when no
// passport exists is decided in M4.3.
func (d *Device) DetectPassport(ctx context.Context) {
	if !d.isAkron() {
		return
	}

	req := akron.BuildIdentificationPDU()
	respPDU, err := d.Client.Transact(ctx, req)
	if err != nil {
		log.Printf("[%s] команда 101 (идентификация) недоступна: %v\n", d.ID, err)
		return
	}

	id, err := akron.ParseIdentification(respPDU)
	if err != nil {
		log.Printf("[%s] ошибка разбора ответа 101: %v\n", d.ID, err)
		return
	}

	if err := d.Repo.SaveDevicePassport(ctx, storage.DevicePassport{
		DeviceID:   d.ID,
		Serial:     id.Serial,
		DeviceType: id.DeviceType,
		Firmware:   id.Firmware,
		UpdatedAt:  time.Now(),
	}); err != nil {
		log.Printf("[%s] ошибка сохранения паспорта: %v\n", d.ID, err)
		return
	}

	log.Printf("[%s] паспорт прибора: тип=0x%02X версия=%s заводской №=%d\n",
		d.ID, id.DeviceType, id.Firmware, id.Serial)
}

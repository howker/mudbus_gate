package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"mbgw/internal/codec"
	"mbgw/internal/pollcore"
	"mbgw/internal/protocol/akron"
	"mbgw/internal/transport"
)

// api_probe.go implements POST /api/devices/probe: a ONE-SHOT live poll
// of a device using transport parameters the operator just typed into
// the "add device" form — BEFORE saving anything — so they see "yes,
// this is a real Акрон-01, serial 12345, its clock reads 14:32" (or a
// clear connection error) instead of finding out only after saving and
// waiting for the next scheduled poll.
//
// This does NOT go through internal/scheduler or internal/poller (no
// device.Device, no lease, no repeated polling) — it opens a transport,
// sends exactly the identification + clock requests, and closes
// immediately. Safe to call repeatedly from a form without registering
// anything.
//
// AKRON: fully implemented — command 101 (identification) + command 03
// (clock, registers 0x10-0x13), the exact protocol confirmed live against
// both the real device and Энергосфера's own driver (see
// docs/M3_DISCOVERY_FINDINGS_akron.md, internal/protocol/akron/akron.go).
//
// VKM360: NOT implemented here. There is no existing, tested code path in
// this project that reads a ВКМ-360 identification/serial number in
// isolation — SaveDevicePassport/GetDevicePassport exist on the Repo
// interface but are never actually called anywhere in the codebase today
// (confirmed by a project-wide search, 2026-08-22), and the ВКМ archive-
// string protocol itself still has an open, undocumented detail per
// docs/SESSION_STATUS_northbound.md ("единственная оставшаяся
// неизвестность: точный словарь тэгов и формат значения"). Writing a
// probe implementation now would mean guessing at a protocol detail this
// project has explicitly flagged as unresolved — exactly the kind of
// guess that cost a full day on a different device earlier in this same
// project. handleDeviceProbe returns a clear "not supported" response for
// kind=vkm360 instead.

type probeRequest struct {
	Kind          string `json:"kind"` // "vkm360" | "akron"
	TransportKind string `json:"transport_kind"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	COM           string `json:"com"`
	Baudrate      int    `json:"baudrate"`
	Parity        string `json:"parity"`
	StopBits      int    `json:"stopbits"`
	TimeoutMs     int    `json:"timeout_ms"`
	UnitID        int    `json:"unit_id"`
}

type probeResponse struct {
	OK           bool   `json:"ok"`
	Error        string `json:"error,omitempty"`
	SerialNumber string `json:"serial_number,omitempty"`
	FirmwareInfo string `json:"firmware_info,omitempty"`
	DeviceTime   string `json:"device_time,omitempty"`
}

func (s *Server) handleDeviceProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "используйте POST")
		return
	}
	var req probeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "некорректный JSON: "+err.Error())
		return
	}

	switch req.Kind {
	case "akron":
		writeJSON(w, http.StatusOK, probeAkron(r.Context(), req))
	case "vkm360":
		writeJSON(w, http.StatusOK, probeResponse{
			OK:    false,
			Error: "Проверка ВКМ-360 пока не реализована — используйте пробный опрос через обычный цикл сбора после сохранения прибора.",
		})
	default:
		writeError(w, http.StatusBadRequest, `поле kind должно быть "vkm360" или "akron"`)
	}
}

// probeAkron opens a transport, sends command 101 (identification) and
// command 03 (clock registers 0x10-0x13), and returns what the device
// reported. Any failure at any step (transport open, transact, parse) is
// reported as OK:false with a human-readable error — never a panic or a
// 500, since a wrong COM port / unreachable IP typed into a form is an
// entirely expected outcome here, not a server bug.
func probeAkron(ctx context.Context, req probeRequest) probeResponse {
	timeoutMs := req.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = 1000
	}

	tr, err := transport.New(transport.Params{
		Kind:            transport.Kind(req.TransportKind),
		Host:            req.Host,
		Port:            req.Port,
		COM:             req.COM,
		Baudrate:        req.Baudrate,
		Parity:          req.Parity,
		StopBits:        req.StopBits,
		ResponseTimeout: time.Duration(timeoutMs) * time.Millisecond,
		// Explicit, not left at Go's zero value: protocol/modbus.Transact
		// reads maxAttempts from this field directly (see core.go) — an
		// unset Retries could mean "zero attempts" depending on that
		// loop's exact semantics, silently breaking every probe
		// regardless of whether the device is actually reachable. A
		// one-shot diagnostic probe doesn't need the operator's real
		// configured retry count (that's for steady-state polling); a
		// small fixed value here is simply "give it a fair chance to
		// respond," not a setting worth exposing in the probe UI.
		Retries: 3,
	})
	if err != nil {
		return probeResponse{OK: false, Error: "не удалось создать транспорт: " + err.Error()}
	}

	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := tr.Open(probeCtx); err != nil {
		return probeResponse{OK: false, Error: "не удалось открыть соединение: " + err.Error()}
	}
	defer tr.Close()

	isTCP := req.TransportKind == "modbus_tcp"
	unitID := req.UnitID
	if unitID == 0 {
		unitID = 1
	}
	reader := pollcore.New(tr, isTCP, uint8(unitID))

	resp := probeResponse{OK: true}

	// Command 101 — identification (type, firmware BCD, serial LE).
	idPDU := akron.BuildIdentificationPDU()
	idResp, err := reader.Transact(probeCtx, idPDU)
	if err != nil {
		return probeResponse{OK: false, Error: "идентификация (команда 101): " + err.Error()}
	}
	_, idData, err := akron.ParseResponsePDU(idResp)
	if err != nil {
		return probeResponse{OK: false, Error: "разбор ответа идентификации: " + err.Error()}
	}
	if len(idData) >= 6 {
		devType := idData[0]
		fwBCD := idData[1]
		serial := uint32(idData[2]) | uint32(idData[3])<<8 | uint32(idData[4])<<16 | uint32(idData[5])<<24
		fwMajor, err1 := codec.DecodeBCDByte(fwBCD)
		fwStr := fmt.Sprintf("тип=%d, версия(сырое BCD)=0x%02X", devType, fwBCD)
		if err1 == nil {
			fwStr = fmt.Sprintf("тип=%d, версия=%d", devType, fwMajor)
		}
		resp.FirmwareInfo = fwStr
		resp.SerialNumber = fmt.Sprintf("%d", serial)
	}

	// Command 03 — clock, registers 0x10-0x13 (8 bytes BCD: sec,min /
	// hour,dow / day,month / year,id_am). Bare PDU here matches how
	// device.go's decodePoint reads the same registers point-by-point —
	// this probe reads all 4 registers in one request instead, since it
	// only needs to display the time, not decode it field-by-field into
	// separate profile Points.
	clockPDU := []byte{0x03, 0x00, 0x10, 0x00, 0x04}
	clockResp, err := reader.Transact(probeCtx, clockPDU)
	if err != nil {
		// Identification succeeded but clock read failed — still report
		// what we got rather than discarding it as a total failure.
		resp.Error = "часы прибора (команда 03): " + err.Error()
		return resp
	}
	if len(clockResp) >= 10 {
		data := clockResp[2:10] // skip func code + byte count
		sec, e1 := codec.DecodeBCDByte(data[0])
		min, e2 := codec.DecodeBCDByte(data[1])
		hour, e3 := codec.DecodeBCDByte(data[2])
		day, e4 := codec.DecodeBCDByte(data[4])
		month, e5 := codec.DecodeBCDByte(data[5])
		year, e6 := codec.DecodeBCDByte(data[6])
		if e1 == nil && e2 == nil && e3 == nil && e4 == nil && e5 == nil && e6 == nil {
			resp.DeviceTime = fmt.Sprintf("%02d.%02d.20%02d %02d:%02d:%02d", day, month, year, hour, min, sec)
		} else {
			resp.DeviceTime = "не удалось разобрать BCD-время"
		}
	}

	return resp
}

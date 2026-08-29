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
	"mbgw/internal/session"
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
// VKM360: ДОБАВЛЕНО (2026-08-29, прямой запрос оператора). Раньше здесь
// был отказ "не реализовано" — единственная причина была в том, что
// точный словарь тэгов АРХИВНОЙ строки (mb_request_poll_string) на тот
// момент был не подтверждён (см. историю в docs/SESSION_STATUS_
// northbound.md). Но пробник не обязан читать архив вообще — он читает
// ПРОСТЫЕ регистры (серийник, версия ПО, часы, мгновенные показания),
// формат которых давно подтверждён живьём (см. tools/vkmprobe и
// registri_mbrrtu_vkm.pdf) и никак не завязан на тот самый неразрешённый
// вопрос про архивную строку. probeVKM ниже — по сути урезанная копия
// tools/vkmprobe's readCurrentValues + прямые чтения 1800-1810HR, без
// самого архива.

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

	// Поля ниже заполняются ТОЛЬКО probeVKM (у Akron нет прямого
	// Modbus-аналога этих мгновенных показаний в самом пробнике —
	// он читает их из архивной команды 102, не отдельными регистрами).
	// Не обязательные (omitempty) — пустая строка означает "этот
	// конкретный регистр не прочитался", а не ошибку всего пробника
	// целиком (частичный успех допустим и полезен оператору).
	Pressure    string `json:"pressure,omitempty"`
	Temperature string `json:"temperature,omitempty"`
	MassFlow    string `json:"mass_flow,omitempty"`
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
		writeJSON(w, http.StatusOK, probeVKM(r.Context(), req))
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

// probeVKM opens a transport, performs the ВКМ session handshake
// (byte-order detection + authorization — required for this device,
// unlike Akron's session "none", see profiles/vkm360.yaml's session
// type "modbus_byteorder_auth"), then reads a handful of plain
// registers: serial number + firmware version (identification-
// equivalent), the device's own clock (1800-1805HR, read-only per
// registri_mbrrtu_vkm.pdf), and three instantaneous readings on pipe 1
// (pressure/temperature/mass flow) as a live proof-of-life beyond just
// the clock. Deliberately does NOT touch the archive-string dance
// (registers 7900-9999) — that's tools/vkmprobe's job for deep protocol
// diagnostics, not a quick "is this really a ВКМ-360 and is it alive"
// check from the add-device form.
//
// Partial success is reported as-is (OK:true with only some fields
// filled) rather than an all-or-nothing failure — a device that answers
// the clock but not, say, the mass-flow register on an unconfigured
// pipe is still clearly "a real, reachable ВКМ-360", which is the
// question this probe answers for the operator.
func probeVKM(ctx context.Context, req probeRequest) probeResponse {
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
		Retries:         3, // см. комментарий у Retries в probeAkron выше — тот же расчёт
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

	sess, err := session.New("modbus_byteorder_auth")
	if err != nil {
		return probeResponse{OK: false, Error: "создание сессии: " + err.Error()}
	}
	if err := sess.Open(probeCtx, tr); err != nil {
		return probeResponse{OK: false, Error: "открытие сессии (byte-order/авторизация): " + err.Error()}
	}

	isTCP := req.TransportKind == "modbus_tcp"
	unitID := req.UnitID
	if unitID == 0 {
		unitID = 1
	}
	reader := pollcore.New(tr, isTCP, uint8(unitID))

	resp := probeResponse{OK: true}

	// ИЗМЕНЕНО (2026-08-29, проверено живьём через tools/vkmtimeprobe на
	// boylernaya_par): здесь раньше были попытки прочитать серийный номер
	// (1810HR), версию ПО (1807HR) и часы прибора (1800-1805HR) — все три
	// взяты из документации ЭЛЕМЕР (registri_mbrrtu_vkm.pdf) как "должны
	// быть реализованы", но НИ ОДИН из них не ответил на реальном приборе
	// (таймаут, не ошибка Modbus — устройство просто не отвечает на эти
	// адреса вообще). Убраны совсем, а не оставлены "на всякий случай" —
	// иначе КАЖДАЯ проверка прибора ждала бы retries×backoff по трём
	// заведомо мёртвым регистрам, прежде чем дойти до полезных данных.
	// Если для другого экземпляра/прошивки ВКМ-360 этот блок всё же
	// работает — его стоит вернуть, но уже как проверенный факт для
	// конкретного прибора, не как общее предположение по документации.
	//
	// Мгновенные показания на трубопроводе №1 — единственное, что
	// реально проверено. Адреса взяты НЕ из чужой документации, а прямо
	// из profiles/vkm360.yaml (тот же профиль, что использует боевой
	// цикл опроса этого самого прибора): IR 2000 (Избыточное давление),
	// IR 2004 (Температура), IR 2000+(pipe-1)*100+8 = IR 2008 при pipe=1
	// (Массовый расход). Форма добавления прибора пока не собирает номер
	// трубы отдельно (это делается позже, на вкладке каналов ЭС), так
	// что пробник намеренно всегда проверяет трубу №1 — просто как живой
	// признак того, что прибор действительно отдаёт измерения.
	if data, err := reader.ReadRaw(probeCtx, "IR", 2000, "float"); err != nil {
		resp.Error = firstNonEmpty(resp.Error, "давление (IR 2000): "+err.Error())
	} else if v, err := codec.DecodeFloat32(data, "0123"); err == nil {
		resp.Pressure = fmt.Sprintf("%.0f Па", v)
	}
	if data, err := reader.ReadRaw(probeCtx, "IR", 2004, "float"); err != nil {
		resp.Error = firstNonEmpty(resp.Error, "температура (IR 2004): "+err.Error())
	} else if v, err := codec.DecodeFloat32(data, "0123"); err == nil {
		resp.Temperature = fmt.Sprintf("%.2f °C", v)
	}
	if data, err := reader.ReadRaw(probeCtx, "IR", 2008, "float"); err != nil {
		resp.Error = firstNonEmpty(resp.Error, "массовый расход (IR 2008): "+err.Error())
	} else if v, err := codec.DecodeFloat32(data, "0123"); err == nil {
		resp.MassFlow = fmt.Sprintf("%.4f кг/с", v)
	}

	// Если не прочиталось ВООБЩЕ ничего — сессия открылась, но за этим
	// явно стоит неверный unit id, незасинхронизированная труба или
	// нестандартная прошивка, а не частичный успех. Сообщаем как отказ,
	// а не как "успех" с полностью пустым ответом.
	//
	// ИСПРАВЛЕНО (2026-08-29): раньше эта проверка смотрела на
	// SerialNumber/DeviceTime — поля, которые probeVKM теперь вообще не
	// заполняет (см. выше), так что она бы ВСЕГДА считала результат
	// неуспешным, даже если все три мгновенных значения прочитались
	// нормально. Теперь проверяет именно те поля, которые эта функция
	// реально может заполнить.
	if resp.Pressure == "" && resp.Temperature == "" && resp.MassFlow == "" {
		resp.OK = false
		resp.Error = firstNonEmpty(resp.Error, "сессия открыта, но ни один регистр не прочитался — проверьте unit id")
	}

	return resp
}

// firstNonEmpty returns a if it's non-empty, otherwise b — used above to
// keep the FIRST error encountered while probing several independent
// registers, instead of the LAST one silently overwriting it.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

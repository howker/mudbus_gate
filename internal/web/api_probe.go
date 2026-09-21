package web

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"mbgw/internal/codec"
	"mbgw/internal/pollcore"
	"mbgw/internal/protocol/akron"
	"mbgw/internal/protocol/modbus"
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
	ID            string `json:"id"`
	Kind          string `json:"kind"` // "vkm360" | "akron" | "ivk-ter"
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

	// Поля ниже — дополнительные мгновенные значения. ВКМ заполняет три
	// своих поля, ИВК-ТЭР — CurrentFlow; у Akron нет прямого
	// Modbus-аналога этих мгновенных показаний в самом пробнике —
	// он читает их из архивной команды 102, не отдельными регистрами).
	// Не обязательные (omitempty) — пустая строка означает "этот
	// конкретный регистр не прочитался", а не ошибку всего пробника
	// целиком (частичный успех допустим и полезен оператору).
	Pressure    string   `json:"pressure,omitempty"`
	Temperature string   `json:"temperature,omitempty"`
	MassFlow    string   `json:"mass_flow,omitempty"`
	CurrentFlow string   `json:"current_flow,omitempty"`
	ArchiveInfo string   `json:"archive_info,omitempty"`
	Steps       []string `json:"steps,omitempty"`
}

// lockProbePhysicalChannel reserves the SAME physical bus lock used by the
// production Readers. The lock is acquired before tr.Open, which is critical:
// otherwise two configured devices (or a UI probe and the service) can both
// race to open one COM port before transaction-level locking even begins.
//
// Probe code holds this outer lock for its whole diagnostic sequence and uses
// pollcore.New (private reader lock) inside it, avoiding recursive locking.
func lockProbePhysicalChannel(params transport.Params, deviceID string) func() {
	return pollcore.LockKey(pollcore.PhysicalIOLockKey(params, deviceID))
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
	case "akron", "vkm360", "ivk-ter":
		// допустимый тип; ниже запускается физическая операция
	default:
		writeError(w, http.StatusBadRequest, `поле kind должно быть "vkm360", "akron" или "ivk-ter"`)
		return
	}

	if !s.beginPhysicalOperation() {
		writeError(w, http.StatusServiceUnavailable, "МодбасШлюз останавливается — проверка прибора не запускается")
		return
	}
	defer s.endPhysicalOperation()

	switch req.Kind {
	case "akron":
		writeJSON(w, http.StatusOK, probeAkron(r.Context(), req))
	case "vkm360":
		writeJSON(w, http.StatusOK, probeVKM(r.Context(), req))
	case "ivk-ter":
		writeJSON(w, http.StatusOK, probeIVKTER(r.Context(), req))
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

	trParams := transport.Params{
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
	}
	tr, err := transport.New(trParams)
	if err != nil {
		return probeResponse{OK: false, Error: "не удалось создать транспорт: " + err.Error()}
	}

	unlockIO := lockProbePhysicalChannel(trParams, req.ID)
	defer unlockIO()

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
// registers: the device's own clock (1800-1805HR, live-confirmed
// 2026-09-05) and three instantaneous readings on pipe 1
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

	trParams := transport.Params{
		Kind:            transport.Kind(req.TransportKind),
		Host:            req.Host,
		Port:            req.Port,
		COM:             req.COM,
		Baudrate:        req.Baudrate,
		Parity:          req.Parity,
		StopBits:        req.StopBits,
		ResponseTimeout: time.Duration(timeoutMs) * time.Millisecond,
		Retries:         3,
	}
	tr, err := transport.New(trParams)
	if err != nil {
		return probeResponse{OK: false, Error: "не удалось создать транспорт: " + err.Error()}
	}

	// Держим bus-lock на ВСЮ диагностическую операцию: Open -> session
	// handshake -> чтения -> Close. Так probe ждёт текущий обмен службы,
	// а не получает ложное "порт занят".
	unlockIO := lockProbePhysicalChannel(trParams, req.ID)
	defer unlockIO()

	probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	if err := tr.Open(probeCtx); err != nil {
		return probeResponse{OK: false, Error: "не удалось открыть соединение: " + err.Error()}
	}
	defer tr.Close()

	sess, err := session.New("modbus_byteorder_auth")
	if err != nil {
		return probeResponse{OK: false, Error: "создание сессии: " + err.Error()}
	}
	if err = sess.Open(probeCtx, tr); err != nil {
		return probeResponse{OK: false, Error: "открытие сессии (byte-order/авторизация): " + err.Error()}
	}

	isTCP := req.TransportKind == "modbus_tcp"
	unitID := req.UnitID
	if unitID == 0 {
		unitID = 1
	}
	reader := pollcore.New(tr, isTCP, uint8(unitID))

	resp := probeResponse{OK: true}

	// ТЕКУЩИЕ ЧАСЫ ВКМ-360 — подтверждено живьём 2026-09-05.
	// Holding registers 1800..1805 (decimal) = day, month, year,
	// hour, minute, second. Читаем все 6 регистров одной функцией 03,
	// тем же способом, что боевой код device.readVKMClock.
	clockResp, clockErr := reader.Transact(probeCtx, []byte{0x03, 0x07, 0x08, 0x00, 0x06})
	if clockErr != nil {
		resp.Error = firstNonEmpty(resp.Error, "время прибора (HR1800..1805): "+clockErr.Error())
	} else if deviceTime, err := decodeProbeVKMClockResponse(clockResp, time.Local); err != nil {
		resp.Error = firstNonEmpty(resp.Error, "разбор времени прибора: "+err.Error())
	} else {
		resp.DeviceTime = deviceTime.Format("02.01.2006 15:04:05")
	}

	// Заводской номер ВКМ здесь намеренно НЕ читаем. Ранее пробовали
	// документированный HR1810, но на живом boylernaya_par прибор на этот
	// адрес не отвечал. Без подтверждённого регистра/команды нельзя
	// показывать оператору случайное значение как заводской номер.
	//
	// Мгновенные показания трубопровода №1 остаются дополнительным
	// proof-of-life: это те же подтверждённые адреса, что использует
	// профиль боевого опроса.
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

	// Если не прочиталось вообще ничего полезного, это уже не частичный
	// успех. Время теперь тоже считается полноценным подтверждением
	// живого ВКМ, даже если конкретные измерительные каналы трубы №1
	// не настроены.
	if resp.DeviceTime == "" && resp.Pressure == "" && resp.Temperature == "" && resp.MassFlow == "" {
		resp.OK = false
		resp.Error = firstNonEmpty(resp.Error, "сессия открыта, но ни часы, ни измерительные регистры не прочитались — проверьте unit id")
	}

	return resp
}

// probeIVKTER выполняет безопасную пошаговую проверку ИВК-ТЭР без
// записи в прибор. Диагностика намеренно разделена на независимые этапы:
// открытие транспорта -> IR 0x8002 -> IR 0x8000 -> архивная функция 65 ->
// IR 0xC034 -> закрытие транспорта. Каждый этап попадает в Steps со своим
// OK/ERROR и, где полезно, сырыми байтами. Для архивного шлюза общий OK
// означает именно успешное чтение полноценной 30-байтовой записи F65;
// ответы обычных IR-регистров не маскируют неработающий архив.
func probeIVKTER(ctx context.Context, req probeRequest) (resp probeResponse) {
	timeoutMs := req.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = 1000
	}
	trParams := transport.Params{
		Kind: transport.Kind(req.TransportKind), Host: req.Host, Port: req.Port, COM: req.COM,
		Baudrate: req.Baudrate, Parity: req.Parity, StopBits: req.StopBits,
		ResponseTimeout: time.Duration(timeoutMs) * time.Millisecond, Retries: 3,
	}

	transportLabel := req.COM
	if req.TransportKind == "modbus_tcp" {
		transportLabel = fmt.Sprintf("%s:%d", req.Host, req.Port)
	}
	if transportLabel == "" {
		transportLabel = "транспорт"
	}

	appendError := func(detail string) {
		if detail == "" {
			return
		}
		if resp.Error == "" {
			resp.Error = detail
			return
		}
		resp.Error += "; " + detail
	}
	probeStepOK := func(text string) {
		resp.Steps = append(resp.Steps, text+" — OK")
	}
	probeStepError := func(text string, err error) {
		detail := "неизвестная ошибка"
		if err != nil {
			detail = err.Error()
		}
		resp.Steps = append(resp.Steps, text+" — ERROR: "+detail)
		appendError(text + ": " + detail)
	}
	probeStepErrorText := func(text, detail string) {
		resp.Steps = append(resp.Steps, text+" — ERROR: "+detail)
		appendError(text + ": " + detail)
	}

	resp.Steps = append(resp.Steps, "создаю транспорт")
	tr, err := transport.New(trParams)
	if err != nil {
		probeStepError("создание транспорта", err)
		return
	}
	probeStepOK("транспорт создан")

	// Четыре независимых диагностических запроса могут каждый исчерпать
	// несколько transport retries. Сначала занимаем тот же physical-bus lock,
	// что использует служба, и только ПОТОМ открываем порт. Поэтому проверка
	// прибора на общем COM ждёт завершения текущего обмена вместо "порт занят".
	unlockIO := lockProbePhysicalChannel(trParams, req.ID)
	defer unlockIO()

	// Общий budget специально больше обычного probe, чтобы первый timeout
	// не лишал функцию 65 своей попытки.
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := tr.Open(probeCtx); err != nil {
		probeStepError("открытие "+transportLabel, err)
		return
	}
	probeStepOK(transportLabel + " открыт")
	transportClosed := false
	closeTransport := func() {
		if transportClosed {
			return
		}
		transportClosed = true
		if err := tr.Close(); err != nil {
			probeStepError("закрытие "+transportLabel, err)
			resp.OK = false
		} else {
			probeStepOK(transportLabel + " закрыт")
		}
	}
	defer closeTransport()

	isTCP := req.TransportKind == "modbus_tcp"
	unitID := req.UnitID
	if unitID == 0 {
		unitID = 1
	}
	reader := pollcore.New(tr, isTCP, uint8(unitID))
	resp.Steps = append(resp.Steps, fmt.Sprintf("начат опрос ИВК-ТЭР, Modbus-адрес %d", unitID))

	// Каждый этап получает собственный timeout: ошибка одного регистра не
	// должна съедать весь budget и скрывать результат главной проверки F65.
	stageTimeout := 4 * time.Second
	if candidate := time.Duration(timeoutMs)*time.Millisecond*4 + 500*time.Millisecond; candidate > stageTimeout {
		stageTimeout = candidate
	}
	if stageTimeout > 8*time.Second {
		stageTimeout = 8 * time.Second
	}

	stageCtx, stageCancel := context.WithTimeout(probeCtx, stageTimeout)
	rawSerial, serialErr := reader.ReadRaw(stageCtx, "IR", 32770, "uint32")
	stageCancel()
	if serialErr != nil {
		probeStepError("IR 0x8002 serial", serialErr)
	} else if serial, err := codec.DecodeUint32(rawSerial, "0123"); err != nil {
		probeStepError("IR 0x8002 serial decode", err)
	} else if serial == 0 || serial == 0xFFFFFFFF {
		probeStepErrorText("IR 0x8002 serial", fmt.Sprintf("некорректное значение %d, raw=% X", serial, rawSerial))
	} else {
		resp.SerialNumber = fmt.Sprintf("%d", serial)
		probeStepOK(fmt.Sprintf("IR 0x8002 serial = %d, raw=% X", serial, rawSerial))
	}

	stageCtx, stageCancel = context.WithTimeout(probeCtx, stageTimeout)
	rawTime, timeErr := reader.ReadRaw(stageCtx, "IR", 32768, "uint32")
	stageCancel()
	if timeErr != nil {
		probeStepError("IR 0x8000 time", timeErr)
	} else if seconds, err := codec.DecodeUint32(rawTime, "0123"); err != nil {
		probeStepError("IR 0x8000 time decode", err)
	} else if seconds == 0 || seconds == 0xFFFFFFFF {
		probeStepErrorText("IR 0x8000 time", fmt.Sprintf("некорректное значение %d, raw=% X", seconds, rawTime))
	} else {
		resp.DeviceTime = time.Unix(int64(seconds), 0).Local().Format("02.01.2006 15:04:05")
		probeStepOK(fmt.Sprintf("IR 0x8000 time = %s, raw=% X", resp.DeviceTime, rawTime))
	}

	// Ключевой этап именно для архивного шлюза. Общий OK для ИВК-ТЭР
	// выставляется только когда функция 65 вернула полноценную 30-байтовую
	// запись с разумной меткой времени. Простые IR-регистры сами по себе
	// больше не маскируют неработающий архив зелёным результатом.
	archiveOK := false
	archivePDU := modbus.BuildArchive65IndexPDU(0, 1, 0)
	resp.Steps = append(resp.Steps, fmt.Sprintf("F65 запрос: % X", archivePDU))
	stageCtx, stageCancel = context.WithTimeout(probeCtx, stageTimeout)
	archiveResp, archiveErr := reader.Transact(stageCtx, archivePDU)
	stageCancel()
	if archiveErr != nil {
		probeStepError("F65 часовой архив", archiveErr)
	} else if payload, err := modbus.ParseArchive65Response(archiveResp); err != nil {
		probeStepErrorText("F65 разбор ответа", fmt.Sprintf("%v; raw response=% X", err, archiveResp))
	} else if len(payload) < 30 {
		probeStepErrorText("F65 часовой архив", fmt.Sprintf("короткая запись %d байт, ожидается 30; raw response=% X", len(payload), archiveResp))
	} else {
		ts := binary.BigEndian.Uint32(payload[:4])
		if ts == 0 || ts == 0xFFFFFFFF {
			probeStepErrorText("F65 часовой архив", fmt.Sprintf("маркер отсутствующей записи 0x%08X; raw response=% X", ts, archiveResp))
		} else {
			archiveTime := time.Unix(int64(ts), 0).Local().Format("02.01.2006 15:04:05")
			resp.ArchiveInfo = "функция 65 отвечает, последняя запись: " + archiveTime
			probeStepOK(fmt.Sprintf("F65 часовой архив = %s, payload=% X", archiveTime, payload[:30]))
			archiveOK = true
		}
	}

	// Текущий расход — только дополнительный proof-of-life. Его ошибка
	// видна отдельно, но не отменяет успешную архивную функцию 65.
	stageCtx, stageCancel = context.WithTimeout(probeCtx, stageTimeout)
	rawFlow, flowErr := reader.ReadRaw(stageCtx, "IR", 49204, "float")
	stageCancel()
	if flowErr != nil {
		probeStepError("IR 0xC034 current flow", flowErr)
	} else if flow, err := codec.DecodeFloat32(rawFlow, "0123"); err != nil {
		probeStepError("IR 0xC034 current flow decode", err)
	} else {
		resp.CurrentFlow = fmt.Sprintf("%.3f л/мин", flow)
		probeStepOK(fmt.Sprintf("IR 0xC034 current flow = %s, raw=% X", resp.CurrentFlow, rawFlow))
	}

	resp.OK = archiveOK
	closeTransport()
	if !archiveOK {
		appendError("КРИТИЧЕСКИЙ ИТОГ: часовой архив ИВК-ТЭР функцией 65 не прочитан")
		resp.Steps = append(resp.Steps, "ИТОГ — ERROR: функция 65 не подтверждена")
	} else if !resp.OK {
		resp.Steps = append(resp.Steps, "ИТОГ — ERROR: архив прочитан, но транспорт не удалось штатно закрыть")
	} else if resp.Error != "" {
		resp.Steps = append(resp.Steps, "ИТОГ — OK: архив читается; есть дополнительные диагностические замечания выше")
	} else {
		resp.Steps = append(resp.Steps, "ИТОГ — OK: ИВК-ТЭР и часовой архив функции 65 подтверждены")
	}
	return
}

func decodeProbeVKMClockResponse(resp []byte, loc *time.Location) (time.Time, error) {
	if len(resp) < 14 {
		return time.Time{}, fmt.Errorf("короткий ответ: %d байт, нужно минимум 14", len(resp))
	}
	if resp[0] != 0x03 {
		return time.Time{}, fmt.Errorf("неожиданная функция 0x%02X вместо 0x03", resp[0])
	}
	if resp[1] < 12 {
		return time.Time{}, fmt.Errorf("byte count=%d, нужно минимум 12", resp[1])
	}

	data := resp[2:14]
	reg := func(i int) int {
		return int(data[i*2])<<8 | int(data[i*2+1])
	}

	day := reg(0)
	month := reg(1)
	year := reg(2)
	hour := reg(3)
	minute := reg(4)
	second := reg(5)

	if second > 59 || minute > 59 || hour > 23 || month < 1 || month > 12 || day < 1 || day > 31 {
		return time.Time{}, fmt.Errorf(
			"время вне диапазона: %02d.%02d.%02d %02d:%02d:%02d",
			day, month, year, hour, minute, second,
		)
	}
	if year < 0 || year > 99 {
		return time.Time{}, fmt.Errorf("год вне диапазона 0..99: %d", year)
	}

	fullYear := 2000 + year
	t := time.Date(fullYear, time.Month(month), day, hour, minute, second, 0, loc)
	if t.Year() != fullYear || int(t.Month()) != month || t.Day() != day {
		return time.Time{}, fmt.Errorf(
			"некорректная календарная дата: %02d.%02d.%04d",
			day, month, fullYear,
		)
	}

	return t, nil
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

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
	reader := pollcore.NewWithLockKey(tr, isTCP, uint8(unitID), pollcore.PhysicalIOLockKey(trParams, req.ID))

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

	probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	if err := tr.Open(probeCtx); err != nil {
		return probeResponse{OK: false, Error: "не удалось открыть соединение: " + err.Error()}
	}
	defer tr.Close()

	// Используем тот же ключ физического канала, что и боевой poller.
	// Это особенно важно для tcp_serial/общего конвертера: ручная
	// проверка из UI не должна врезаться в текущий обмен другого прибора
	// на том же физическом канале.
	ioLockKey := pollcore.PhysicalIOLockKey(trParams, req.ID)

	sess, err := session.New("modbus_byteorder_auth")
	if err != nil {
		return probeResponse{OK: false, Error: "создание сессии: " + err.Error()}
	}
	unlock := pollcore.LockKey(ioLockKey)
	err = sess.Open(probeCtx, tr)
	unlock()
	if err != nil {
		return probeResponse{OK: false, Error: "открытие сессии (byte-order/авторизация): " + err.Error()}
	}

	isTCP := req.TransportKind == "modbus_tcp"
	unitID := req.UnitID
	if unitID == 0 {
		unitID = 1
	}
	reader := pollcore.NewWithLockKey(tr, isTCP, uint8(unitID), ioLockKey)

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

// probeIVKTER выполняет безопасную проверку ИВК-ТЭР без записи в прибор:
// читает серийный номер вторичного вычислителя (IR 0x8002), текущие часы
// (IR 0x8000, uint32 Unix) и текущий расход (IR 0xC034, float).
// Все три регистра доступны на чтение в рабочем режиме; установка времени
// здесь намеренно отсутствует — HR 0x8000 требует сервисного режима прибора.
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
	resp.Steps = append(resp.Steps, "создаю транспорт")
	tr, err := transport.New(trParams)
	if err != nil {
		resp.Error = "не удалось создать транспорт: " + err.Error()
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	if err := tr.Open(probeCtx); err != nil {
		resp.Error = "не удалось открыть соединение: " + err.Error()
		resp.Steps = append(resp.Steps, "порт/соединение не открыто")
		return
	}
	resp.Steps = append(resp.Steps, "порт/соединение открыто")
	defer func() {
		if err := tr.Close(); err != nil {
			resp.Steps = append(resp.Steps, "ошибка закрытия порта: "+err.Error())
		} else {
			resp.Steps = append(resp.Steps, "порт/соединение закрыто")
		}
	}()

	isTCP := req.TransportKind == "modbus_tcp"
	unitID := req.UnitID
	if unitID == 0 {
		unitID = 1
	}
	reader := pollcore.NewWithLockKey(tr, isTCP, uint8(unitID), pollcore.PhysicalIOLockKey(trParams, req.ID))
	resp.OK = true
	resp.Steps = append(resp.Steps, fmt.Sprintf("начат опрос ИВК-ТЭР, Modbus-адрес %d", unitID))

	if raw, err := reader.ReadRaw(probeCtx, "IR", 32770, "uint32"); err != nil {
		resp.Error = firstNonEmpty(resp.Error, "серийный номер (IR 0x8002): "+err.Error())
	} else if serial, err := codec.DecodeUint32(raw, "0123"); err != nil {
		resp.Error = firstNonEmpty(resp.Error, "разбор серийного номера: "+err.Error())
	} else if serial == 0 || serial == 0xFFFFFFFF {
		resp.Error = firstNonEmpty(resp.Error, fmt.Sprintf("некорректный серийный номер: %d", serial))
	} else {
		resp.SerialNumber = fmt.Sprintf("%d", serial)
		resp.Steps = append(resp.Steps, "прибор ответил: серийный номер прочитан")
	}

	if raw, err := reader.ReadRaw(probeCtx, "IR", 32768, "uint32"); err != nil {
		resp.Error = firstNonEmpty(resp.Error, "время прибора (IR 0x8000): "+err.Error())
	} else if seconds, err := codec.DecodeUint32(raw, "0123"); err != nil {
		resp.Error = firstNonEmpty(resp.Error, "разбор времени прибора: "+err.Error())
	} else if seconds == 0 || seconds == 0xFFFFFFFF {
		resp.Error = firstNonEmpty(resp.Error, "прибор вернул некорректное значение времени")
	} else {
		resp.DeviceTime = time.Unix(int64(seconds), 0).Local().Format("02.01.2006 15:04:05")
		resp.Steps = append(resp.Steps, "часы прибора прочитаны")
	}

	// Критическая часть проверки ИВК-ТЭР: читаем вершину часового архива
	// той же функцией 65 (0x41), которую использует боевой PollArchives.
	resp.Steps = append(resp.Steps, "проверяю часовой архив функцией 65")
	archiveResp, archiveErr := reader.Transact(probeCtx, modbus.BuildArchive65IndexPDU(0, 1, 0))
	if archiveErr != nil {
		resp.Error = firstNonEmpty(resp.Error, "архив ИВК-ТЭР (функция 65): "+archiveErr.Error())
	} else if payload, err := modbus.ParseArchive65Response(archiveResp); err != nil {
		resp.Error = firstNonEmpty(resp.Error, "разбор архива ИВК-ТЭР (функция 65): "+err.Error())
	} else if len(payload) < 30 {
		resp.Error = firstNonEmpty(resp.Error, fmt.Sprintf("архив ИВК-ТЭР: короткая запись %d байт, ожидается 30", len(payload)))
	} else {
		ts := binary.BigEndian.Uint32(payload[:4])
		if ts == 0 || ts == 0xFFFFFFFF {
			resp.Error = firstNonEmpty(resp.Error, "архив ИВК-ТЭР: прибор вернул маркер отсутствующей записи")
		} else {
			resp.ArchiveInfo = "функция 65 отвечает, последняя запись: " + time.Unix(int64(ts), 0).Local().Format("02.01.2006 15:04:05")
			resp.Steps = append(resp.Steps, "часовой архив функцией 65 читается")
		}
	}

	// Мгновенный расход оставляем дополнительной диагностикой, но он не
	// определяет успех архивного шлюза.
	if raw, err := reader.ReadRaw(probeCtx, "IR", 49204, "float"); err == nil {
		if flow, err := codec.DecodeFloat32(raw, "0123"); err == nil {
			resp.CurrentFlow = fmt.Sprintf("%.3f л/мин", flow)
		}
	}

	if resp.SerialNumber == "" && resp.DeviceTime == "" && resp.ArchiveInfo == "" {
		resp.OK = false
		resp.Steps = append(resp.Steps, "прибор не отвечает на контрольные запросы")
		resp.Error = firstNonEmpty(resp.Error, "соединение открыто, но ИВК-ТЭР не ответил ни на контрольные регистры, ни на архивную функцию 65")
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

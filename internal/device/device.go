package device

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"time"

	"mbgw/internal/archive"
	"mbgw/internal/codec"
	"mbgw/internal/devicestatus"
	"mbgw/internal/errs"
	"mbgw/internal/health"
	"mbgw/internal/lease"
	"mbgw/internal/pointresolver"
	"mbgw/internal/profile"
	"mbgw/internal/protocol/modbus"
	"mbgw/internal/quality"
	"mbgw/internal/session"
	"mbgw/internal/storage"
)

// PointClient is the minimal interface Device needs to read logical points
// and send raw transactions to a device. *pollcore.Reader implements this.
type PointClient interface {
	ReadRaw(ctx context.Context, space string, addr int, dataType string) ([]byte, error)
	Transact(ctx context.Context, req []byte) ([]byte, error)
}

// sessionAdapter adapts session.Session to archive.ArchiveSession (which
// expects State() string), since session.Session.State() returns the
// package-local session.State int-enum instead. This keeps
// internal/archive from importing internal/session directly (see
// archive.ArchiveSession's doc comment on the layering rule).
type sessionAdapter struct {
	sess session.Session
}

func (a sessionAdapter) State() string {
	switch a.sess.State() {
	case session.StateClosed:
		return "closed"
	case session.StateInitializing:
		return "initializing"
	case session.StateReady:
		return "ready"
	case session.StateError:
		return "error"
	default:
		return "unknown"
	}
}

type Device struct {
	ID      string
	Profile *profile.Profile
	Client  PointClient
	Sess    session.Session
	Repo    storage.Repo
	Lease   *lease.LocalLease

	// GapScanWindowHours, when > 0, makes each archive poll finish by
	// patching any missing hours in the last N hours (see GapScan). 0
	// disables it. Set from config; left 0 by New() so existing callers
	// are unaffected.
	GapScanWindowHours int

	// BackfillMaxDepthHours is the configured startup/manual deep-backfill
	// limit for this device. poller.KindBackfill calls BackfillArchives
	// without explicit options, so BackfillArchives uses this value as its
	// default. 0 keeps the existing "use physical buffer depth" behaviour.
	BackfillMaxDepthHours int

	// VKM-360 automatic clock correction settings. The server copies these
	// from the device row in SQLite. Automatic correction is enabled only
	// when MaxStepSeconds and DailyLimitSeconds are both > 0. HR1009 itself
	// is limited by the UVP protocol to at most +/-99 seconds per command.
	TimeCorrectionDeadbandSeconds   int
	TimeCorrectionMaxStepSeconds    int
	TimeCorrectionDailyLimitSeconds int
}

func New(id string, p *profile.Profile, cli PointClient, sess session.Session, repo storage.Repo, l *lease.LocalLease) *Device {
	return &Device{
		ID:      id,
		Profile: p,
		Client:  cli,
		Sess:    sess,
		Repo:    repo,
		Lease:   l,
	}
}

// leaseAcquireRetries / leaseAcquireRetryDelay управляют повторными
// попытками занять lease конкретного прибора+архива, когда она временно
// занята чем-то ещё — прежде всего принудительным переопросом (см.
// vkm_reload.go/ForceReloadVKMHourly, akron_reload.go/
// ForceReloadAkronHourly — оба держат lease только на время ОДНОГО
// запрошенного периода, отпуская её между периодами через
// collectVKMPeriod/аналогичную функцию Akron). Без повтора обычный
// плановый такт (pollVKMHourlyLatest / generic-ветка PollArchives ниже),
// попавший ровно в момент, когда переопрос удерживает линию, молча
// пропускал бы весь такт — до часа простоя получасовки, пока не
// сработает следующий тик планировщика.
//
// ПОДТВЕРЖДЕНО ЖИВЬЁМ (найдено оператором 2026-08-27/29, mbgw_server.log):
//
//	VKM архив main: период 27.08.2026 13:30: ошибка:
//	lease: lease held for device "boylernaya_par"... device lease held by another owner
//
// 8 попыток по 4с = до 32с ожидания — с запасом перекрывает типичное
// время одного периода переопроса (в переписке с оператором: 60
// периодов ≈ 3 минуты, то есть около 3с на период).
const (
	leaseAcquireRetries    = 8
	leaseAcquireRetryDelay = 4 * time.Second
)

// acquireLeaseWithRetry — обёртка над d.Lease.Acquire с повтором ИМЕННО
// при конфликте занятости (errs.ErrLease, см. internal/lease/lease.go).
// Прочие ошибки (например, пустой deviceID) возвращаются немедленно, без
// бессмысленного ожидания — они сами по себе не "рассосутся" со
// временем, в отличие от занятой линии связи.
func (d *Device) acquireLeaseWithRetry(ctx context.Context, leaseContext string, ttl time.Duration) (func(), error) {
	var lastErr error
	for attempt := 1; attempt <= leaseAcquireRetries; attempt++ {
		release, err := d.Lease.Acquire(ctx, d.ID, leaseContext, ttl)
		if err == nil {
			if attempt > 1 {
				log.Printf("[%s] lease %q занята с %d-й попытки (ждали конфликт с другой операцией на этой же линии)\n", d.ID, leaseContext, attempt)
			}
			return release, nil
		}
		lastErr = err
		if !errors.Is(err, errs.ErrLease) {
			return nil, err // не конфликт занятости — повтор не поможет
		}
		if attempt == leaseAcquireRetries {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(leaseAcquireRetryDelay):
		}
	}
	return nil, fmt.Errorf("после %d попыток за %v (линия связи занята другой операцией): %w",
		leaseAcquireRetries, time.Duration(leaseAcquireRetries)*leaseAcquireRetryDelay, lastErr)
}

// Start runs two independent polling loops: current values (frequent,
// e.g. every few seconds) and archives (infrequent, e.g. hourly/daily,
// since archive strategies like mb_request_poll_string are multi-step
// transactions with device-side collection delays and should not be
// driven at the same cadence as simple register reads).
func (d *Device) Start(ctx context.Context, pointInterval time.Duration, archiveInterval time.Duration) {
	pointTicker := time.NewTicker(pointInterval)
	defer pointTicker.Stop()

	var archiveTicker *time.Ticker
	var archiveChan <-chan time.Time
	if archiveInterval > 0 && len(d.Profile.Archives) > 0 {
		archiveTicker = time.NewTicker(archiveInterval)
		defer archiveTicker.Stop()
		archiveChan = archiveTicker.C
	}

	log.Printf("[%s] запуск цикла опроса (текущие: %v, архивы: %v)...\n", d.ID, pointInterval, archiveInterval)

	d.detectFirmwareVariant(ctx)
	d.Poll(ctx)

	for {
		select {
		case <-ctx.Done():
			log.Printf("[%s] остановка опроса\n", d.ID)
			return
		case <-pointTicker.C:
			d.Poll(ctx)
		case <-archiveChan:
			d.PollArchives(ctx)
		}
	}
}

// detectFirmwareVariant queries function 17 (Report Slave ID) once at
// startup, mainly to log firmware version info for diagnostics. Devices
// that do not support function 17 (e.g. it is not part of their protocol,
// or the emulator/real device returns an error) simply log a warning and
// continue - this is not fatal to polling. Using firmware_variant to
// actually branch archive decoding logic is a follow-up once a device
// profile needs more than one record layout variant (see backlog).
func (d *Device) detectFirmwareVariant(ctx context.Context) {
	req := modbus.BuildReportSlaveIDPDU()
	respPDU, err := d.Client.Transact(ctx, req)
	if err != nil {
		log.Printf("[%s] функция 17 (report slave id) недоступна: %v\n", d.ID, err)
		return
	}

	resp, err := modbus.ParseReportSlaveIDResponse(respPDU)
	if err != nil {
		log.Printf("[%s] ошибка разбора ответа функции 17: %v\n", d.ID, err)
		return
	}

	log.Printf("[%s] диагностика (func17): run_status=0x%02X raw_data=%q\n", d.ID, resp.RunStatus, string(resp.RawData))
}

// Poll reads every current-value point declared in the device profile
// (exported so internal/poller can drive it centrally; Device.Start
// still exists for standalone/single-device use and calls this too).
func (d *Device) Poll(ctx context.Context) {
	d.updateTimeDrift(ctx)
	statusValues := d.collectStatusValues(ctx)

	for _, pt := range d.Profile.Points {
		// Write-only points (e.g. VZLET's HR 0x8000 "time set" register)
		// are not part of the regular read cycle - reading them back
		// would just echo the last written value, not any live reading,
		// and wastes a poll slot. Points not explicitly marked
		// access:"write" (the common case: unset, "read", "readwrite")
		// are polled as before.
		if pt.Access == "write" {
			continue
		}
		if pt.Instance == "" {
			d.pollOnePoint(ctx, pt, pt.AddrOrZero(), "", statusValues)
			continue
		}

		inst, ok := d.Profile.Instances[pt.Instance]
		if !ok {
			log.Printf("[%s] точка %s: instance %q не описан в профиле\n", d.ID, pt.Name, pt.Instance)
			continue
		}

		count := inst.Count
		if inst.Enumerate != "fixed_count" {
			log.Printf("[%s] точка %s: enumerate %q пока не поддерживается (только fixed_count) - см. backlog\n", d.ID, pt.Name, inst.Enumerate)
			continue
		}
		if count <= 0 {
			log.Printf("[%s] точка %s: instance %q имеет count<=0\n", d.ID, pt.Name, pt.Instance)
			continue
		}

		for i := 1; i <= count; i++ {
			addr, err := pointresolver.Resolve(pt.AddrFormula, pt.Instance, i)
			if err != nil {
				log.Printf("[%s] точка %s (instance %d): ошибка формулы адреса: %v\n", d.ID, pt.Name, i, err)
				continue
			}
			d.pollOnePoint(ctx, pt, addr, fmt.Sprintf("%d", i), statusValues)
		}
	}
}

// updateTimeDrift publishes the current meter clock state for the
// dashboard.
//
// Akron has a live-confirmed read-only clock block at function 03,
// registers 0x10..0x13.
//
// VKM-360 has a live-confirmed read-only clock block at holding registers
// 1800..1805: day, month, year, hour, minute, second. The block is read in
// ONE transaction so the fields cannot straddle a second boundary.
//
// For both meter types, server time is sampled immediately before and
// after the Modbus transaction and the midpoint is used for comparison,
// which removes most request/response latency bias.
func (d *Device) updateTimeDrift(ctx context.Context) {
	if d.hasAkronArchive() {
		d.updateAkronTimeDrift(ctx)
		return
	}
	d.updateVKMTimeDrift(ctx)
}

// timeCorrectionStore is deliberately a narrow local interface instead of
// widening storage.Repo. The production *sqlite.Repo implements it; simple
// device-package test fakes do not need to implement correction bookkeeping
// unless a test explicitly enables automatic correction.
type timeCorrectionStore interface {
	TimeCorrectionUsedLast24Hours(ctx context.Context, deviceID string, now time.Time) (int, error)
	RecordTimeCorrection(ctx context.Context, deviceID string, correctionSeconds int, correctedAt time.Time) error
}

func (d *Device) updateVKMTimeDrift(ctx context.Context) {
	deviceTime, midpoint, err := d.readVKMClock(ctx)
	if err != nil {
		devicestatus.Set(d.ID, devicestatus.TimeDrift{
			CheckedAt: midpoint,
			Reliable:  false,
			Note:      "не удалось прочитать текущие часы ВКМ: " + err.Error(),
		})
		return
	}

	// First publish the observed drift even when automatic correction is
	// disabled. The dashboard must always show the real read-only result.
	devicestatus.Set(d.ID, devicestatus.TimeDrift{
		CheckedAt:    midpoint,
		DriftSeconds: deviceTime.Sub(midpoint).Seconds(),
		Reliable:     true,
	})

	// Safe opt-in: existing devices have zeros after schema migration and
	// therefore only READ their clocks until the operator explicitly sets
	// both correction limits in the device form.
	if d.TimeCorrectionMaxStepSeconds <= 0 || d.TimeCorrectionDailyLimitSeconds <= 0 {
		return
	}
	// HR1009 correction is signed in the SERVER-minus-DEVICE direction:
	// positive moves the VKM clock forward, negative moves it backward.
	desired := int(math.Round(midpoint.Sub(deviceTime).Seconds()))
	if desired == 0 || absInt(desired) <= d.TimeCorrectionDeadbandSeconds {
		return
	}

	store, ok := d.Repo.(timeCorrectionStore)
	if !ok {
		log.Printf("[%s] [ВРЕМЯ] автокоррекция ВКМ пропущена: хранилище не поддерживает учёт суточного лимита\n", d.ID)
		return
	}

	used, err := store.TimeCorrectionUsedLast24Hours(ctx, d.ID, midpoint)
	if err != nil {
		// Fail closed: if the daily budget cannot be read, do not write the
		// clock. Otherwise a DB problem could silently bypass the operator's
		// maximum-correction-per-24h safety limit.
		log.Printf("[%s] [ВРЕМЯ] автокоррекция ВКМ пропущена: не удалось проверить лимит за 24 часа: %v\n", d.ID, err)
		return
	}

	remaining := d.TimeCorrectionDailyLimitSeconds - used
	if remaining <= 0 {
		log.Printf("[%s] [ВРЕМЯ] коррекция не выполняется: лимит за 24 часа исчерпан (%d/%d сек)\n",
			d.ID, used, d.TimeCorrectionDailyLimitSeconds)
		return
	}

	maxStep := d.TimeCorrectionMaxStepSeconds
	if maxStep > 99 {
		maxStep = 99 // protocol hard limit; API also validates this value
	}

	step := absInt(desired)
	if step > maxStep {
		step = maxStep
	}
	if step > remaining {
		step = remaining
	}
	if step <= 0 {
		return
	}
	if desired < 0 {
		step = -step
	}

	// Reserve this correction in the rolling 24-hour budget BEFORE touching
	// the meter clock. This is deliberately conservative: if the following
	// Modbus write fails (or its response is lost), the reservation remains
	// until it ages out after 24 hours. That may temporarily reduce the
	// available budget, but it guarantees that a DB/accounting failure can
	// never let real clock writes exceed the operator's configured daily
	// limit, including across process restarts.
	reservedAt := time.Now()
	if err := store.RecordTimeCorrection(ctx, d.ID, step, reservedAt); err != nil {
		log.Printf("[%s] [ВРЕМЯ] автокоррекция ВКМ пропущена: не удалось зарезервировать %+d сек в лимите за 24 часа: %v\n", d.ID, step, err)
		return
	}

	// UVP-280.01 / VKM-360 live-confirmed encoding of HR1009:
	// +15 seconds -> 1515 decimal, -24 seconds -> -2424 decimal.
	// Therefore the signed command word is correctionSeconds * 101.
	command := int16(step * 101)
	req := modbus.BuildWriteSingleRegisterPDU(1009, uint16(command))
	resp, err := d.Client.Transact(ctx, req)
	if err != nil {
		log.Printf("[%s] [ВРЕМЯ] ошибка коррекции часов ВКМ на %+d сек: %v; %+d сек остаются зарезервированы в суточном лимите\n", d.ID, step, err, absInt(step))
		return
	}
	if !sameWriteSingleRegisterResponse(resp, req) {
		log.Printf("[%s] [ВРЕМЯ] ответ на коррекцию часов ВКМ не подтверждён: % X, ожидалось % X; %+d сек остаются зарезервированы в суточном лимите\n", d.ID, resp, req, absInt(step))
		return
	}

	log.Printf("[%s] [ВРЕМЯ] часы ВКМ скорректированы на %+d сек (лимит за 24ч: было использовано %d, после этой коррекции зарезервировано %d из %d сек)\n",
		d.ID, step, used, used+absInt(step), d.TimeCorrectionDailyLimitSeconds)

	// The live probe showed the clock settles immediately but we give the
	// device a short moment before the verification read. This happens only
	// when an actual correction was made, not on every poll.
	select {
	case <-ctx.Done():
		return
	case <-time.After(1500 * time.Millisecond):
	}

	afterDeviceTime, afterMidpoint, err := d.readVKMClock(ctx)
	if err != nil {
		devicestatus.Set(d.ID, devicestatus.TimeDrift{
			CheckedAt: afterMidpoint,
			Reliable:  false,
			Note:      "коррекция времени ВКМ выполнена, но контрольное чтение не удалось: " + err.Error(),
		})
		return
	}

	devicestatus.Set(d.ID, devicestatus.TimeDrift{
		CheckedAt:    afterMidpoint,
		DriftSeconds: afterDeviceTime.Sub(afterMidpoint).Seconds(),
		Reliable:     true,
	})
}

func (d *Device) readVKMClock(ctx context.Context) (time.Time, time.Time, error) {
	// Holding registers 1800..1805 (decimal) = 0x0708..0x070D.
	// One function-03 request reads all six fields atomically:
	// day, month, year, hour, minute, second.
	before := time.Now()
	resp, err := d.Client.Transact(ctx, []byte{0x03, 0x07, 0x08, 0x00, 0x06})
	after := time.Now()
	midpoint := before.Add(after.Sub(before) / 2)
	if err != nil {
		return time.Time{}, midpoint, err
	}

	deviceTime, err := decodeVKMClockResponse(resp, time.Local)
	if err != nil {
		return time.Time{}, midpoint, err
	}
	return deviceTime, midpoint, nil
}

func sameWriteSingleRegisterResponse(resp, req []byte) bool {
	if len(req) != 5 || len(resp) < 5 {
		return false
	}
	for i := 0; i < 5; i++ {
		if resp[i] != req[i] {
			return false
		}
	}
	return true
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func (d *Device) updateAkronTimeDrift(ctx context.Context) {
	before := time.Now()
	resp, err := d.Client.Transact(ctx, []byte{0x03, 0x00, 0x10, 0x00, 0x04})
	after := time.Now()
	midpoint := before.Add(after.Sub(before) / 2)

	if err != nil {
		devicestatus.Set(d.ID, devicestatus.TimeDrift{
			CheckedAt: midpoint,
			Reliable:  false,
			Note:      "не удалось прочитать текущие часы Akron: " + err.Error(),
		})
		return
	}

	deviceTime, err := decodeAkronClockResponse(resp, time.Local)
	if err != nil {
		devicestatus.Set(d.ID, devicestatus.TimeDrift{
			CheckedAt: midpoint,
			Reliable:  false,
			Note:      "не удалось разобрать текущие часы Akron: " + err.Error(),
		})
		return
	}

	devicestatus.Set(d.ID, devicestatus.TimeDrift{
		CheckedAt:    midpoint,
		DriftSeconds: deviceTime.Sub(midpoint).Seconds(),
		Reliable:     true,
	})
}

func decodeVKMClockResponse(resp []byte, loc *time.Location) (time.Time, error) {
	// Standard Read Holding Registers response PDU:
	// [0x03][byteCount=12]
	// [dayHi][dayLo][monthHi][monthLo][yearHi][yearLo]
	// [hourHi][hourLo][minuteHi][minuteLo][secondHi][secondLo]
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

func (d *Device) hasAkronArchive() bool {
	for _, a := range d.Profile.Archives {
		if a.Strategy == "akron_archive" {
			return true
		}
	}
	return false
}

func decodeAkronClockResponse(resp []byte, loc *time.Location) (time.Time, error) {
	// Standard Read Holding Registers response PDU:
	// [0x03][byteCount=8][sec][min][hour][dow][day][month][year][id/am]
	if len(resp) < 10 {
		return time.Time{}, fmt.Errorf("короткий ответ: %d байт, нужно минимум 10", len(resp))
	}
	if resp[0] != 0x03 {
		return time.Time{}, fmt.Errorf("неожиданная функция 0x%02X вместо 0x03", resp[0])
	}
	if resp[1] < 8 {
		return time.Time{}, fmt.Errorf("byte count=%d, нужно минимум 8", resp[1])
	}

	data := resp[2:10]
	sec, e1 := codec.DecodeBCDByte(data[0])
	min, e2 := codec.DecodeBCDByte(data[1])
	hour, e3 := codec.DecodeBCDByte(data[2])
	day, e4 := codec.DecodeBCDByte(data[4])
	month, e5 := codec.DecodeBCDByte(data[5])
	year, e6 := codec.DecodeBCDByte(data[6])

	for _, err := range []error{e1, e2, e3, e4, e5, e6} {
		if err != nil {
			return time.Time{}, err
		}
	}

	if sec > 59 || min > 59 || hour > 23 || month < 1 || month > 12 || day < 1 || day > 31 {
		return time.Time{}, fmt.Errorf(
			"время вне диапазона: %02d.%02d.20%02d %02d:%02d:%02d",
			day, month, year, hour, min, sec,
		)
	}

	fullYear := 2000 + year
	t := time.Date(fullYear, time.Month(month), day, hour, min, sec, 0, loc)
	if t.Year() != fullYear || int(t.Month()) != month || t.Day() != day {
		return time.Time{}, fmt.Errorf(
			"некорректная календарная дата: %02d.%02d.%04d",
			day, month, fullYear,
		)
	}

	return t, nil
}

// pollOnePoint reads, decodes, and saves a single point at a resolved
// address, optionally tagged with an instance identifier (empty string
// for non-parametric points).
// collectStatusValues reads and decodes every point referenced as a
// quality_map "source" (typically a status/bitfield word), returning a
// map of point name -> decoded integer value. Called once per poll cycle,
// before evaluating quality for the rest of the device's points. Points
// with a parametric instance are not supported as quality sources in this
// vertical slice (status words are assumed device-wide, not per-instance).
func (d *Device) collectStatusValues(ctx context.Context) quality.StatusValues {
	result := make(quality.StatusValues)

	sourceNames := make(map[string]bool)
	for _, rule := range d.Profile.QualityMap {
		sourceNames[rule.Source] = true
	}
	if len(sourceNames) == 0 {
		return result
	}

	readOne := func(pt profile.Point, addr int, instance string) {
		dataBytes, err := d.Client.ReadRaw(ctx, pt.Space, addr, pt.Type)
		if err != nil {
			log.Printf("[%s] ошибка чтения статуса %s (instance=%s): %v\n", d.ID, pt.Name, instance, err)
			return
		}
		val, err := d.decodePoint(pt, dataBytes)
		if err != nil {
			log.Printf("[%s] ошибка декодирования статуса %s (instance=%s): %v\n", d.ID, pt.Name, instance, err)
			return
		}
		intVal, ok := val.(int64)
		if !ok {
			log.Printf("[%s] статус %s имеет нечисловой тип %T, пропускаю\n", d.ID, pt.Name, val)
			return
		}
		if result[pt.Name] == nil {
			result[pt.Name] = make(map[string]int64)
		}
		result[pt.Name][instance] = intVal
	}

	for _, pt := range d.Profile.Points {
		if !sourceNames[pt.Name] {
			continue
		}

		if pt.Instance == "" {
			readOne(pt, pt.AddrOrZero(), "")
			continue
		}

		inst, ok := d.Profile.Instances[pt.Instance]
		if !ok || inst.Enumerate != "fixed_count" || inst.Count <= 0 {
			log.Printf("[%s] статус %s: instance %q не поддерживается для сбора статуса\n", d.ID, pt.Name, pt.Instance)
			continue
		}
		for i := 1; i <= inst.Count; i++ {
			addr, err := pointresolver.Resolve(pt.AddrFormula, pt.Instance, i)
			if err != nil {
				log.Printf("[%s] статус %s (instance %d): ошибка формулы адреса: %v\n", d.ID, pt.Name, i, err)
				continue
			}
			readOne(pt, addr, fmt.Sprintf("%d", i))
		}
	}

	return result
}
func (d *Device) pollOnePoint(ctx context.Context, pt profile.Point, addr int, instance string, statusValues quality.StatusValues) {
	dataBytes, err := d.Client.ReadRaw(ctx, pt.Space, addr, pt.Type)
	if err != nil {
		log.Printf("[%s] ошибка опроса %s (instance=%s): %v\n", d.ID, pt.Name, instance, err)
		return
	}

	val, err := d.decodePoint(pt, dataBytes)
	if err != nil {
		log.Printf("[%s] ошибка декодирования %s (instance=%s): %v\n", d.ID, pt.Name, instance, err)
		return
	}

	qTag, reason := quality.Evaluate(pt.Name, instance, d.Profile.QualityMap, statusValues)

	reading := storage.ReadingCurrent{
		DeviceID:      d.ID,
		PointID:       pt.Name,
		Instance:      instance,
		Value:         val,
		Unit:          pt.Unit,
		Quality:       string(qTag),
		QualityReason: reason,
		Timestamp:     time.Now(),
	}
	if err := d.Repo.SaveReadingCurrent(ctx, reading); err != nil {
		log.Printf("[%s] ошибка сохранения: %v\n", d.ID, err)
		return
	}
	health.MarkCurrentSuccess(d.ID, reading.Timestamp)

	if instance != "" {
		log.Printf("[%s] [SAVE] %s[%s] = %v %s\n", d.ID, pt.Name, instance, val, pt.Unit)
	} else {
		log.Printf("[%s] [SAVE] %s = %v %s\n", d.ID, pt.Name, val, pt.Unit)
	}
}

// pollArchives runs each archive strategy declared in the device profile.
// The Client is used directly as archive.Transactor: every strategy's
// requests/responses are bare Modbus PDUs (no address, no CRC baked in -
// those are the RTU/TCP transport layer's job, added/stripped uniformly
// by protocol/modbus.Transact for every strategy the same way, whether
// the PDU carries a standard function code or a vendor User-Defined one
// like VZLET's 65 or Akron's 100-110). See internal/protocol/akron's
// package doc comment for why this matters and what happens if a
// strategy builds a complete frame itself instead.
// PollArchives runs each archive strategy declared in the device profile
// (exported so internal/poller can drive it centrally).
func (d *Device) PollArchives(ctx context.Context) {
	for _, a := range d.Profile.Archives {
		// VKM's mb_request_poll_string strategy returns ONE aggregate per
		// request (CONFIRMED live 2026-08-01 — see vkm_hourly.go), not a
		// per-hour breakdown the generic single-wide-window query below
		// would assume. It gets its own dedicated per-hour path instead.
		if a.Strategy == "mb_request_poll_string" {
			d.pollVKMHourlyLatest(ctx, a)
			continue
		}

		reader, ok := archive.Get(a.Strategy)
		if !ok {
			log.Printf("[%s] архив %s: неизвестная стратегия %s\n", d.ID, a.ID, a.Strategy)
			continue
		}

		layout := layoutFromProfile(a)

		// CONFIRMED BUG (2026-08-22): index-based strategies (currently
		// only akron_archive) read ONLY FromIndex/ToIndex — From/To
		// (time-based) are irrelevant to them, since the device itself
		// only understands "give me N rows starting at index i", not a
		// time window (see archive.AkronArchiveReader.Read: count is
		// derived from q.ToIndex-q.FromIndex, defaulting to 1 when both
		// are left at their zero value). Leaving FromIndex/ToIndex unset
		// here made every regular poll tick fetch exactly ONE record
		// (i=1, the device's single newest row) regardless of the
		// intended 24h window below — the periodic sweep was never
		// actually sweeping; only the separate backfill/gap-scan path
		// (backfill.go) set these correctly.
		//
		// ToIndex uses a.MaxRowsOrDefault()-1 — the SAME authoritative,
		// per-profile row-count source backfill.go already uses — not a
		// hardcoded number. This matters: profiles/acron-01.yaml's own
		// comment documents that this exact class of hardcoded-limit
		// mistake already happened once (datasheet says 31 rows/request,
		// real device firmware only accepts 27, confirmed live; 28+ gets
		// rejected with Modbus exception 0xFC). A hardcoded window size
		// here would silently drift from whatever a future device/
		// firmware's confirmed-safe value is, exactly like the datasheet
		// value did. persistAkronHourly's upsert makes any overlap with
		// already-stored hours harmless.
		pageSize := a.MaxRowsOrDefault()
		q := archive.ArchiveQuery{
			DeviceID:     d.ID,
			ArchiveID:    a.ID,
			Instance:     1,
			From:         time.Now().Add(-24 * time.Hour),
			To:           time.Now(),
			FromIndex:    0,
			ToIndex:      pageSize - 1,
			RecordLayout: layout,
			WordOrder32:  d.Profile.Codec.WordOrder32,
			WordOrder64:  d.Profile.Codec.WordOrder64,
			Params:       a.Params,
		}

		release, leaseErr := d.acquireLeaseWithRetry(ctx, a.ID, 30*time.Second)
		if leaseErr != nil {
			log.Printf("[%s] архив %s: не удалось занять lease: %v\n", d.ID, a.ID, leaseErr)
			continue
		}

		records, err := reader.Read(ctx, sessionAdapter{d.Sess}, d.Client, q)
		release()
		if err != nil {
			log.Printf("[%s] архив %s: ошибка чтения: %v\n", d.ID, a.ID, err)
			continue
		}

		log.Printf("[%s] архив %s: получено записей: %d\n", d.ID, a.ID, len(records))
		for _, rec := range records {
			log.Printf("[%s] архив %s: запись ts=%v поля=%v\n", d.ID, a.ID, rec.RecordTS, rec.Fields)
		}

		if a.Strategy == "akron_archive" {
			if kind, _ := a.Params["archive_kind"].(string); kind == "hourly" {
				saved := persistAkronHourly(ctx, d.Repo, d.ID, a, records)
				if saved > 0 {
					health.MarkArchiveSuccess(d.ID, time.Now())
				}
				log.Printf("[%s] архив %s: сохранено часовок: %d/%d\n", d.ID, a.ID, saved, len(records))
			}
		}
	}

	// After the regular sweep, patch any recent holes (single failed hours
	// that the contiguous sweep above may not cover). No-op when
	// GapScanWindowHours is 0 or when there are no gaps.
	if d.GapScanWindowHours > 0 {
		d.GapScan(ctx, d.GapScanWindowHours)
	}
}

// decodePoint decodes raw register bytes according to the point's declared
// type. Covers numeric/bitfield/composite (u32+float, long+float) types
// supported by codec. Strings/asciiz and stInfoEvent are not yet wired here
// (they require record_layout-aware decoding beyond a single point read) -
// see backlog.
func (d *Device) decodePoint(pt profile.Point, data []byte) (any, error) {
	order32 := d.Profile.Codec.WordOrder32
	order64 := d.Profile.Codec.WordOrder64

	// ByteOffset lets a point read a sub-region of the raw register data,
	// for devices that pack more than one logical field into a single
	// register (e.g. Akron-01/02's clock register 0x0010 = [second, minute]).
	// Zero (the common case) leaves data untouched.
	if pt.ByteOffset > 0 {
		if pt.ByteOffset >= len(data) {
			return nil, fmt.Errorf("point %q: byte_offset %d is out of range for %d bytes read", pt.Name, pt.ByteOffset, len(data))
		}
		data = data[pt.ByteOffset:]
	}

	switch pt.Type {
	case "float":
		v, err := codec.DecodeFloat32(data, order32)
		if err != nil {
			return nil, err
		}
		return float64(v), nil
	case "double":
		return codec.DecodeFloat64(data, order64)
	case "int32":
		v, err := codec.DecodeInt32(data, order32)
		if err != nil {
			return nil, err
		}
		return int64(v), nil
	case "uint32":
		v, err := codec.DecodeUint32(data, order32)
		if err != nil {
			return nil, err
		}
		if pt.Epoch != "" {
			return time.Unix(int64(v), 0).UTC(), nil
		}
		return int64(v), nil
	case "int16":
		v, err := codec.DecodeInt16(data)
		if err != nil {
			return nil, err
		}
		return int64(v), nil
	case "uint16", "bitfield":
		v, err := codec.DecodeUint16(data)
		if err != nil {
			return nil, err
		}
		return int64(v), nil
	case "scaled_int":
		v, err := codec.DecodeUint16(data)
		if err != nil {
			return nil, err
		}
		scale := pt.Scale
		if scale == 0 {
			scale = 1
		}
		return float64(v) * scale, nil
	case "u32+float":
		return codec.DecodeU32Float(data, order32)
	case "long+float":
		return codec.DecodeLongFloat(data, order32)
	case "bcd":
		// Decodes the first byte of data as a single BCD digit pair
		// (0-99). This assumes one BCD field occupies its own register
		// (RegisterCount("bcd")=1, i.e. 2 bytes read, first byte used) -
		// a device that packs two BCD fields into one register (e.g.
		// Akron-01/02's clock: second+minute sharing register 0x0010)
		// cannot be modeled as two separate Points this way; that needs
		// a sub-register field offset concept, deliberately not solved
		// here (see backlog).
		v, err := codec.DecodeBCDByte(data[0])
		if err != nil {
			return nil, err
		}
		return int64(v), nil
	case "string", "asciiz":
		return codec.DecodeString(data), nil
	default:
		return "", nil
	}
}

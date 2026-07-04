package device

import (
    "context"
    "fmt"
    "log"
    "time"

    "mbgw/internal/archive"
    "mbgw/internal/client"
    "mbgw/internal/codec"
    "mbgw/internal/pointresolver"
    "mbgw/internal/profile"
    "mbgw/internal/protocol/modbus"
    "mbgw/internal/session"
    "mbgw/internal/storage"
)

type Device struct {
    ID      string
    Profile *profile.Profile
    Client  client.Client
    Sess    session.Session
    Repo    storage.Repo
}

func New(id string, p *profile.Profile, cli client.Client, sess session.Session, repo storage.Repo) *Device {
    return &Device{
        ID:      id,
        Profile: p,
        Client:  cli,
        Sess:    sess,
        Repo:    repo,
    }
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
    d.poll(ctx)

    for {
        select {
        case <-ctx.Done():
            log.Printf("[%s] остановка опроса\n", d.ID)
            return
        case <-pointTicker.C:
            d.poll(ctx)
        case <-archiveChan:
            d.pollArchives(ctx)
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
func (d *Device) poll(ctx context.Context) {
    statusValues := d.collectStatusValues(ctx)

    for _, pt := range d.Profile.Points {
        if pt.Instance == "" {
            d.pollOnePoint(ctx, pt, pt.Addr, "", statusValues)
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

// pollOnePoint reads, decodes, and saves a single point at a resolved
// address, optionally tagged with an instance identifier (empty string
// for non-parametric points).
// collectStatusValues reads and decodes every point referenced as a
// quality_map "source" (typically a status/bitfield word), returning a
// map of point name -> decoded integer value. Called once per poll cycle,
// before evaluating quality for the rest of the device's points. Points
// with a parametric instance are not supported as quality sources in this
// vertical slice (status words are assumed device-wide, not per-instance).
func (d *Device) collectStatusValues(ctx context.Context) map[string]int64 {
    result := make(map[string]int64)

    sourceNames := make(map[string]bool)
    for _, rule := range d.Profile.QualityMap {
        sourceNames[rule.Source] = true
    }
    if len(sourceNames) == 0 {
        return result
    }

    for _, pt := range d.Profile.Points {
        if !sourceNames[pt.Name] || pt.Instance != "" {
            continue
        }
        dataBytes, err := d.Client.ReadRaw(ctx, pt.Space, pt.Addr, pt.Type)
        if err != nil {
            log.Printf("[%s] ошибка чтения статуса %s: %v\n", d.ID, pt.Name, err)
            continue
        }
        val, err := d.decodePoint(pt, dataBytes)
        if err != nil {
            log.Printf("[%s] ошибка декодирования статуса %s: %v\n", d.ID, pt.Name, err)
            continue
        }
        intVal, ok := val.(int64)
        if !ok {
            log.Printf("[%s] статус %s имеет нечисловой тип %T, пропускаю\n", d.ID, pt.Name, val)
            continue
        }
        result[pt.Name] = intVal
    }

    return result
}
func (d *Device) pollOnePoint(ctx context.Context, pt profile.Point, addr int, instance string, statusValues map[string]int64) {
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

    quality, reason := evaluateQuality(pt.Name, d.Profile.QualityMap, statusValues)

    reading := storage.ReadingCurrent{
        DeviceID:      d.ID,
        PointID:       pt.Name,
        Instance:      instance,
        Value:         val,
        Unit:          pt.Unit,
        Quality:       quality,
        QualityReason: reason,
        Timestamp:     time.Now(),
    }
    if err := d.Repo.SaveReadingCurrent(ctx, reading); err != nil {
        log.Printf("[%s] ошибка сохранения: %v\n", d.ID, err)
        return
    }

    if instance != "" {
        log.Printf("[%s] [SAVE] %s[%s] = %v %s\n", d.ID, pt.Name, instance, val, pt.Unit)
    } else {
        log.Printf("[%s] [SAVE] %s = %v %s\n", d.ID, pt.Name, val, pt.Unit)
    }
}

// pollArchives runs each archive strategy declared in the device profile.
// The Client is used directly as archive.Transactor (client.Client now
// includes Transact, so any implementation satisfies both interfaces).
func (d *Device) pollArchives(ctx context.Context) {
    for _, a := range d.Profile.Archives {
        reader, ok := archive.Get(a.Strategy)
        if !ok {
            log.Printf("[%s] архив %s: неизвестная стратегия %s\n", d.ID, a.ID, a.Strategy)
            continue
        }

        layout := make([]archive.RecordLayoutField, 0, len(a.RecordLayout))
        for _, f := range a.RecordLayout {
            layout = append(layout, archive.RecordLayoutField{
                Offset: f.Offset,
                Name:   f.Name,
                Type:   f.Type,
                Unit:   f.Unit,
                Scale:  f.Scale,
                CRC:    f.CRC,
                Epoch:  f.Epoch,
            })
        }

        q := archive.ArchiveQuery{
            DeviceID:     d.ID,
            ArchiveID:    a.ID,
            Instance:     1,
            From:         time.Now().Add(-24 * time.Hour),
            To:           time.Now(),
            RecordLayout: layout,
            WordOrder32:  d.Profile.Codec.WordOrder32,
            WordOrder64:  d.Profile.Codec.WordOrder64,
        }

        records, err := reader.Read(ctx, d.Client, q)
        if err != nil {
            log.Printf("[%s] архив %s: ошибка чтения: %v\n", d.ID, a.ID, err)
            continue
        }

        log.Printf("[%s] архив %s: получено записей: %d\n", d.ID, a.ID, len(records))
        for _, rec := range records {
            log.Printf("[%s] архив %s: запись ts=%v поля=%v\n", d.ID, a.ID, rec.RecordTS, rec.Fields)
        }
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
    case "string", "asciiz":
        return codec.DecodeString(data), nil
    default:
        return "", nil
    }
}
package device

import (
    "context"
    "log"
    "time"

    "mbgw/internal/archive"
    "mbgw/internal/client"
    "mbgw/internal/codec"
    "mbgw/internal/profile"
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

func (d *Device) poll(ctx context.Context) {
    for _, pt := range d.Profile.Points {
        dataBytes, err := d.Client.ReadRaw(ctx, pt.Space, pt.Addr, pt.Type)
        if err != nil {
            log.Printf("[%s] ошибка опроса %s: %v\n", d.ID, pt.Name, err)
            continue
        }

        val, err := d.decodePoint(pt, dataBytes)
        if err != nil {
            log.Printf("[%s] ошибка декодирования %s: %v\n", d.ID, pt.Name, err)
            continue
        }

        reading := storage.ReadingCurrent{
            DeviceID:      d.ID,
            PointID:       pt.Name,
            Instance:      "",
            Value:         val,
            Unit:          pt.Unit,
            Quality:       "GOOD",
            QualityReason: "",
            Timestamp:     time.Now(),
        }
        if err := d.Repo.SaveReadingCurrent(ctx, reading); err != nil {
            log.Printf("[%s] ошибка сохранения: %v\n", d.ID, err)
            continue
        }
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
    default:
        return "", nil
    }
}
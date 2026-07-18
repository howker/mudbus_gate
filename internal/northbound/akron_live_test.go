package northbound

import (
	"encoding/binary"
	"math"
	"testing"
	"time"

	"mbgw/internal/codec"
	"mbgw/internal/storage"
)

// seedAkron fills a fakeRepo with a passport, current readings, and three
// consecutive hourly rows (oldest→newest), mirroring what the downstream
// poller produces in production.
func seedAkron(repo *fakeRepo) {
	_ = repo.SaveDevicePassport(nil, storage.DevicePassport{
		DeviceID: "acron-1", Serial: 12345, DeviceType: 0x00, Firmware: "3.7",
	})
	now := time.Now()
	_ = repo.SaveReadingCurrent(nil, storage.ReadingCurrent{
		DeviceID: "acron-1", PointID: "V", Value: "1.25", Timestamp: now,
	})
	_ = repo.SaveReadingCurrent(nil, storage.ReadingCurrent{
		DeviceID: "acron-1", PointID: "Q", Value: "18.5", Timestamp: now,
	})
	_ = repo.SaveReadingCurrent(nil, storage.ReadingCurrent{
		DeviceID: "acron-1", PointID: "acc_time", Value: "3600", Timestamp: now,
	})
	base := time.Date(2026, 7, 17, 14, 0, 0, 0, time.Local)
	for i := 0; i < 3; i++ {
		_ = repo.SaveHourlyArchive(nil, storage.HourlyArchiveRecord{
			DeviceID: "acron-1", Channel: "", Param: "V",
			TsHour: base.Add(time.Duration(i) * time.Hour), // 14:00, 15:00, 16:00
			Value:  580.0 + float64(i),                     // 580, 581, 582
			Unit:   "m3",
		})
	}
}

func TestAkronLive_Identity_FromPassport(t *testing.T) {
	repo := &fakeRepo{}
	seedAkron(repo)
	r := NewAkronLiveResponder(repo, "acron-1")

	resp := r.Respond([]byte{101})
	want := []byte{101, 6, 0x00, 0x37, 0x39, 0x30, 0x00, 0x00} // 12345 LE
	if string(resp) != string(want) {
		t.Fatalf("101 resp = % X, want % X", resp, want)
	}
}

func TestAkronLive_Identity_NoPassport_Silent(t *testing.T) {
	r := NewAkronLiveResponder(&fakeRepo{}, "acron-1")
	if resp := r.Respond([]byte{101}); resp != nil {
		t.Fatalf("expected silence with no passport, got % X", resp)
	}
}

func TestAkronLive_Hourly_NewestFirstAndValues(t *testing.T) {
	repo := &fakeRepo{}
	seedAkron(repo)
	r := NewAkronLiveResponder(repo, "acron-1")

	// i=1, n=2 → newest two rows: 16:00 (582) then 15:00 (581).
	resp := r.Respond([]byte{104, 0x00, 0x01, 0x02})
	if resp == nil || resp[0] != 104 {
		t.Fatalf("bad 104 resp: % X", resp)
	}
	byteCount := int(resp[1])
	if byteCount != 18 || len(resp) != 2+18 {
		t.Fatalf("byteCount = %d (len %d), want 18 (+2 hdr)", byteCount, len(resp))
	}

	row0 := resp[2 : 2+9]
	row1 := resp[2+9 : 2+18]

	v0, err := codec.DecodeAkronVolume(row0[0:5], "3210")
	if err != nil || v0 != 582.0 {
		t.Fatalf("row0 volume = %v (err %v), want 582", v0, err)
	}
	if row0[5] != 0x16 || row0[6] != 0x17 || row0[7] != 0x07 || row0[8] != 0x26 {
		t.Fatalf("row0 time BCD = % X, want 16 17 07 26", row0[5:9])
	}
	v1, err := codec.DecodeAkronVolume(row1[0:5], "3210")
	if err != nil || v1 != 581.0 {
		t.Fatalf("row1 volume = %v (err %v), want 581", v1, err)
	}
	if row1[5] != 0x15 {
		t.Fatalf("row1 hour BCD = %X, want 15", row1[5])
	}
}

func TestAkronLive_Hourly_PastEnd_ReturnsWhatExists(t *testing.T) {
	repo := &fakeRepo{}
	seedAkron(repo) // 3 rows total
	r := NewAkronLiveResponder(repo, "acron-1")

	// i=3, n=5 → only the oldest row (14:00) exists at and past that index.
	resp := r.Respond([]byte{104, 0x00, 0x03, 0x05})
	if int(resp[1]) != 9 {
		t.Fatalf("byteCount = %d, want 9 (single row)", resp[1])
	}
	if resp[2+5] != 0x14 {
		t.Fatalf("hour BCD = %X, want 14", resp[2+5])
	}

	// Completely past the end → "no records": 68 00.
	resp = r.Respond([]byte{104, 0x00, 0x09, 0x02})
	if len(resp) != 2 || resp[1] != 0x00 {
		t.Fatalf("past-end resp = % X, want 68 00", resp)
	}
}

func TestAkronLive_Current_Layout(t *testing.T) {
	repo := &fakeRepo{}
	seedAkron(repo)
	r := NewAkronLiveResponder(repo, "acron-1")

	resp := r.Respond([]byte{102})
	if resp == nil || resp[0] != 102 || int(resp[1]) != 18 || len(resp) != 20 {
		t.Fatalf("bad 102 resp: % X", resp)
	}
	data := resp[2:]

	v := math.Float32frombits(binary.LittleEndian.Uint32(data[0:4]))
	q := math.Float32frombits(binary.LittleEndian.Uint32(data[4:8]))
	if v != 1.25 || q != 18.5 {
		t.Fatalf("V=%v Q=%v, want 1.25 / 18.5", v, q)
	}

	// U/Pu must decode to the newest hourly value (582).
	vol, err := codec.DecodeAkronVolume(data[8:13], "3210")
	if err != nil || vol != 582.0 {
		t.Fatalf("U/Pu volume = %v (err %v), want 582", vol, err)
	}

	acc := binary.LittleEndian.Uint32(data[13:17])
	if acc != 3600 {
		t.Fatalf("acc_time = %d, want 3600", acc)
	}
	if data[17] != 0x00 {
		t.Fatalf("fault code = %X, want 00", data[17])
	}
}

func TestAkronLive_ProbesAndStubs(t *testing.T) {
	r := NewAkronLiveResponder(&fakeRepo{}, "acron-1")

	if resp := r.Respond([]byte{110}); string(resp) != string([]byte{110, 1, 1}) {
		t.Fatalf("110 resp = % X", resp)
	}
	if resp := r.Respond([]byte{106, 1, 1}); string(resp) != string([]byte{106, 0}) {
		t.Fatalf("106 resp = % X", resp)
	}
	if resp := r.Respond([]byte{105, 0, 1, 1}); string(resp) != string([]byte{105, 0}) {
		t.Fatalf("105 resp = % X", resp)
	}
}

func TestAkronLive_Clock_IsLive(t *testing.T) {
	r := NewAkronLiveResponder(&fakeRepo{}, "acron-1")

	before := time.Now()
	resp := r.Respond([]byte{0x03, 0x00, 0x10, 0x00, 0x04})
	if resp == nil || int(resp[1]) != 8 {
		t.Fatalf("clock resp = % X", resp)
	}
	// Registers: [sec,min][hour,dow][day,mon][yy,0]. Check day/month/year
	// match "now" (hour could flip at midnight; date check is stable enough
	// within a test run).
	day := int(resp[2+4]>>4)*10 + int(resp[2+4]&0x0F)
	mon := int(resp[2+5]>>4)*10 + int(resp[2+5]&0x0F)
	yy := int(resp[2+6]>>4)*10 + int(resp[2+6]&0x0F)
	now := time.Now()
	okDay := day == now.Day() || day == before.Day()
	if !okDay || mon != int(now.Month()) || yy != now.Year()%100 {
		t.Fatalf("clock date = %02d.%02d.%02d, want today", day, mon, yy)
	}

	// Reads outside the clock block are not served.
	if resp := r.Respond([]byte{0x03, 0x00, 0x00, 0x00, 0x02}); resp != nil {
		t.Fatalf("non-clock read should be nil, got % X", resp)
	}
}

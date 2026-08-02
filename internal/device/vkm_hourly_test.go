package device

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"mbgw/internal/archive"
	"mbgw/internal/lease"
	"mbgw/internal/storage/sqlite"
)

func newTestDeviceForVKM(t *testing.T) *Device {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "vkm_hourly_test.db")
	repo, err := sqlite.New(dbPath)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.InitArchiveSchema(context.Background()); err != nil {
		t.Fatalf("init archive schema: %v", err)
	}
	return &Device{ID: "vkm_test", Repo: repo, Lease: lease.New()}
}

// TestPersistVKMHourly_SavesBothParams confirms S and ST are saved as two
// separate archive_hourly rows (param="S", param="ST") for the requested
// hour — NOT the instantaneous fields (Pi/Pbar/T/dP/H), which are
// deliberately excluded (see vkmHourlyParams's doc comment).
func TestPersistVKMHourly_SavesBothParams(t *testing.T) {
	d := newTestDeviceForVKM(t)
	hour := time.Date(2026, 8, 1, 14, 0, 0, 0, time.UTC)

	rec := archive.ArchiveRecord{
		Fields: map[string]any{
			"S":       1837.7266,
			"S_unit":  "кг",
			"ST":      5.2585298e+09,
			"ST_unit": "Дж",
			"Pi":      4.2206e+05, // instantaneous — must NOT be saved
			"Pi_unit": "Па",
		},
	}

	saved := persistVKMHourly(context.Background(), d, hour, rec)
	if saved != 2 {
		t.Fatalf("expected 2 fields saved (S, ST), got %d", saved)
	}

	got, err := d.Repo.GetHourlyArchiveDesc(context.Background(), "vkm_test", "", "S", 0, 10)
	if err != nil {
		t.Fatalf("read back S: %v", err)
	}
	if len(got) != 1 || got[0].Value != 1837.7266 || got[0].Unit != "кг" {
		t.Fatalf("unexpected S row: %+v", got)
	}

	gotST, err := d.Repo.GetHourlyArchiveDesc(context.Background(), "vkm_test", "", "ST", 0, 10)
	if err != nil {
		t.Fatalf("read back ST: %v", err)
	}
	if len(gotST) != 1 || gotST[0].Value != 5.2585298e+09 || gotST[0].Unit != "Дж" {
		t.Fatalf("unexpected ST row: %+v", gotST)
	}

	// Pi must never have been written as an hourly row.
	gotPi, err := d.Repo.GetHourlyArchiveDesc(context.Background(), "vkm_test", "", "Pi", 0, 10)
	if err != nil {
		t.Fatalf("read back Pi: %v", err)
	}
	if len(gotPi) != 0 {
		t.Fatalf("Pi (instantaneous) should never be saved to archive_hourly, got %d rows", len(gotPi))
	}
}

// TestPersistVKMHourly_MissingFieldIsGraceful confirms a result missing
// one of S/ST (e.g. a device that only reports one of them, or a garbled
// read) still saves whatever IS present instead of failing outright.
func TestPersistVKMHourly_MissingFieldIsGraceful(t *testing.T) {
	d := newTestDeviceForVKM(t)
	hour := time.Date(2026, 8, 1, 15, 0, 0, 0, time.UTC)

	rec := archive.ArchiveRecord{
		Fields: map[string]any{
			"ST":      1.0e+10,
			"ST_unit": "Дж",
			// S deliberately absent
		},
	}

	saved := persistVKMHourly(context.Background(), d, hour, rec)
	if saved != 1 {
		t.Fatalf("expected 1 field saved (ST only), got %d", saved)
	}
}

// TestIsVKMTimeAnomalous_RealExamples — на живых данных: нормальный
// формат (дата со слэшами) и аномальный (голые секунды через дефис),
// оба встречались реально (2026-08-02, несколько раз за один день).
func TestIsVKMTimeAnomalous_RealExamples(t *testing.T) {
	normal := "Time={Время  }28/07/26 15:00:00-28/07/26 15:30:00;Pi={Изб. давление *}4.1494e+05Па;"
	if isVKMTimeAnomalous(normal) {
		t.Fatalf("нормальный формат (со слэшами в дате) ошибочно помечен как аномальный: %q", normal)
	}

	anomalous := "Time=839089620-839089800сек;Pi=425096.5Па;"
	if !isVKMTimeAnomalous(anomalous) {
		t.Fatalf("аномальный формат (голые секунды) не распознан: %q", anomalous)
	}

	// Компактный формат (без {..}) тоже должен корректно распознаваться —
	// боевой режим northbound теперь всегда его использует.
	normalCompact := "Time=28/07/26 15:00:00-28/07/26 15:30:00;Pi=4.1494e+05Па;"
	if isVKMTimeAnomalous(normalCompact) {
		t.Fatalf("нормальный компактный формат ошибочно помечен как аномальный: %q", normalCompact)
	}
}

// TestIsVKMTimeAnomalous_NoTimeField — если поля Time вообще нет,
// не должно быть ложного срабатывания.
func TestIsVKMTimeAnomalous_NoTimeField(t *testing.T) {
	raw := "Pi=4.1494e+05Па;S=1230.7Кг;"
	if isVKMTimeAnomalous(raw) {
		t.Fatalf("отсутствие поля Time не должно считаться аномалией: %q", raw)
	}
}

package device

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"mbgw/internal/archive"
	"mbgw/internal/errs"
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
	if err := repo.InitDeviceConfigSchema(context.Background()); err != nil {
		t.Fatalf("init device config schema: %v", err)
	}
	return &Device{ID: "vkm_test", Repo: repo, Lease: lease.New()}
}

// TestPersistVKMHourly_SavesAllParams confirms S, ST, T, and Pi are ALL
// saved as separate archive_hourly rows (param="S"/"ST"/"T"/"Pi") for the
// requested period.
//
// ИЗМЕНЕНО (2026-08-29, найдено оператором): раньше этот тест
// (назывался TestPersistVKMHourly_SavesBothParams) проверял ОБРАТНОЕ —
// что Pi/T сознательно НЕ сохраняются как мгновенные показания. Это
// решение привело к другому, более заметному багу: вкладка «Архивы» в
// UI показывала для ВКМ только 2 параметра из 4 (масса и тепло),
// давление и температура не отображались вообще. Правильное решение —
// сохранять и их тоже (см. vkmHourlyParams в vkm_hourly.go), а
// физически корректную агрегацию мгновенных показаний (среднее, а не
// сумма, для «по суткам»/«по месяцам») делать отдельно на уровне
// отображения архива (internal/web/api_archive.go, paramAverage) — эта
// функция сама по себе просто сохраняет то, что реально пришло от
// прибора, без какой-либо агрегации.
func TestPersistVKMHourly_SavesAllParams(t *testing.T) {
	d := newTestDeviceForVKM(t)
	hour := time.Date(2026, 8, 1, 14, 0, 0, 0, time.UTC)

	rec := archive.ArchiveRecord{
		Fields: map[string]any{
			"S":       1837.7266,
			"S_unit":  "кг",
			"ST":      5.2585298e+09,
			"ST_unit": "Дж",
			"T":       40.3497543,
			"T_unit":  "°C",
			"Pi":      4.2206e+05,
			"Pi_unit": "Па",
		},
	}

	saved := persistVKMHourly(context.Background(), d, 1, hour, rec)
	if saved != 4 {
		t.Fatalf("expected 4 fields saved (S, ST, T, Pi), got %d", saved)
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

	gotT, err := d.Repo.GetHourlyArchiveDesc(context.Background(), "vkm_test", "", "T", 0, 10)
	if err != nil {
		t.Fatalf("read back T: %v", err)
	}
	if len(gotT) != 1 || gotT[0].Value != 40.3497543 || gotT[0].Unit != "°C" {
		t.Fatalf("unexpected T row: %+v", gotT)
	}

	gotPi, err := d.Repo.GetHourlyArchiveDesc(context.Background(), "vkm_test", "", "Pi", 0, 10)
	if err != nil {
		t.Fatalf("read back Pi: %v", err)
	}
	if len(gotPi) != 1 || gotPi[0].Value != 4.2206e+05 || gotPi[0].Unit != "Па" {
		t.Fatalf("unexpected Pi row: %+v", gotPi)
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

	saved := persistVKMHourly(context.Background(), d, 1, hour, rec)
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

// TestParseVKMPeriodEndTime_SlashFormat — "датный" формат Time= (со
// слэшами) должен разбираться в правильный time.Time.
func TestParseVKMPeriodEndTime_SlashFormat(t *testing.T) {
	raw := "Time={Время  }28/07/26 15:00:00-28/07/26 15:30:00;Pi={Изб. давление *}4.1494e+05Па;"
	got, ok := parseVKMPeriodEndTime(raw)
	if !ok {
		t.Fatal("ожидался успешный разбор для датного формата")
	}
	want := time.Date(2026, 7, 28, 15, 30, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Fatalf("получено %v, ожидалось %v", got, want)
	}
}

// TestParseVKMPeriodEndTime_RawSecondsFormat — реальный пример из живого
// лога boylernaya_par, взятый из ЦЕПОЧКИ последовательных периодов
// подряд (2026-08-31, найдено оператором живьём — предыдущий пример
// "841440600-841442400сек -> 22:30" оказался считан с экрана консоли
// ОШИБОЧНО, со сдвигом на один период; см. подробную историю в
// doc-комментарии vkmRawSecondsEpoch в vkm_hourly.go). Этот пример
// проверен по цепочке из 5+ идущих подряд периодов (каждый следующий
// START в точности равен предыдущему END), что исключает ошибку чтения
// одной отдельной строки.
func TestParseVKMPeriodEndTime_RawSecondsFormat(t *testing.T) {
	raw := "Time=841420800-841422600сек;Pi=413110.938Па;"
	got, ok := parseVKMPeriodEndTime(raw)
	if !ok {
		t.Fatal("ожидался успешный разбор для секундного формата")
	}
	want := time.Date(2026, 8, 29, 16, 30, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Fatalf("получено %v, ожидалось %v", got, want)
	}
}

// TestParseVKMPeriodEndTime_NoTimeField — если поля Time вообще нет,
// разбор должен честно вернуть ok=false, а не панику или мусор.
func TestParseVKMPeriodEndTime_NoTimeField(t *testing.T) {
	raw := "Pi=4.1494e+05Па;S=1230.7Кг;"
	if _, ok := parseVKMPeriodEndTime(raw); ok {
		t.Fatal("ожидался ok=false при отсутствии поля Time")
	}
}

func TestPersistVKMHourly_SeparatesPipes(t *testing.T) {
	d := newTestDeviceForVKM(t)
	ts := time.Date(2026, 9, 24, 12, 30, 0, 0, time.Local)

	pipe1 := archive.ArchiveRecord{Fields: map[string]any{"T": 10.5}}
	pipe2 := archive.ArchiveRecord{Fields: map[string]any{"T": 20.5}}

	if got := persistVKMHourly(context.Background(), d, 1, ts, pipe1); got != 1 {
		t.Fatalf("pipe1 saved=%d, want 1", got)
	}
	if got := persistVKMHourly(context.Background(), d, 2, ts, pipe2); got != 1 {
		t.Fatalf("pipe2 saved=%d, want 1", got)
	}

	rows1, err := d.Repo.GetHourlyArchiveDesc(context.Background(), d.ID, "", "T", 0, 10)
	if err != nil {
		t.Fatalf("read pipe1: %v", err)
	}
	rows2, err := d.Repo.GetHourlyArchiveDesc(context.Background(), d.ID, "2", "T", 0, 10)
	if err != nil {
		t.Fatalf("read pipe2: %v", err)
	}
	if len(rows1) != 1 || rows1[0].Value != 10.5 {
		t.Fatalf("pipe1 rows=%+v, want one value 10.5", rows1)
	}
	if len(rows2) != 1 || rows2[0].Value != 20.5 {
		t.Fatalf("pipe2 rows=%+v, want one value 20.5", rows2)
	}
}

func TestMissingVKMPeriods_UsesRawPresenceNotS(t *testing.T) {
	d := newTestDeviceForVKM(t)
	ctx := context.Background()
	start := time.Date(2026, 9, 24, 10, 0, 0, 0, time.Local)
	label := start.Add(vkmArchivePeriod)

	// A gas pipe may legitimately have no S field. The raw row is the source
	// of truth that the period was collected and must prevent endless backfill.
	if err := d.Repo.SaveVKMRawString(ctx, d.ID, 2, label, "Time=x;V=123.4;"); err != nil {
		t.Fatalf("save raw pipe2: %v", err)
	}

	missing, err := missingVKMPeriods(ctx, d.Repo, d.ID, 2, start, start)
	if err != nil {
		t.Fatalf("missing periods: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("raw period exists but was reported missing: %+v", missing)
	}
}

func TestVKMActivePipesReadsFreshConfiguration(t *testing.T) {
	d := newTestDeviceForVKM(t)
	ctx := context.Background()
	repo := d.Repo.(*sqlite.Repo)

	got := d.vkmActivePipes(ctx)
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("default active pipes=%v, want [1]", got)
	}

	if err := repo.SetVKMActivePipes(ctx, d.ID, []int{1, 2}); err != nil {
		t.Fatalf("set active pipes: %v", err)
	}
	got = d.vkmActivePipes(ctx)
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("active pipes after config change=%v, want [1 2] without restart", got)
	}
}

func TestVKMCatchUpDelayedSeventyMinutesReadsAllMissingPeriodsOnAllPipes(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 26, 12, 10, 0, 0, time.Local)
	want := []time.Time{
		time.Date(2026, 9, 26, 10, 30, 0, 0, time.Local),
		time.Date(2026, 9, 26, 11, 0, 0, 0, time.Local),
		time.Date(2026, 9, 26, 11, 30, 0, 0, time.Local),
	}
	var got []string
	err := runVKMCatchUpAt(ctx, now, 24, []int{1, 2},
		func(_ context.Context, pipe int, from, to time.Time) ([]time.Time, error) {
			if to != time.Date(2026, 9, 26, 11, 30, 0, 0, time.Local) {
				t.Fatalf("last completed=%v", to)
			}
			return append([]time.Time(nil), want...), nil
		},
		func(_ context.Context, pipe int, periodStart time.Time) (int, error) {
			got = append(got, fmt.Sprintf("%d/%s", pipe, periodStart.Format("15:04")))
			return 1, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want)*2 {
		t.Fatalf("collected=%v, want %d periods", got, len(want)*2)
	}
	for i, pipe := range []int{1, 2} {
		for j, period := range want {
			wantKey := fmt.Sprintf("%d/%s", pipe, period.Format("15:04"))
			if got[i*len(want)+j] != wantKey {
				t.Fatalf("got[%d]=%q want %q (all=%v)", i*len(want)+j, got[i*len(want)+j], wantKey, got)
			}
		}
	}
}

func TestVKMCatchUpStopsEachPipeAfterCommunicationFailureAndCapsDepth(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 26, 12, 10, 0, 0, time.Local)
	last := time.Date(2026, 9, 26, 11, 30, 0, 0, time.Local)
	periods := []time.Time{last.Add(-time.Hour), last.Add(-30 * time.Minute), last}

	missingCalls := 0
	collectCalls := 0
	err := runVKMCatchUpAt(ctx, now, 648, []int{1, 2},
		func(_ context.Context, pipe int, from, to time.Time) ([]time.Time, error) {
			missingCalls++
			if to != last {
				t.Fatalf("pipe %d last=%v want %v", pipe, to, last)
			}
			if got := to.Sub(from); got != 23*time.Hour+30*time.Minute {
				t.Fatalf("scheduled catch-up depth=%v want 23h30m (24h/48 half-hours)", got)
			}
			return append([]time.Time(nil), periods...), nil
		},
		func(_ context.Context, pipe int, periodStart time.Time) (int, error) {
			collectCalls++
			return 0, fmt.Errorf("pipe %d %s: %w", pipe, periodStart.Format("15:04"), errs.ErrTimeout)
		})
	if err != nil {
		t.Fatal(err)
	}
	if missingCalls != 2 {
		t.Fatalf("missing calls=%d want 2", missingCalls)
	}
	if collectCalls != 2 {
		t.Fatalf("unreachable meter must get one failed period per pipe, calls=%d want 2", collectCalls)
	}
}

func TestMissingVKMPeriodsSkipsConfirmedNoRecordsUntilRetryTime(t *testing.T) {
	d := newTestDeviceForVKM(t)
	ctx := context.Background()
	repo := d.Repo.(*sqlite.Repo)
	start := time.Date(2026, 9, 26, 10, 0, 0, 0, time.Local)
	label := start.Add(vkmArchivePeriod)
	if err := repo.MarkVKMNoRecords(ctx, d.ID, 1, label, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	missing, err := missingVKMPeriods(ctx, d.Repo, d.ID, 1, start, start)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Fatalf("confirmed no-record period must be suppressed, got %v", missing)
	}
	if err := repo.MarkVKMNoRecords(ctx, d.ID, 1, label, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	missing, err = missingVKMPeriods(ctx, d.Repo, d.ID, 1, start, start)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 || !missing[0].Equal(start) {
		t.Fatalf("expired no-record marker must permit rare retry, got %v", missing)
	}
}

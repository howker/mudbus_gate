package northbound

import (
	"context"
	"testing"
	"time"
)

func TestDBVKMArchiveSource_FindsSavedString(t *testing.T) {
	repo := &fakeRepo{}
	hour := time.Date(2026, 8, 1, 14, 0, 0, 0, time.UTC)
	stored := "Pi=<Изб. давление *>4.2229e+05Па;S={Масса теплонос. }1837.7266кг;"
	// Боевая ветка убирает блоки-заголовки {..}/<..> перед отдачей в ЭС —
	// ПОДТВЕРЖДЕНО живым перебором (2026-08-02): драйвер ЭС принимает
	// именно компактный формат без них.
	want := "Pi=4.2229e+05Па;S=1837.7266кг;"

	if err := repo.SaveVKMRawString(context.Background(), "vkm360_real", 1, hour, stored); err != nil {
		t.Fatalf("seed error: %v", err)
	}

	src := NewDBVKMArchiveSource(repo, "vkm360_real")

	// Запрос ровно на границе часа — должен найти сохранённое.
	got, ok := src.Archive(1, hour, hour.Add(time.Hour), 0)
	if !ok {
		t.Fatal("expected ok=true for a stored hour")
	}
	if got != want {
		t.Fatalf("unexpected raw string: got %q, want %q", got, want)
	}
}

func TestDBVKMArchiveSource_NoRecordsWhenMissing(t *testing.T) {
	repo := &fakeRepo{}
	src := NewDBVKMArchiveSource(repo, "vkm360_real")

	hour := time.Date(2026, 8, 1, 15, 0, 0, 0, time.UTC)
	_, ok := src.Archive(1, hour, hour.Add(time.Hour), 0)
	if ok {
		t.Fatal("expected ok=false when nothing is stored for that hour")
	}
}

// TestDBVKMArchiveSource_DifferentPipesAreIsolated confirms данные одной
// трубы не подмешиваются к другой — разные ЭС-каналы на одном приборе не
// должны видеть чужой архив.
func TestDBVKMArchiveSource_DifferentPipesAreIsolated(t *testing.T) {
	repo := &fakeRepo{}
	hour := time.Date(2026, 8, 1, 16, 0, 0, 0, time.UTC)

	if err := repo.SaveVKMRawString(context.Background(), "vkm360_real", 1, hour, "pipe1 data"); err != nil {
		t.Fatalf("seed error: %v", err)
	}

	src := NewDBVKMArchiveSource(repo, "vkm360_real")

	if _, ok := src.Archive(2, hour, hour.Add(time.Hour), 0); ok {
		t.Fatal("expected ok=false for a different pipe with nothing stored")
	}
	if got, ok := src.Archive(1, hour, hour.Add(time.Hour), 0); !ok || got != "pipe1 data" {
		t.Fatalf("expected pipe1's own data, got %q ok=%v", got, ok)
	}
}

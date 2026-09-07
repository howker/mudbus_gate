package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRussianLogWriterTranslatesOperatorTags(t *testing.T) {
	var dst bytes.Buffer
	w := newRussianLogWriter(&dst)
	input := "2026/09/07 15:18:13 [SAVE] V=1 [ERROR] database is locked [WEB] запрос [poller] запуск [es-sync] проход\n"
	if n, err := w.Write([]byte(input)); err != nil || n != len(input) {
		t.Fatalf("Write: n=%d err=%v", n, err)
	}
	got := dst.String()
	for _, want := range []string{"[СОХРАНЕНО]", "[ОШИБКА]", "[ВЕБ]", "[ОПРОС]", "[СИНХРОНИЗАЦИЯ С ЭС]"} {
		if !strings.Contains(got, want) {
			t.Fatalf("в строке нет русской метки %q: %q", want, got)
		}
	}
	for _, stale := range []string{"[SAVE]", "[ERROR]", "[WEB]", "[poller]", "[es-sync]"} {
		if strings.Contains(got, stale) {
			t.Fatalf("в строке осталась старая метка %q: %q", stale, got)
		}
	}
}

func TestRussianLogWriterKeepsTechnicalNames(t *testing.T) {
	var dst bytes.Buffer
	w := newRussianLogWriter(&dst)
	input := "[ERROR] Modbus SQL Server ID_PP PointMains SQLITE_BUSY\n"
	_, _ = w.Write([]byte(input))
	got := dst.String()
	for _, technical := range []string{"Modbus", "SQL Server", "ID_PP", "PointMains", "SQLITE_BUSY"} {
		if !strings.Contains(got, technical) {
			t.Fatalf("техническое имя %q было изменено: %q", technical, got)
		}
	}
}
func TestRussianLogWriterTranslatesLegacyArchivePhrases(t *testing.T) {
	var dst bytes.Buffer
	w := newRussianLogWriter(&dst)
	input := "gap-scan: VKM архив main: VKM дозабор main: архив hourly: дозабор hourly:\n"
	_, _ = w.Write([]byte(input))
	got := dst.String()
	for _, want := range []string{"проверка пропусков:", "архив ВКМ:", "дозабор архива ВКМ:", "часовой архив:", "дозабор часового архива:"} {
		if !strings.Contains(got, want) {
			t.Fatalf("в строке нет русского текста %q: %q", want, got)
		}
	}
	for _, stale := range []string{"gap-scan:", "VKM архив main:", "VKM дозабор main:", "архив hourly:", "дозабор hourly:"} {
		if strings.Contains(got, stale) {
			t.Fatalf("в строке остался legacy-текст %q: %q", stale, got)
		}
	}
}

package northbound

import (
	"strings"
	"testing"
)

// TestNormalizeVKMNumbers_RealStuckPeriod — на данных живого захвата
// (2026-08-02, период 30.07 08:30, на котором ЭС бесконечно повторяла
// запрос): dP было целым (12397), без точки. Проверяем, что после
// нормализации оно получает ".0", а поле Time (дата, не число) остаётся
// нетронутым.
func TestNormalizeVKMNumbers_RealStuckPeriod(t *testing.T) {
	raw := "Time={Время  }30/07/26 08:30:00-30/07/26 09:00:00;" +
		"Pi={Изб. давление *}4.2474e+05Па;" +
		"dP={Перепад давления *}12397Па;" +
		"S={Масса теплонос. }1242.6656кг;" +
		"S_ns={Масса теплонос. при НС}0кг;" +
		"Twrk={Время штатной работы}30м 00сек;"

	got := normalizeVKMNumbers(raw)

	// dP: целое -> получает .0
	wantDP := "dP={Перепад давления *}12397.0Па"
	if !strings.Contains(got, wantDP) {
		t.Fatalf("dP не нормализовалось: получено %q, ожидалось вхождение %q", got, wantDP)
	}
	// S_ns: 0 -> 0.0
	wantSns := "S_ns={Масса теплонос. при НС}0.0кг"
	if !strings.Contains(got, wantSns) {
		t.Fatalf("S_ns не нормализовалось: получено %q, ожидалось вхождение %q", got, wantSns)
	}
	// Time НЕ должен быть тронут — там дата, не число.
	wantTime := "Time={Время  }30/07/26 08:30:00-30/07/26 09:00:00"
	if !strings.Contains(got, wantTime) {
		t.Fatalf("поле Time было испорчено: получено %q", got)
	}
	// Уже дробное (Pi, S) число не должно задваивать точку.
	if strings.Contains(got, "4.2474e+05.0") || strings.Contains(got, "1242.6656.0") {
		t.Fatalf("уже дробное число было испорчено повторной точкой: %q", got)
	}
}

// TestNormalizeVKMNumbers_AlreadyAcceptedPeriodUnchanged — период, который
// ЭС УЖЕ успешно приняла (все значения дробные) — normalizeVKMNumbers не
// должен ничего в нём менять.
func TestNormalizeVKMNumbers_AlreadyAcceptedPeriodUnchanged(t *testing.T) {
	raw := "Time={Время  }01/08/26 19:00:00-01/08/26 19:30:00;" +
		"Pi={Изб. давление *}4.2126e+05Па;" +
		"dP={Перепад давления *}6566.1Па;" +
		"S={Масса теплонос. }929.72064кг;"

	got := normalizeVKMNumbers(raw)
	if got != raw {
		t.Fatalf("уже дробная строка была изменена:\nбыло:  %q\nстало: %q", raw, got)
	}
}

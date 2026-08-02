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

	got := vkmAddDecimalPoint(raw)

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
// ЭС УЖЕ успешно приняла (все значения дробные) — vkmAddDecimalPoint не
// должен ничего в нём менять.
func TestNormalizeVKMNumbers_AlreadyAcceptedPeriodUnchanged(t *testing.T) {
	raw := "Time={Время  }01/08/26 19:00:00-01/08/26 19:30:00;" +
		"Pi={Изб. давление *}4.2126e+05Па;" +
		"dP={Перепад давления *}6566.1Па;" +
		"S={Масса теплонос. }929.72064кг;"

	got := vkmAddDecimalPoint(raw)
	if got != raw {
		t.Fatalf("уже дробная строка была изменена:\nбыло:  %q\nстало: %q", raw, got)
	}
}

// TestNormalizeVKMLineBreaks_RealCorrectionSuffix — regression-тест на
// живой находке (2026-08-02): период с суффиксом "кор.времени" содержал
// буквальный CRLF прямо в данных, перед завершающей ';'. Именно этот
// период ЭС бесконечно переспрашивала (id дошёл до 418), пока соседний
// период без такого суффикса принимался нормально.
func TestNormalizeVKMLineBreaks_RealCorrectionSuffix(t *testing.T) {
	raw := "Twrk={Время штатной работы}29м 57сек;Tnss={Время нештатных ситуаций}0сек;" +
		"NSS={Нештатные ситуации(время)  Сообщения[время]}кор.времени\r\n;"

	got := vkmStripLineBreaks(raw)

	if strings.Contains(got, "\r") || strings.Contains(got, "\n") {
		t.Fatalf("перенос строки не убран: %q", got)
	}
	// Сам текст "кор.времени" должен сохраниться — убираем только разрыв строки.
	if !strings.Contains(got, "кор.времени") {
		t.Fatalf("текст 'кор.времени' был утрачен: %q", got)
	}
	// Завершающая ';' должна остаться на месте (была после \r\n).
	if !strings.HasSuffix(got, ";") {
		t.Fatalf("строка должна заканчиваться на ';': %q", got)
	}
}

// TestVKMVariants_AllProduceValidStrings — базовая проверка всех вариантов
// перебора: каждый должен вернуть непустую строку, сохранить поле Time
// с исходным диапазоном дат и не оставить в данных переносов строк.
func TestVKMVariants_AllProduceValidStrings(t *testing.T) {
	raw := "Time={Время  }27/07/26 16:00:00-27/07/26 16:30:00;" +
		"Pi={Изб. давление *}4.1042e+05Па;" +
		"dP={Перепад давления *}7972Па;" +
		"S={Масса теплонос. }1006.4156кг;" +
		"Twrk={Время штатной работы}29м 57сек;" +
		"NSS={Нештатные ситуации(время)  Сообщения[время]}кор.времени\r\n;"

	for _, v := range vkmVariants {
		if v.fn == nil {
			continue // вариант 8 требует доступа к БД, проверяется отдельно
		}
		got := v.fn(raw)
		if got == "" {
			t.Fatalf("вариант %s вернул пустую строку", v.name)
		}
		if !strings.Contains(got, "27/07/26 16:00:00-27/07/26 16:30:00") {
			t.Fatalf("вариант %s потерял диапазон времени: %q", v.name, got)
		}
		// Вариант 1 (контроль) намеренно отдаёт строку как есть, включая CRLF.
		if v.name != "1-as-is" && (strings.Contains(got, "\r") || strings.Contains(got, "\n")) {
			t.Fatalf("вариант %s оставил перенос строки: %q", v.name, got)
		}
	}
}

// TestVKMForceFullTwrk проверяет, что Twrk приводится к полному периоду,
// а остальные поля не задеваются.
func TestVKMForceFullTwrk(t *testing.T) {
	raw := "Twrk={Время штатной работы}29м 57сек;Tnss={Время нештатных ситуаций}0сек;"
	got := vkmForceFullTwrk(raw)
	if !strings.Contains(got, "Twrk={Время штатной работы}30м 00сек") {
		t.Fatalf("Twrk не приведён к полному периоду: %q", got)
	}
	if !strings.Contains(got, "Tnss={Время нештатных ситуаций}0сек") {
		t.Fatalf("задето соседнее поле Tnss: %q", got)
	}
}

// TestVKMStripAllHeaders проверяет компактный формат (без блоков {..}).
func TestVKMStripAllHeaders(t *testing.T) {
	raw := "Pi={Изб. давление *}4.1042e+05Па;S={Масса теплонос. }1006.4156кг;"
	got := vkmStripAllHeaders(raw)
	want := "Pi=4.1042e+05Па;S=1006.4156кг;"
	if got != want {
		t.Fatalf("компактный формат неверен:\nполучено: %q\nожидалось: %q", got, want)
	}
}

// TestVKMEmptyNSS проверяет полное опустошение поля NSS.
func TestVKMEmptyNSS(t *testing.T) {
	raw := "Tnss={Время нештатных ситуаций}0сек;NSS={Нештатные ситуации(время)}кор.времени;"
	got := vkmEmptyNSS(raw)
	if !strings.Contains(got, "NSS=;") {
		t.Fatalf("поле NSS не опустошено: %q", got)
	}
	if !strings.Contains(got, "Tnss={Время нештатных ситуаций}0сек") {
		t.Fatalf("задето соседнее поле Tnss: %q", got)
	}
}

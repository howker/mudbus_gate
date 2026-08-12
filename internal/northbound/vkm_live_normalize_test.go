package northbound

import (
	"strings"
	"testing"
	"time"
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

// TestVKMExpandExponent_RealValues — на реальных значениях из архива ВКМ
// (2026-08-02). Гипотеза по багу нулей: ЭС не разбирает научную нотацию.
// Разворот e+NN в обычное число должен затронуть ТОЛЬКО экспоненциальные
// значения, не трогая обычные десятичные (которые уже работали — S, T).
func TestVKMExpandExponent_RealValues(t *testing.T) {
	cases := map[string]string{
		"Pi=4.2126e+05Па":   "Pi=421260Па",
		"ST=2.658389e+09Дж": "ST=2658389000Дж",
		"Pbar=1.0092e+05Па": "Pbar=100920Па",
		"H=2.8694e+06Дж/кг": "H=2869400Дж/кг",
		// Обычные числа не трогаем.
		"dP=6338.6Па":   "dP=6338.6Па",
		"S=929.72064кг": "S=929.72064кг",
		"T=202.16°С":    "T=202.16°С",
	}
	for in, want := range cases {
		got := vkmExpandExponent(in)
		if got != want {
			t.Errorf("vkmExpandExponent(%q) = %q, ожидалось %q", in, got, want)
		}
	}
}

// TestVKMExpandExponent_FullString — на целой архивной строке: все
// экспоненциальные поля развёрнуты, структура (теги, ;, единицы) цела.
func TestVKMExpandExponent_FullString(t *testing.T) {
	raw := "Time=02/08/26 05:30:00-02/08/26 06:00:00;Pi=4.27e+05Па;Pbar=1.0092e+05Па;" +
		"T=206.98°С;dP=6338.6Па;S=913.77557кг;ST=2.6219832e+09Дж;"
	got := vkmExpandExponent(raw)

	if strings.Contains(got, "e+") || strings.Contains(got, "E+") {
		t.Fatalf("осталась экспоненциальная запись: %q", got)
	}
	// Дата в поле Time (со слэшами и двоеточиями) не должна пострадать.
	if !strings.Contains(got, "02/08/26 05:30:00-02/08/26 06:00:00") {
		t.Fatalf("поле Time повреждено: %q", got)
	}
	// Обычные числа на месте.
	if !strings.Contains(got, "S=913.77557кг") || !strings.Contains(got, "dP=6338.6Па") {
		t.Fatalf("обычные числа пострадали: %q", got)
	}
	// Экспоненциальные развёрнуты.
	if !strings.Contains(got, "Pi=427000Па") {
		t.Fatalf("Pi не развёрнут: %q", got)
	}
}

// TestApplyNumFormatProbe_DistributesFormats проверяет, что режим перебора
// формата раздаёт РАЗНЫЕ форматы по РАЗНЫМ экспоненциальным полям одной
// строки, не трогая обычные (S, T) и структуру.
func TestApplyNumFormatProbe_DistributesFormats(t *testing.T) {
	repo := &fakeRepo{}
	src := NewDBVKMArchiveSource(repo, "vkm360_real")
	src.NumFormatProbe = true
	// NumProbeLogPath пустой -> лог идёт в обычный log, файл не трогаем.

	raw := "Time=02/08/26 05:30:00-02/08/26 06:00:00;" +
		"Pi=4.27e+05Па;Pbar=1.0092e+05Па;T=206.98°С;S=913.77557кг;ST=2.6219832e+09Дж;"

	period := timeParseTest(t, "2026-08-02 05:30")
	got := src.applyNumFormatProbe(period, raw)

	// Обычные поля не тронуты.
	if !strings.Contains(got, "S=913.77557кг") || !strings.Contains(got, "T=206.98°С") {
		t.Fatalf("обычные поля пострадали: %q", got)
	}
	// Дата цела.
	if !strings.Contains(got, "02/08/26 05:30:00-02/08/26 06:00:00") {
		t.Fatalf("дата пострадала: %q", got)
	}
	// Экспоненциальные поля больше НЕ должны все совпадать по формату —
	// Pi получил формат A (целое), Pbar формат B и т.д. Проверяем, что Pi
	// стало целым (первый формат в списке — "A-целое-без-точки").
	if !strings.Contains(got, "Pi=427000Па") {
		t.Fatalf("Pi должен был получить формат A (целое 427000): %q", got)
	}
	// ST не должно остаться в исходной научной нотации с маленькой 'e'.
	if strings.Contains(got, "ST=2.6219832e+09") {
		t.Fatalf("ST не переформатирован: %q", got)
	}
}

func timeParseTest(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		t.Fatalf("time parse: %v", err)
	}
	return tm
}

// TestVKMScaleFields_ScalesOnlyNamedTag проверяет, что масштабируется
// только указанный тег, включая похожие имена (S_ns при масштабировании S).
func TestVKMScaleFields_ScalesOnlyNamedTag(t *testing.T) {
	raw := "S=929.72064кг;S_ns=0кг;ST=2.6219832e+09Дж;"
	scale := map[string]float64{"ST": 0.001}

	got := vkmScaleFields(raw, scale)

	if !strings.Contains(got, "S=929.72064кг") {
		t.Fatalf("S не должен был измениться: %q", got)
	}
	if !strings.Contains(got, "S_ns=0кг") {
		t.Fatalf("S_ns не должен был измениться (похожее имя на S): %q", got)
	}
	if !strings.Contains(got, "ST=2.6219832e+06Дж") {
		t.Fatalf("ST должен был уменьшиться в 1000 раз: %q", got)
	}
}

// TestVKMScaleFields_PreservesHeader проверяет, что при оставленной шапке
// масштабирование трогает только число, шапка остаётся на месте.
func TestVKMScaleFields_PreservesHeader(t *testing.T) {
	raw := "ST={Тепловая энергия }2.6219832e+09Дж;"
	scale := map[string]float64{"ST": 0.001}

	got := vkmScaleFields(raw, scale)
	want := "ST={Тепловая энергия }2.6219832e+06Дж;"
	if got != want {
		t.Fatalf("получено %q, ожидалось %q", got, want)
	}
}

// TestVKMScaleFields_PlainNumber проверяет масштабирование обычного (не
// экспоненциального) числа.
func TestVKMScaleFields_PlainNumber(t *testing.T) {
	raw := "S=929.72064кг;"
	scale := map[string]float64{"S": 2}

	got := vkmScaleFields(raw, scale)
	if !strings.Contains(got, "S=1859.44128кг") {
		t.Fatalf("S не удвоился корректно: %q", got)
	}
}

// TestVKMOverrideFields_ReplacesOnlyNamedTag проверяет прямую подмену
// значения (не масштабирование) — для чистой диагностики (2026-08-10):
// подставить простое целое ("77") вместо реального значения ST/Pi,
// исключая любые побочные факторы формы записи исходного числа.
//
// ОБНОВЛЕНО (2026-08-11): replacement теперь заменяет число И единицу
// целиком (не сохраняет старую единицу автоматически) — нужно для
// проверки гипотезы "дело в единице измерения" (ST как Гкал вместо Дж).
// Раньше "ST:77" всегда давало "ST=77Дж" (единица прибора сохранялась);
// теперь она заменяется полностью тем, что указано в overrides, поэтому
// тесты передают replacement уже вместе с нужной единицей ("77Дж",
// "77Па"), а не голым числом.
func TestVKMOverrideFields_ReplacesOnlyNamedTag(t *testing.T) {
	raw := "Pi=418657.969Па;S=1279.54846кг;ST=3.75875712e+09Дж;"
	overrides := map[string]string{"ST": "77Дж", "Pi": "77Па"}

	got := vkmOverrideFields(raw, overrides)

	if !strings.Contains(got, "ST=77Дж") {
		t.Fatalf("ST не заменён на 77Дж: %q", got)
	}
	if !strings.Contains(got, "Pi=77Па") {
		t.Fatalf("Pi не заменён на 77Па: %q", got)
	}
	if !strings.Contains(got, "S=1279.54846кг") {
		t.Fatalf("S не должен был измениться (не указан в overrides): %q", got)
	}
}

// TestVKMOverrideFields_PreservesHeader проверяет, что при оставленной
// шапке подмена трогает число и единицу, а шапка остаётся на месте.
func TestVKMOverrideFields_PreservesHeader(t *testing.T) {
	raw := "ST={Тепловая энергия }3.75875712e+09Дж;"
	overrides := map[string]string{"ST": "77Дж"}

	got := vkmOverrideFields(raw, overrides)
	want := "ST={Тепловая энергия }77Дж;"
	if got != want {
		t.Fatalf("получено %q, ожидалось %q", got, want)
	}
}

// TestVKMOverrideFields_ReplacesUnitToo — новый тест (2026-08-11): проверяет
// именно то, ради чего расширили функцию — замену единицы измерения, не
// только числа (для проверки гипотезы "ST должен быть в Гкал, не в Дж").
func TestVKMOverrideFields_ReplacesUnitToo(t *testing.T) {
	raw := "ST=3.75875712e+09Дж;"
	overrides := map[string]string{"ST": "0.077Гкал"}

	got := vkmOverrideFields(raw, overrides)
	want := "ST=0.077Гкал;"
	if got != want {
		t.Fatalf("единица не заменена: получено %q, ожидалось %q", got, want)
	}
}

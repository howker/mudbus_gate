package web

import (
	"testing"
	"time"
)

// TestFormatPeriodLabel_HourlyGroupsBothHalves — регрессионный тест на
// реальный баг (2026-08-27): после смены соглашения о метке периода на
// "конец интервала" (см. persistVKMHourly в internal/device/vkm_hourly.go)
// группировка "По часам" стала неверно разносить две получасовки одного
// часа по РАЗНЫМ корзинам, если метка одной из них приходилась ровно на
// границу часа — час на экране показывал примерно половину правильной
// суммы. Проверяет, что получасовки с метками "16:30" и "17:00" (обе
// принадлежат интервалу [16:00,17:00)) попадают в ОДНУ И ТУ ЖЕ часовую
// корзину, подписанную "17:00" (конец часа, как и везде в системе).
func TestFormatPeriodLabel_HourlyGroupsBothHalves(t *testing.T) {
	loc := time.Local

	firstHalf := time.Date(2026, 8, 27, 16, 30, 0, 0, loc) // конец интервала [16:00,16:30)
	secondHalf := time.Date(2026, 8, 27, 17, 0, 0, 0, loc) // конец интервала [16:30,17:00)

	labelFirst := formatPeriodLabel(firstHalf, "hourly")
	labelSecond := formatPeriodLabel(secondHalf, "hourly")

	if labelFirst != labelSecond {
		t.Fatalf("обе получасовки одного часа [16:00,17:00) должны попадать в ОДНУ корзину при группировке 'По часам', получили разные: %q и %q — значит одна из них улетает в соседний час, и оба часа на экране показывают примерно половину правильной суммы (реальный баг 2026-08-27)", labelFirst, labelSecond)
	}

	wantLabel := "2026-08-27 17:00" // подпись корзины — КОНЕЦ часа, как и везде в системе (Akron, "raw", "daily")
	if labelFirst != wantLabel {
		t.Errorf("метка часовой корзины = %q, ожидалось %q (час [16:00,17:00) должен подписываться концом — 17:00)", labelFirst, wantLabel)
	}
}

// TestFormatPeriodLabel_HourlyPassthroughForAkron — Akron уже отдаёт
// часовые записи с меткой ровно на границе часа (например "17:00" для
// интервала [16:00,17:00)) — группировка "По часам" для него должна быть
// прозрачной (тот же час на входе, тот же час на выходе), фикс от
// 2026-08-27 не должен был это сломать.
func TestFormatPeriodLabel_HourlyPassthroughForAkron(t *testing.T) {
	loc := time.Local
	akronHourly := time.Date(2026, 8, 27, 17, 0, 0, 0, loc)

	got := formatPeriodLabel(akronHourly, "hourly")
	want := "2026-08-27 17:00"
	if got != want {
		t.Errorf("часовая метка Akron изменилась при группировке 'По часам': получили %q, ожидалось %q (должна остаться неизменной)", got, want)
	}
}

// TestFormatPeriodLabel_HourlyDoesNotMergeDifferentHours — соседние часы
// НЕ должны схлопываться в одну корзину (обратная сторона фикса — легко
// было бы "перелечить" и случайно смешать всё подряд).
func TestFormatPeriodLabel_HourlyDoesNotMergeDifferentHours(t *testing.T) {
	loc := time.Local
	endOf16 := time.Date(2026, 8, 27, 17, 0, 0, 0, loc) // конец часа [16:00,17:00)
	endOf17 := time.Date(2026, 8, 27, 18, 0, 0, 0, loc) // конец часа [17:00,18:00)

	label16 := formatPeriodLabel(endOf16, "hourly")
	label17 := formatPeriodLabel(endOf17, "hourly")

	if label16 == label17 {
		t.Fatalf("соседние часы схлопнулись в одну корзину: %q — фикс группировки применён слишком широко", label16)
	}
}

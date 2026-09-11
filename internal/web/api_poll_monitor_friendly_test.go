package web

import "testing"

func TestFriendlyPollErrorHidesInternalLeaseDetails(t *testing.T) {
	raw := `прибор занят другим опросом: после 8 попыток: lease held for device "dev1" requested context "hourly"`
	if got := friendlyPollError(raw); got != "Прибор занят другим опросом" {
		t.Fatalf("friendlyPollError() = %q", got)
	}
}

func TestFriendlyPollErrorFallbackDoesNotLeakTechnicalText(t *testing.T) {
	raw := "some internal english error: context=foo retry=7"
	if got := friendlyPollError(raw); got != "Ошибка опроса — подробности в журнале" {
		t.Fatalf("friendlyPollError() = %q", got)
	}
}

func TestFriendlyTimeDriftNote(t *testing.T) {
	if got := friendlyTimeDriftNote("не удалось прочитать текущие часы ВКМ: i/o timeout", false); got != "Не удалось проверить время прибора" {
		t.Fatalf("friendlyTimeDriftNote() = %q", got)
	}
}

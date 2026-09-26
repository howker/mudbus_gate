package web

import (
	"strings"
	"testing"
)

func TestAdminUIVKMPointEditorHasTenPipesFourSlotsAndES5Toggle(t *testing.T) {
	checks := []string{
		"for (var pipe = 1; pipe <= 10; pipe++)",
		"for (var slot = 1; slot <= 4; slot++)",
		"toggleVKMPipe(pipe)",
		"ch_pipe_' + pipe + '_active",
		"pipe_no: pipe",
		"slot_no: slot",
		"active_pipes",
		"/api/vkm-source-tags?device_id=",
		"Обновить список параметров",
		"Пересканировать трубопроводы",
		"rescanVKMPipes()",
		"/api/vkm-pipe-discovery",
	}
	for _, want := range checks {
		if !strings.Contains(adminUIHTML, want) {
			t.Fatalf("admin UI missing VKM multipipe marker %q", want)
		}
	}
	for _, forbidden := range []string{"<th>Мин.</th>", "<th>Макс.</th>", "Обновить параметры из архива", "Пустой архивный период не считается отсутствием трубопровода"} {
		if strings.Contains(adminUIHTML, forbidden) {
			t.Fatalf("admin UI still contains removed VKM text %q", forbidden)
		}
	}
	if strings.Contains(adminUIHTML, "<details") || strings.Contains(adminUIHTML, "<summary") {
		t.Fatal("VKM pipe editor must remain ES5/old-IE compatible; details/summary found")
	}
}

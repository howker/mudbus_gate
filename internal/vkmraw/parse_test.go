package vkmraw

import "testing"

func TestParseHeadersUnitsAndGasVolume(t *testing.T) {
	raw := "Time=01/09/26 00:00:00-01/09/26 00:30:00;" +
		"Pi=<Изб. давление *>4.2097e+05Па;" +
		"V={Объём газа ст.у. }12.345м³;" +
		"T={Температура }9.57435°C;NSS=;"
	got := Parse(raw)
	if len(got) != 3 {
		t.Fatalf("fields=%v", got)
	}
	if got[0].Tag != "Pi" || got[0].Header != "Изб. давление" || got[0].Unit != "Па" || got[0].Value != 420970 {
		t.Fatalf("Pi=%+v", got[0])
	}
	if got[1].Tag != "V" || got[1].Header != "Объём газа ст.у." || got[1].Unit != "м³" || got[1].Value != 12.345 {
		t.Fatalf("V=%+v", got[1])
	}
}

func TestParseHeaderBeforeEqualsAndNumericTags(t *testing.T) {
	raw := "V{Объём газа}=2.5 м3;T=10.25°C;Time=1-2сек;"
	fields := Parse(raw)
	if len(fields) != 2 || fields[0].Tag != "V" || fields[0].Header != "Объём газа" || fields[0].Unit != "м3" {
		t.Fatalf("fields=%+v", fields)
	}
	tags := NumericTags(raw)
	if len(tags) != 2 || tags[0] != "V" || tags[1] != "T" {
		t.Fatalf("tags=%v", tags)
	}
	if v, ok := Float(raw, "T"); !ok || v != 10.25 {
		t.Fatalf("T=%v ok=%v", v, ok)
	}
}

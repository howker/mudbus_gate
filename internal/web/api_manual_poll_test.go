package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleDevicePollNowQueuesOneDevice(t *testing.T) {
	var got string
	s := &Server{}
	s.SetManualDevicePoll(func(deviceID string) error {
		got = deviceID
		return nil
	})

	req := httptest.NewRequest(http.MethodPost, "/api/devices/poll-now", strings.NewReader(`{"device_id":"ivk_ter_1"}`))
	rec := httptest.NewRecorder()
	s.handleDevicePollNow(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got != "ivk_ter_1" {
		t.Fatalf("callback device=%q, want ivk_ter_1", got)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["status"] != "queued" {
		t.Fatalf("status=%q, want queued", body["status"])
	}
}

func TestHandleDevicePollNowRejectsMissingDevice(t *testing.T) {
	s := &Server{}
	s.SetManualDevicePoll(func(string) error { return nil })
	req := httptest.NewRequest(http.MethodPost, "/api/devices/poll-now", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	s.handleDevicePollNow(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want %d", rec.Code, http.StatusBadRequest)
	}
}

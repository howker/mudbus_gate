package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	sqliterepo "mbgw/internal/storage/sqlite"
)

func TestHandleVKMPipeDiscoveryPersistsScanWithoutChangingActivePipes(t *testing.T) {
	s, repo := newVKMMultipipeWebTestServer(t)
	s.SetVKMPipeScanner(func(_ context.Context, deviceID string) ([]VKMPipeScanResult, error) {
		if deviceID != "vkm_multi" {
			t.Fatalf("deviceID=%q, want vkm_multi", deviceID)
		}
		out := make([]VKMPipeScanResult, 0, 10)
		for pipe := 1; pipe <= 10; pipe++ {
			status := sqliterepo.VKMDiscoveryAbsent
			if pipe == 1 {
				status = sqliterepo.VKMDiscoveryAvailable
			} else if pipe == 2 {
				// Pipe 2 is already active for normal polling, but two empty
				// periods made discovery inconclusive. Rescan must not disable it.
				status = sqliterepo.VKMDiscoveryUncertain
			}
			raw := ""
			if status == sqliterepo.VKMDiscoveryAvailable {
				raw = "T=12.5;Pi=101325;"
			}
			out = append(out, VKMPipeScanResult{PipeNo: pipe, Status: status, Raw: raw})
		}
		return out, nil
	})

	req := httptest.NewRequest(http.MethodPost, "/api/vkm-pipe-discovery", bytes.NewReader([]byte(`{"device_id":"vkm_multi"}`)))
	rr := httptest.NewRecorder()
	s.handleVKMPipeDiscovery(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	var got []sqliterepo.VKMPipeDiscoveryRecord
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != 10 || got[0].Status != sqliterepo.VKMDiscoveryAvailable || got[1].Status != sqliterepo.VKMDiscoveryUncertain || got[2].Status != sqliterepo.VKMDiscoveryAbsent {
		t.Fatalf("scan result=%+v", got)
	}

	active, err := repo.GetVKMActivePipes(context.Background(), "vkm_multi")
	if err != nil {
		t.Fatalf("get active pipes: %v", err)
	}
	if len(active) != 2 || active[0] != 1 || active[1] != 2 {
		t.Fatalf("scan changed active pipes: %v", active)
	}

	stored, err := repo.GetVKMPipeDiscovery(context.Background(), "vkm_multi")
	if err != nil {
		t.Fatalf("get stored discovery: %v", err)
	}
	if len(stored) != 10 {
		t.Fatalf("stored discovery rows=%d, want 10", len(stored))
	}
	if len(stored[0].Tags) != 2 || stored[0].Tags[0] != "T" || stored[0].Tags[1] != "Pi" {
		t.Fatalf("stored pipe1 tags=%v, want [T Pi]", stored[0].Tags)
	}

	tagsReq := httptest.NewRequest(http.MethodGet, "/api/vkm-source-tags?device_id=vkm_multi", nil)
	tagsRR := httptest.NewRecorder()
	s.handleVKMSourceTags(tagsRR, tagsReq)
	if tagsRR.Code != http.StatusOK {
		t.Fatalf("source-tags status=%d body=%s", tagsRR.Code, tagsRR.Body.String())
	}
	var sourceRows []struct {
		PipeNo int      `json:"pipe_no"`
		Found  bool     `json:"found"`
		Source string   `json:"source"`
		Tags   []string `json:"tags"`
	}
	if err := json.Unmarshal(tagsRR.Body.Bytes(), &sourceRows); err != nil {
		t.Fatalf("decode source tags: %v", err)
	}
	if len(sourceRows) != 10 || !sourceRows[0].Found || sourceRows[0].Source != "scan" || len(sourceRows[0].Tags) != 2 {
		t.Fatalf("source tags after scan=%+v", sourceRows)
	}
}

func TestHandleVKMPipeDiscoveryGETReturnsPersistedSnapshot(t *testing.T) {
	s, repo := newVKMMultipipeWebTestServer(t)
	if err := repo.ReplaceVKMPipeDiscovery(context.Background(), "vkm_multi", []sqliterepo.VKMPipeDiscoveryRecord{
		{PipeNo: 1, Status: sqliterepo.VKMDiscoveryAvailable},
		{PipeNo: 2, Status: sqliterepo.VKMDiscoveryUncertain, Detail: "no records"},
	}); err != nil {
		t.Fatalf("seed discovery: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/vkm-pipe-discovery?device_id=vkm_multi", nil)
	rr := httptest.NewRecorder()
	s.handleVKMPipeDiscovery(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var rows []sqliterepo.VKMPipeDiscoveryRecord
	if err := json.Unmarshal(rr.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 2 || rows[0].PipeNo != 1 || rows[1].Status != sqliterepo.VKMDiscoveryUncertain {
		t.Fatalf("rows=%+v", rows)
	}
}

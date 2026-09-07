package web

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func newReloadTestServer() *Server {
	return &Server{
		baseCtx:    context.Background(),
		reloadJobs: make(map[string]*reloadJob),
	}
}

func postReload(t *testing.T, s *Server, deviceID string) *httptest.ResponseRecorder {
	t.Helper()
	body := []byte(`{"device_id":"` + deviceID + `","from":"2026-09-07T12:00","to":"2026-09-07T13:00"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/devices/reload-archive", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleForceReload(rec, req)
	return rec
}

func TestForceReloadDoesNotTriggerESSync(t *testing.T) {
	s := newReloadTestServer()
	var syncCalls int32
	done := make(chan struct{})

	s.SetSyncNow(func(deviceID string) error {
		atomic.AddInt32(&syncCalls, 1)
		return nil
	})
	s.SetForceReload(func(ctx context.Context, deviceID string, from, to time.Time, onProgress func(done, total int)) (int, error) {
		onProgress(1, 1)
		close(done)
		return 1, nil
	})

	rec := postReload(t, s, "osmos")
	if rec.Code != http.StatusOK {
		t.Fatalf("переопрос не запустился: HTTP %d, body=%s", rec.Code, rec.Body.String())
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("фоновый переопрос не завершился")
	}
	// Даём фоновой goroutine закончить финальное обновление reloadJob.
	time.Sleep(10 * time.Millisecond)
	if got := atomic.LoadInt32(&syncCalls); got != 0 {
		t.Fatalf("принудительный переопрос не должен запускать синхронизацию с ЭС, вызовов=%d", got)
	}
}

func TestForceReloadDifferentDevicesCanRunInParallel(t *testing.T) {
	s := newReloadTestServer()
	started := make(chan string, 2)
	release := make(chan struct{})

	s.SetForceReload(func(ctx context.Context, deviceID string, from, to time.Time, onProgress func(done, total int)) (int, error) {
		started <- deviceID
		select {
		case <-release:
			return 1, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	})

	for _, id := range []string{"osmos", "boylernaya_par"} {
		rec := postReload(t, s, id)
		if rec.Code != http.StatusOK {
			close(release)
			t.Fatalf("переопрос %s не запустился: HTTP %d, body=%s", id, rec.Code, rec.Body.String())
		}
	}

	seen := make(map[string]bool)
	for len(seen) < 2 {
		select {
		case id := <-started:
			seen[id] = true
		case <-time.After(500 * time.Millisecond):
			close(release)
			t.Fatalf("переопросы разных приборов не стартовали параллельно: %v", seen)
		}
	}
	close(release)
}

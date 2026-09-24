package integration

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	sqliterepo "mbgw/internal/storage/sqlite"
)

func TestRunEnergosphereSyncReloadingWithRepoReloadsAfterTriggerWhileUnconfigured(t *testing.T) {
	repo, err := sqliterepo.New(filepath.Join(t.TempDir(), "dynamic_es.db"))
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()

	calls := make(chan struct{}, 4)
	load := func(context.Context) (Config, bool, error) {
		calls <- struct{}{}
		return Config{}, false, nil
	}

	trigger := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunEnergosphereSyncReloadingWithRepo(ctx, repo, load, trigger)
	}()

	waitCall := func(label string) {
		t.Helper()
		select {
		case <-calls:
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for config reload: %s", label)
		}
	}

	waitCall("initial")
	trigger <- struct{}{}
	waitCall("after trigger")

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("worker returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop after context cancellation")
	}
}

func TestRunEnergosphereSyncReloadingWithRepoRejectsNilLoader(t *testing.T) {
	repo, err := sqliterepo.New(filepath.Join(t.TempDir(), "dynamic_es_nil_loader.db"))
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()

	err = RunEnergosphereSyncReloadingWithRepo(context.Background(), repo, nil, nil)
	if err == nil {
		t.Fatal("expected error for nil config loader")
	}
}

package servicelog

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestAppendListAndCleanup(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx := context.Background()
	if err := l.Append(ctx, "предупреждение", "контроль зависания", "проверка"); err != nil {
		t.Fatal(err)
	}
	rows, err := l.List(ctx, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	if rows[0].Level != "предупреждение" || rows[0].Message != "проверка" {
		t.Fatalf("unexpected row: %#v", rows[0])
	}
	if err := l.Cleanup(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	rows, err = l.List(ctx, 10)
	if err != nil || len(rows) != 0 {
		t.Fatalf("after cleanup rows=%d err=%v", len(rows), err)
	}
}

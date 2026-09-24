package sqlite

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

func TestVKMActivePipesRoundTrip(t *testing.T) {
	r, err := New(filepath.Join(t.TempDir(), "pipes.db"))
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer r.Close()
	ctx := context.Background()
	if err := r.InitDeviceConfigSchema(ctx); err != nil {
		t.Fatalf("init device config schema: %v", err)
	}

	got, err := r.GetVKMActivePipes(ctx, "vkm1")
	if err != nil {
		t.Fatalf("get empty: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("new device explicit pipe rows=%v, want empty (caller defaults to pipe1)", got)
	}

	if err := r.SetVKMActivePipes(ctx, "vkm1", []int{10, 2, 1}); err != nil {
		t.Fatalf("set pipes: %v", err)
	}
	got, err = r.GetVKMActivePipes(ctx, "vkm1")
	if err != nil {
		t.Fatalf("get pipes: %v", err)
	}
	want := []int{1, 2, 10}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pipes=%v, want %v", got, want)
	}
}

func TestVKMActivePipesRejectsInvalidSet(t *testing.T) {
	r, err := New(filepath.Join(t.TempDir(), "pipes_invalid.db"))
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer r.Close()
	ctx := context.Background()
	if err := r.InitDeviceConfigSchema(ctx); err != nil {
		t.Fatalf("init device config schema: %v", err)
	}

	for _, pipes := range [][]int{{}, {0}, {11}, {1, 1}} {
		if err := r.SetVKMActivePipes(ctx, "vkm1", pipes); err == nil {
			t.Fatalf("SetVKMActivePipes(%v) unexpectedly succeeded", pipes)
		}
	}
}

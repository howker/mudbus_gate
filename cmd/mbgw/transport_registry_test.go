package main

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type testTransportCloser struct {
	calls int32
	err   error
	wait  <-chan struct{}
}

func (c *testTransportCloser) Close() error {
	atomic.AddInt32(&c.calls, 1)
	if c.wait != nil {
		<-c.wait
	}
	return c.err
}

func TestTransportRegistryCloseAllAndIdempotent(t *testing.T) {
	r := newTransportRegistry()
	ok1 := &testTransportCloser{}
	bad := &testTransportCloser{err: errors.New("close failed")}
	if !r.Add("d1", ok1) || !r.Add("d2", bad) {
		t.Fatal("Add unexpectedly rejected before shutdown")
	}

	s := r.CloseAll(time.Second)
	if s.Total != 2 || s.Closed != 1 || s.Failed != 1 || s.TimedOut != 0 {
		t.Fatalf("unexpected summary: %+v", s)
	}
	if atomic.LoadInt32(&ok1.calls) != 1 || atomic.LoadInt32(&bad.calls) != 1 {
		t.Fatalf("each closer must be called exactly once: ok=%d bad=%d", ok1.calls, bad.calls)
	}

	s2 := r.CloseAll(time.Second)
	if s2.Total != 0 {
		t.Fatalf("second CloseAll must be a no-op, got %+v", s2)
	}
	if r.Add("late", &testTransportCloser{}) {
		t.Fatal("Add must reject after shutdown begins")
	}
}

func TestTransportRegistryCloseAllIsBounded(t *testing.T) {
	release := make(chan struct{})
	blocked := &testTransportCloser{wait: release}
	fast := &testTransportCloser{}
	r := newTransportRegistry()
	r.Add("blocked", blocked)
	r.Add("fast", fast)

	started := time.Now()
	s := r.CloseAll(40 * time.Millisecond)
	elapsed := time.Since(started)
	if elapsed > 500*time.Millisecond {
		close(release)
		t.Fatalf("CloseAll exceeded bounded wait: %v", elapsed)
	}
	if s.Total != 2 || s.Closed != 1 || s.TimedOut != 1 {
		close(release)
		t.Fatalf("unexpected timeout summary: %+v", s)
	}
	if atomic.LoadInt32(&fast.calls) != 1 {
		close(release)
		t.Fatal("fast transport was not closed while another transport blocked")
	}
	close(release)
}

package main

import (
	"bytes"
	"sync"
	"testing"
	"time"
)

type blockingWriter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return len(p), nil
}

func TestNonBlockingWriterDoesNotBlockCaller(t *testing.T) {
	dst := &blockingWriter{started: make(chan struct{}), release: make(chan struct{})}
	w := newNonBlockingWriter(dst, 1)

	done := make(chan struct{})
	go func() {
		_, _ = w.Write([]byte("первая строка\n"))
		_, _ = w.Write([]byte("вторая строка\n"))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("запись в неблокирующий writer зависла вместе с получателем")
	}

	select {
	case <-dst.started:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("фоновый writer не начал запись в получатель")
	}
	close(dst.release)
}

func TestNonBlockingWriterCopiesInput(t *testing.T) {
	var dst bytes.Buffer
	w := newNonBlockingWriter(&dst, 4)
	p := []byte("исходная строка\n")
	_, _ = w.Write(p)
	copy(p, []byte("ИСПОРЧЕНО"))

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if dst.String() == "исходная строка\n" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("получатель получил не копию исходных байтов: %q", dst.String())
}

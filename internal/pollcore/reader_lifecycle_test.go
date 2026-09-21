package pollcore

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"mbgw/internal/transport"
)

type lifecycleTestTransport struct {
	params transport.Params

	mu       sync.Mutex
	open     bool
	closed   bool
	opens    int
	releases int

	onOpen    func()
	onRelease func()
}

func (t *lifecycleTestTransport) Open(context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return io.ErrClosedPipe
	}
	if !t.open {
		t.open = true
		t.opens++
		if t.onOpen != nil {
			t.onOpen()
		}
	}
	return nil
}

func (t *lifecycleTestTransport) Release() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.open {
		t.open = false
		t.releases++
		if t.onRelease != nil {
			t.onRelease()
		}
	}
	return nil
}

func (t *lifecycleTestTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	t.open = false
	return nil
}

func (t *lifecycleTestTransport) Send(context.Context, []byte) error { return nil }

func (t *lifecycleTestTransport) Receive(context.Context, time.Duration) ([]byte, error) {
	return nil, nil
}

func (t *lifecycleTestTransport) Info() transport.Params { return t.params }

func TestReaderWithPhysicalChannelReleasesSerialAfterOperation(t *testing.T) {
	tr := &lifecycleTestTransport{
		params: transport.Params{Kind: transport.KindRTUSerial, COM: "COM16"},
	}
	r := NewWithLockKey(tr, false, 1, PhysicalIOLockKey(tr.params, "dev-1"))

	_, err := r.withPhysicalChannel(context.Background(), func() ([]byte, error) {
		tr.mu.Lock()
		defer tr.mu.Unlock()
		if !tr.open {
			t.Fatal("transport must be open inside physical operation")
		}
		return []byte{1}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.open {
		t.Fatal("serial transport remained open after physical operation")
	}
	if tr.opens != 1 || tr.releases != 1 {
		t.Fatalf("want opens=1 releases=1, got opens=%d releases=%d", tr.opens, tr.releases)
	}
}

func TestReadersOnSameCOMSerializeOpenThroughRelease(t *testing.T) {
	var stateMu sync.Mutex
	active := 0
	overlap := false

	makeTransport := func() *lifecycleTestTransport {
		tr := &lifecycleTestTransport{
			params: transport.Params{Kind: transport.KindRTUSerial, COM: "COM16"},
		}
		tr.onOpen = func() {
			stateMu.Lock()
			defer stateMu.Unlock()
			active++
			if active > 1 {
				overlap = true
			}
		}
		tr.onRelease = func() {
			stateMu.Lock()
			active--
			stateMu.Unlock()
		}
		return tr
	}

	tr1 := makeTransport()
	tr2 := makeTransport()
	key1 := PhysicalIOLockKey(tr1.params, "dev-1")
	key2 := PhysicalIOLockKey(tr2.params, "dev-2")
	if key1 != key2 {
		t.Fatalf("same COM must share lock key: %q != %q", key1, key2)
	}

	r1 := NewWithLockKey(tr1, false, 1, key1)
	r2 := NewWithLockKey(tr2, false, 2, key2)

	start := make(chan struct{})
	var wg sync.WaitGroup
	run := func(r *Reader) {
		defer wg.Done()
		<-start
		_, err := r.withPhysicalChannel(context.Background(), func() ([]byte, error) {
			time.Sleep(25 * time.Millisecond)
			return nil, nil
		})
		if err != nil {
			t.Errorf("physical operation: %v", err)
		}
	}

	wg.Add(2)
	go run(r1)
	go run(r2)
	close(start)
	wg.Wait()

	stateMu.Lock()
	defer stateMu.Unlock()
	if overlap {
		t.Fatal("two devices on the same COM had overlapping open physical channels")
	}
	if active != 0 {
		t.Fatalf("physical channel leak: active=%d", active)
	}
}

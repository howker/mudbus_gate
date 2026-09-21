package transport

import (
	"io"
	"testing"
)

type releaseTestPort struct {
	closed bool
}

func (p *releaseTestPort) Read([]byte) (int, error)    { return 0, io.EOF }
func (p *releaseTestPort) Write(b []byte) (int, error) { return len(b), nil }

func (p *releaseTestPort) Close() error {
	p.closed = true
	return nil
}

func TestSerialReleaseFreesHandleWithoutTerminalClose(t *testing.T) {
	p := &releaseTestPort{}
	tr := &serialTransport{
		params: Params{Kind: KindRTUSerial, COM: "COM16"},
		port:   p,
	}

	if err := tr.Release(); err != nil {
		t.Fatal(err)
	}
	if !p.closed {
		t.Fatal("Release did not close the physical handle")
	}
	if tr.port != nil {
		t.Fatal("Release left physical handle attached")
	}
	if tr.closed {
		t.Fatal("Release must keep transport reusable")
	}

	// Final Close remains terminal even when the physical handle is idle.
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	if !tr.closed {
		t.Fatal("Close must mark transport terminally closed")
	}
}

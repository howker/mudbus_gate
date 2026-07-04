package transport

import (
    "context"
    "net"
    "testing"
    "time"
)

func TestTCPTransport_LoopbackSendReceive(t *testing.T) {
    ln, err := net.Listen("tcp", "127.0.0.1:0")
    if err != nil {
        t.Fatalf("listen error: %v", err)
    }
    defer ln.Close()

    wantReq := []byte{0x01, 0x03, 0x00, 0x10, 0x00, 0x02}
    wantResp := []byte{
        0x00, 0x01, 0x00, 0x00, 0x00, 0x07, 0x01,
        0x03, 0x04, 0x12, 0x34, 0x56, 0x78,
    }

    serverDone := make(chan struct{})
    go func() {
        defer close(serverDone)
        conn, err := ln.Accept()
        if err != nil {
            return
        }
        defer conn.Close()

        buf := make([]byte, len(wantReq))
        if _, err := conn.Read(buf); err != nil {
            return
        }
        if string(buf) != string(wantReq) {
            return
        }
        _, _ = conn.Write(wantResp)
    }()

    tp, err := New(Params{
        Kind:            KindModbusTCP,
        Host:            "127.0.0.1",
        Port:            ln.Addr().(*net.TCPAddr).Port,
        ResponseTimeout: 2 * time.Second,
        InterframeDelay: 0,
    })
    if err != nil {
        t.Fatalf("new transport error: %v", err)
    }
    defer tp.Close()

    ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
    defer cancel()

    if err := tp.Open(ctx); err != nil {
        t.Fatalf("open error: %v", err)
    }

    if err := tp.Send(ctx, wantReq); err != nil {
        t.Fatalf("send error: %v", err)
    }

    got, err := tp.Receive(ctx, 2*time.Second)
    if err != nil {
        t.Fatalf("receive error: %v", err)
    }
    if string(got) != string(wantResp) {
        t.Fatalf("unexpected response: want %x, got %x", wantResp, got)
    }

    <-serverDone
}

func TestTCPTransport_OpenIsIdempotent(t *testing.T) {
    ln, err := net.Listen("tcp", "127.0.0.1:0")
    if err != nil {
        t.Fatalf("listen error: %v", err)
    }
    defer ln.Close()

    go func() {
        conn, err := ln.Accept()
        if err != nil {
            return
        }
        defer conn.Close()
        time.Sleep(200 * time.Millisecond)
    }()

    tp, err := New(Params{
        Kind:            KindModbusTCP,
        Host:            "127.0.0.1",
        Port:            ln.Addr().(*net.TCPAddr).Port,
        ResponseTimeout: 2 * time.Second,
    })
    if err != nil {
        t.Fatalf("new transport error: %v", err)
    }
    defer tp.Close()

    ctx := context.Background()
    if err := tp.Open(ctx); err != nil {
        t.Fatalf("first open error: %v", err)
    }
    if err := tp.Open(ctx); err != nil {
        t.Fatalf("second open (should be no-op) error: %v", err)
    }
}

func TestTCPTransport_Close(t *testing.T) {
    ln, err := net.Listen("tcp", "127.0.0.1:0")
    if err != nil {
        t.Fatalf("listen error: %v", err)
    }
    defer ln.Close()

    go func() {
        conn, err := ln.Accept()
        if err != nil {
            return
        }
        defer conn.Close()
        time.Sleep(100 * time.Millisecond)
    }()

    tp, err := New(Params{
        Kind:            KindModbusTCP,
        Host:            "127.0.0.1",
        Port:            ln.Addr().(*net.TCPAddr).Port,
        ResponseTimeout: 2 * time.Second,
    })
    if err != nil {
        t.Fatalf("new transport error: %v", err)
    }

    if err := tp.Open(context.Background()); err != nil {
        t.Fatalf("open error: %v", err)
    }
    if err := tp.Close(); err != nil {
        t.Fatalf("close error: %v", err)
    }
}

func TestTCPTransport_CallsAfterCloseFail(t *testing.T) {
    ln, err := net.Listen("tcp", "127.0.0.1:0")
    if err != nil {
        t.Fatalf("listen error: %v", err)
    }
    defer ln.Close()

    go func() {
        conn, err := ln.Accept()
        if err != nil {
            return
        }
        conn.Close()
    }()

    tp, err := New(Params{
        Kind:            KindModbusTCP,
        Host:            "127.0.0.1",
        Port:            ln.Addr().(*net.TCPAddr).Port,
        ResponseTimeout: 2 * time.Second,
    })
    if err != nil {
        t.Fatalf("new transport error: %v", err)
    }

    ctx := context.Background()
    if err := tp.Open(ctx); err != nil {
        t.Fatalf("open error: %v", err)
    }
    if err := tp.Close(); err != nil {
        t.Fatalf("close error: %v", err)
    }

    if err := tp.Send(ctx, []byte{0x01}); err == nil {
        t.Fatal("expected error sending after close, got nil")
    }
}
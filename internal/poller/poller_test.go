package poller

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mbgw/internal/device"
	"mbgw/internal/lease"
	"mbgw/internal/profile"
	"mbgw/internal/scheduler"
	"mbgw/internal/session"
	sqliterepo "mbgw/internal/storage/sqlite"
)

// countingClient is a device.PointClient that counts ReadRaw/Transact
// calls, so tests can verify exactly how many times a device was
// actually polled (the "no double poll" DoD requirement).
type countingClient struct {
	mu            sync.Mutex
	readCount     int32
	transactCount int32
}

func (c *countingClient) ReadRaw(ctx context.Context, space string, addr int, dataType string) ([]byte, error) {
	atomic.AddInt32(&c.readCount, 1)
	return []byte{0x42, 0xC8, 0x00, 0x00}, nil
}

func (c *countingClient) Transact(ctx context.Context, req []byte) ([]byte, error) {
	atomic.AddInt32(&c.transactCount, 1)
	return nil, nil
}

func (c *countingClient) Reads() int {
	return int(atomic.LoadInt32(&c.readCount))
}

func newTestDevice(t *testing.T, id string) (*device.Device, *countingClient) {
	t.Helper()
	p := &profile.Profile{
		Codec: profile.Codec{WordOrder32: "0123"},
		Points: []profile.Point{
			{Name: "V", Type: "float", Access: "read"},
		},
	}
	cli := &countingClient{}
	sess := &session.NoopSession{}
	repo, err := sqliterepo.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create repo: %v", err)
	}
	if err := repo.InitSchema(context.Background()); err != nil {
		t.Fatalf("failed to init schema: %v", err)
	}
	leaseMgr := lease.New()
	dev := device.New(id, p, cli, sess, repo, leaseMgr)
	return dev, cli
}

func TestPoller_DispatchesCurrentTask(t *testing.T) {
	dev, cli := newTestDevice(t, "dev1")
	sched := scheduler.New(nil)
	sched.RequestManualPoll("dev1", scheduler.KindCurrent)

	p := New(sched, map[string]*device.Device{"dev1": dev}, time.Second)
	p.drain(context.Background())

	if cli.Reads() != 1 {
		t.Fatalf("expected exactly 1 read from a single current-value task, got %d", cli.Reads())
	}
}

func TestPoller_UnknownDevice_DoesNotPanic(t *testing.T) {
	sched := scheduler.New(nil)
	sched.RequestManualPoll("ghost", scheduler.KindCurrent)

	p := New(sched, map[string]*device.Device{}, time.Second)
	p.drain(context.Background())
}

func TestPoller_NoDoublePoll_SingleDispatchPerTask(t *testing.T) {
	dev, cli := newTestDevice(t, "dev1")
	sched := scheduler.New(nil)
	for i := 0; i < 5; i++ {
		sched.RequestManualPoll("dev1", scheduler.KindCurrent)
	}

	p := New(sched, map[string]*device.Device{"dev1": dev}, time.Second)
	p.drain(context.Background())

	if cli.Reads() != 5 {
		t.Fatalf("expected exactly 5 reads for 5 queued tasks, got %d", cli.Reads())
	}
}

func TestPoller_EndToEnd_FourMockDevices(t *testing.T) {
	devices := make(map[string]*device.Device)
	clients := make(map[string]*countingClient)
	sched := scheduler.New(nil)

	for _, id := range []string{"dev1", "dev2", "dev3", "dev4"} {
		dev, cli := newTestDevice(t, id)
		devices[id] = dev
		clients[id] = cli
		sched.Register(id, 20*time.Millisecond, 0, nil)
	}

	p := New(sched, devices, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	go p.Run(ctx)

	time.Sleep(120 * time.Millisecond)
	cancel()
	time.Sleep(20 * time.Millisecond)

	for id, cli := range clients {
		if cli.Reads() == 0 {
			t.Fatalf("device %q was never polled", id)
		}
	}
}

func TestPoller_ArchiveTask_Dispatched(t *testing.T) {
	p := &profile.Profile{
		Codec: profile.Codec{WordOrder32: "0123"},
		Points: []profile.Point{
			{Name: "V", Type: "float", Access: "read"},
		},
		Archives: []profile.Archive{
			{
				ID:       "hourly",
				Strategy: "mb_func65",
				Params:   map[string]any{"archive_type": 0},
			},
		},
	}
	cli := &countingClient{}
	sess := &session.NoopSession{}
	repo, err := sqliterepo.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create repo: %v", err)
	}
	if err := repo.InitSchema(context.Background()); err != nil {
		t.Fatalf("failed to init schema: %v", err)
	}
	leaseMgr := lease.New()
	dev := device.New("dev1", p, cli, sess, repo, leaseMgr)

	sched := scheduler.New(nil)
	sched.RequestManualPoll("dev1", scheduler.KindArchive)

	poller := New(sched, map[string]*device.Device{"dev1": dev}, time.Second)
	poller.drain(context.Background())

	if atomic.LoadInt32(&cli.transactCount) == 0 {
		t.Fatal("expected an archive task to trigger at least one Transact call")
	}
	if cli.Reads() != 0 {
		t.Fatalf("expected an archive task to NOT call ReadRaw (that's Poll's path, not PollArchives'), got %d reads", cli.Reads())
	}
}

func TestRunDeviceQueue_CancelDropsPendingTasks(t *testing.T) {
	dev, cli := newTestDevice(t, "dev1")
	sched := scheduler.New(nil)
	p := New(sched, map[string]*device.Device{"dev1": dev}, time.Second)

	q := &deviceTaskQueue{
		tasks: []scheduler.Task{
			{DeviceID: "dev1", Kind: scheduler.KindCurrent},
			{DeviceID: "dev1", Kind: scheduler.KindCurrent},
		},
		running: true,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p.runDeviceQueue(ctx, q)

	if cli.Reads() != 0 {
		t.Fatalf("expected cancelled queue to dispatch no pending tasks, got %d reads", cli.Reads())
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	if q.running {
		t.Fatal("expected cancelled queue worker to mark itself stopped")
	}
	if len(q.tasks) != 0 {
		t.Fatalf("expected cancelled queue to drop pending tasks, got %d", len(q.tasks))
	}
}

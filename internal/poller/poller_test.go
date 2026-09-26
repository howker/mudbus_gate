package poller

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mbgw/internal/archive"
	"mbgw/internal/device"
	"mbgw/internal/health"
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
	t.Cleanup(func() { _ = repo.Close() })
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
				ID:               "hourly",
				Strategy:         "mb_func65",
				Params:           map[string]any{"archive_type": 0},
				BufferDepthHours: 1,
				RecordLayout: []profile.RecordField{
					{Offset: 0, Name: "archive_time", Type: "uint32", Epoch: "1970-01-01"},
					{Offset: 4, Name: "v_plus", Type: "float", Unit: "m3"},
				},
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

type parallelClient struct {
	started chan<- string
	release <-chan struct{}
	id      string
}

func (c *parallelClient) ReadRaw(ctx context.Context, space string, addr int, dataType string) ([]byte, error) {
	select {
	case c.started <- c.id:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-c.release:
		return []byte{0x42, 0xC8, 0x00, 0x00}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *parallelClient) Transact(ctx context.Context, req []byte) ([]byte, error) {
	return nil, nil
}

func newParallelTestDevice(t *testing.T, id string, cli device.PointClient) *device.Device {
	t.Helper()
	p := &profile.Profile{
		Codec:  profile.Codec{WordOrder32: "0123"},
		Points: []profile.Point{{Name: "V", Type: "float", Access: "read"}},
	}
	repo, err := sqliterepo.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create repo: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.InitSchema(context.Background()); err != nil {
		t.Fatalf("failed to init schema: %v", err)
	}
	return device.New(id, p, cli, &session.NoopSession{}, repo, lease.New())
}

func TestPoller_DifferentDevicesRunInParallel(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})

	dev1 := newParallelTestDevice(t, "dev1", &parallelClient{id: "dev1", started: started, release: release})
	dev2 := newParallelTestDevice(t, "dev2", &parallelClient{id: "dev2", started: started, release: release})

	sched := scheduler.New(nil)
	sched.RequestManualPoll("dev1", scheduler.KindCurrent)
	sched.RequestManualPoll("dev2", scheduler.KindCurrent)
	p := New(sched, map[string]*device.Device{"dev1": dev1, "dev2": dev2}, time.Second)

	done := make(chan struct{})
	go func() {
		p.drain(context.Background())
		close(done)
	}()

	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case id := <-started:
			seen[id] = true
		case <-time.After(500 * time.Millisecond):
			close(release)
			t.Fatalf("разные приборы не начали опрос параллельно; успели стартовать: %v", seen)
		}
	}
	close(release)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("параллельный опрос не завершился")
	}
}

func TestPoller_BackfillChecksDeviceClock(t *testing.T) {
	addr := 32768
	p := &profile.Profile{
		Meta:  profile.Meta{Model: "IVK-TER"},
		Codec: profile.Codec{WordOrder32: "0123"},
		Points: []profile.Point{{
			Name: "current_time", Space: "IR", Addr: &addr, Type: "uint32", Epoch: "1970-01-01", Access: "read",
		}},
	}
	cli := &countingClient{}
	repo, err := sqliterepo.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create repo: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.InitSchema(context.Background()); err != nil {
		t.Fatalf("failed to init schema: %v", err)
	}
	dev := device.New("ivk1", p, cli, &session.NoopSession{}, repo, lease.New())

	sched := scheduler.New(nil)
	sched.RequestManualPoll("ivk1", scheduler.KindBackfill)
	New(sched, map[string]*device.Device{"ivk1": dev}, time.Second).drain(context.Background())

	if cli.Reads() != 1 {
		t.Fatalf("startup backfill clock reads=%d, want 1", cli.Reads())
	}
}

type pollerNoRecordVKMReader struct{ calls *int32 }

func (r *pollerNoRecordVKMReader) Strategy() string { return "mb_request_poll_string" }
func (r *pollerNoRecordVKMReader) Read(_ context.Context, _ archive.ArchiveSession, _ archive.Transactor, _ archive.ArchiveQuery) ([]archive.ArchiveRecord, error) {
	atomic.AddInt32(r.calls, 1)
	return nil, archive.ErrVKMNoRecords
}

func TestPollerArchiveStatusSuccessWhenLatestVKMPeriodNotClosed(t *testing.T) {
	old, ok := archive.Get("mb_request_poll_string")
	var calls int32
	archive.Register(&pollerNoRecordVKMReader{calls: &calls})
	defer func() {
		if ok {
			archive.Register(old)
		}
	}()

	repo, err := sqliterepo.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.InitSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := repo.InitDeviceConfigSchema(context.Background()); err != nil {
		t.Fatal(err)
	}

	prof := &profile.Profile{Archives: []profile.Archive{{ID: "main", Strategy: "mb_request_poll_string"}}}
	dev := device.New("vkm_status_latest_not_closed", prof, &countingClient{}, &session.NoopSession{}, repo, lease.New())

	// Startup backfill already filled every historical period; the newest
	// boundary is explicitly marked no-record from an earlier attempt.
	now := time.Now()
	latestStart := now.Truncate(30 * time.Minute).Add(-30 * time.Minute)
	latestLabel := latestStart.Add(30 * time.Minute)
	for i := 1; i < 48; i++ {
		if err := repo.SaveVKMRawString(context.Background(), dev.ID, 1, latestLabel.Add(-time.Duration(i)*30*time.Minute), "T=1°C;"); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.MarkVKMNoRecords(context.Background(), dev.ID, 1, latestLabel, time.Now().Add(6*time.Hour)); err != nil {
		t.Fatal(err)
	}

	sched := scheduler.New(nil)
	sched.RequestManualPoll(dev.ID, scheduler.KindArchive)
	p := New(sched, map[string]*device.Device{dev.ID: dev}, time.Second)
	p.drain(context.Background())

	st := health.Get().Devices[dev.ID]
	if !st.LastPollKnown || !st.LastPollOK {
		t.Fatalf("latest VKM no-record response must produce successful live poll status: %+v", st)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("physical reads=%d, want exactly one mandatory latest read", got)
	}
}

package poller

import (
	"context"
	"log"
	"sync"
	"time"

	"mbgw/internal/device"
	"mbgw/internal/health"
	"mbgw/internal/scheduler"
)

// Package poller централизованно раздаёт задания планировщика
// (scheduler.Task) нужным приборам (device.Device), заменяя прежние
// горутины-тикеры на прибор (device.Start) единой общей очередью — это
// то, что реально позволяет ручному опросу, приоритету и отложенному
// повтору (всё это уже встроено в internal/scheduler) доходить до
// настоящих приборов.
//
// ИЗМЕНЕНО (2026-08-31, прямой запрос оператора: "нужен одновременный
// опрос чтобы не было цепочки ожидания"): раньше диспетчеризация была
// НАМЕРЕННО однопоточной (одно задание за раз, строго по очереди) —
// самый простой вариант, который заведомо не может опросить один и тот
// же прибор дважды одновременно, раз транспорт/сессия прибора не
// потокобезопасны сами по себе (CONTRACTS.md, раздел 1). Но при большом
// числе приборов (в проде их будет до полусотни) это означало реальную
// цепочку блокировки: если у одного прибора медленный/подвисший ответ,
// ВСЕ остальные приборы этого тика ждут его освобождения, прежде чем
// их опрос вообще начнётся.
//
// Теперь разные приборы опрашиваются параллельно, но задания ОДНОГО
// прибора сохраняют порядок, в котором их выдал scheduler. Для этого у
// каждого прибора есть своя маленькая FIFO-очередь и не более одной
// worker-горутины. Это важно: обычный mutex гарантирует только
// взаимное исключение, но НЕ гарантирует порядок захвата; если просто
// запустить по горутине на каждую задачу, backfill/current/archive одного
// прибора могут фактически выполниться не в том порядке, в котором были
// извлечены из scheduler.
//
// deviceMu остаётся последней защитой физического Device на случай
// синхронного drain() в тестах/служебных вызовах. В production основной
// порядок обеспечивает per-device FIFO ниже.
type deviceTaskQueue struct {
	mu      sync.Mutex
	tasks   []scheduler.Task
	running bool
}

type Poller struct {
	scheduler *scheduler.Scheduler
	devices   map[string]*device.Device
	tickEvery time.Duration

	// deviceMu — мьютекс НА КАЖДЫЙ ПРИБОР, а не один общий на все.
	deviceMu sync.Map

	// deviceQueues — FIFO заданий по каждому прибору. Пока worker одного
	// прибора занят долгим backfill, новые задания этого же прибора
	// добавляются сюда, а не превращаются в отдельные горутины, висящие на
	// mutex. Для разных приборов worker'ы независимы и работают параллельно.
	deviceQueues sync.Map
}

// New creates a Poller. devices maps device ID -> already-constructed
// Device (transport/session/profile already wired, as cmd/mbgw/run.go
// builds them). tickEvery controls how often the scheduler's due-times
// are checked; a zero/negative value defaults to 1 second.
func New(sched *scheduler.Scheduler, devices map[string]*device.Device, tickEvery time.Duration) *Poller {
	if tickEvery <= 0 {
		tickEvery = 1 * time.Second
	}
	return &Poller{scheduler: sched, devices: devices, tickEvery: tickEvery}
}

// Run drives the scheduler (Tick, then drain every due task) until ctx is
// cancelled. Blocks the calling goroutine - callers typically invoke this
// via `go poller.Run(ctx)`.
func (p *Poller) Run(ctx context.Context) {
	ticker := time.NewTicker(p.tickEvery)
	defer ticker.Stop()

	log.Printf("[poller] запуск (%d приборов, тик %v)\n", len(p.devices), p.tickEvery)

	for {
		select {
		case <-ctx.Done():
			log.Println("[poller] остановка")
			return
		case <-ticker.C:
			now := time.Now()
			p.scheduler.Tick(now)
			p.drainAsync(ctx)
			health.MarkPollerCycle(now)
		}
	}
}

// drainAsync забирает все готовые задания из scheduler и раскладывает их
// по FIFO конкретных приборов. Функция сама не ждёт выполнения: долгий
// startup-backfill одного прибора не замораживает следующие Tick и не
// мешает другим приборам.
//
// Важное отличие от схемы "go dispatchLocked(...) на каждую задачу":
// здесь порядок задач одного прибора сохраняется, и на занятом приборе
// не накапливаются горутины, ожидающие mutex.
func (p *Poller) drainAsync(ctx context.Context) {
	for {
		task, ok := p.scheduler.Next()
		if !ok {
			return
		}
		p.enqueueAsync(ctx, task)
	}
}

func (p *Poller) taskQueue(deviceID string) *deviceTaskQueue {
	v, _ := p.deviceQueues.LoadOrStore(deviceID, &deviceTaskQueue{})
	return v.(*deviceTaskQueue)
}

func (p *Poller) enqueueAsync(ctx context.Context, task scheduler.Task) {
	q := p.taskQueue(task.DeviceID)

	q.mu.Lock()
	q.tasks = append(q.tasks, task)
	if q.running {
		q.mu.Unlock()
		return
	}
	q.running = true
	q.mu.Unlock()

	go p.runDeviceQueue(ctx, q)
}

func (p *Poller) runDeviceQueue(ctx context.Context, q *deviceTaskQueue) {
	for {
		q.mu.Lock()
		if len(q.tasks) == 0 {
			q.running = false
			q.mu.Unlock()
			return
		}
		task := q.tasks[0]
		q.tasks = q.tasks[1:]
		q.mu.Unlock()

		p.dispatchLocked(ctx, task)
	}
}

// deviceLock возвращает мьютекс, закреплённый именно за этим прибором —
// создаётся при первом обращении, дальше переиспользуется. Разные
// приборы получают разные мьютексы и потому никогда не блокируют друг
// друга; повторные задания ОДНОГО прибора корректно встают в очередь
// друг за другом через один и тот же мьютекс.
func (p *Poller) deviceLock(deviceID string) *sync.Mutex {
	v, _ := p.deviceMu.LoadOrStore(deviceID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// drain — синхронный вариант для тестов/служебных вызовов. Он сохраняет
// ту же семантику, что production: разные приборы идут параллельно, а
// задачи одного прибора выполняются строго в порядке scheduler.Next().
func (p *Poller) drain(ctx context.Context) {
	batches := make(map[string][]scheduler.Task)
	var deviceOrder []string

	for {
		task, ok := p.scheduler.Next()
		if !ok {
			break
		}
		if _, exists := batches[task.DeviceID]; !exists {
			deviceOrder = append(deviceOrder, task.DeviceID)
		}
		batches[task.DeviceID] = append(batches[task.DeviceID], task)
	}

	var wg sync.WaitGroup
	for _, deviceID := range deviceOrder {
		tasks := batches[deviceID]
		wg.Add(1)
		go func(batch []scheduler.Task) {
			defer wg.Done()
			for _, task := range batch {
				p.dispatchLocked(ctx, task)
			}
		}(tasks)
	}
	wg.Wait()
}

// dispatchLocked берёт мьютекс ИМЕННО ЭТОГО прибора перед тем, как его
// опрашивать — если для этого же прибора уже выполняется другое
// задание, эта горутина подождёт здесь, не занимая при этом ничего
// общего с остальными приборами.
func (p *Poller) dispatchLocked(ctx context.Context, task scheduler.Task) {
	mu := p.deviceLock(task.DeviceID)
	mu.Lock()
	defer mu.Unlock()
	p.dispatch(ctx, task)
}

// dispatch routes one Task to its device's Poll or PollArchives.
func (p *Poller) dispatch(ctx context.Context, task scheduler.Task) {
	dev, ok := p.devices[task.DeviceID]
	if !ok {
		log.Printf("[poller] задача для неизвестного прибора %q, пропускаю\n", task.DeviceID)
		return
	}

	switch task.Kind {
	case scheduler.KindCurrent:
		dev.Poll(ctx)
	case scheduler.KindArchive:
		dev.PollArchives(ctx)
	case scheduler.KindBackfill:
		// Deep catch-up (device.BackfillArchives), same call the startup
		// sweep makes — but dispatched here, in-queue, so it can never
		// run concurrently with a current-value or regular-archive poll
		// for the same device's transport. See scheduler.KindBackfill's
		// doc comment for the incident this prevents. Это гарантия
		// сохраняется и с параллельным диспетчером — dispatchLocked уже
		// не даёт ДВУМ заданиям ОДНОГО прибора выполняться одновременно,
		// какого бы вида они ни были.
		dev.BackfillArchives(ctx, device.BackfillOptions{})
	default:
		log.Printf("[poller] неизвестный тип задачи %q для %q\n", task.Kind, task.DeviceID)
	}
}

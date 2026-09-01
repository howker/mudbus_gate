package poller

import (
	"context"
	"log"
	"sync"
	"time"

	"mbgw/internal/device"
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
// Теперь каждое задание запускается в СВОЕЙ горутине — приборы
// опрашиваются параллельно, а не по очереди. Защита от двойного
// опроса ОДНОГО И ТОГО ЖЕ прибора сохранена — теперь через отдельный
// мьютекс НА КАЖДЫЙ ПРИБОР (deviceMu, лениво создаётся при первом
// обращении к прибору): если для прибора A уже выполняется одно
// задание, а планировщик выдал для него ЕЩЁ одно (например, прибор
// оказался медленнее межтикового интервала) — новая горутина просто
// подождёт на мьютексе ЭТОГО прибора, не мешая при этом ни одному
// ДРУГОМУ прибору обрабатываться параллельно.
type Poller struct {
	scheduler *scheduler.Scheduler
	devices   map[string]*device.Device
	tickEvery time.Duration

	// deviceMu — мьютекс НА КАЖДЫЙ ПРИБОР (ключ — device ID), а не один
	// общий мьютекс на всех: общий мьютекс свёл бы параллельный опрос
	// обратно к последовательному, ровно то, что мы и чиним. sync.Map
	// подходит здесь лучше обычной map+mutex, потому что сама структура
	// заполняется лениво, конкурентно, и никогда не удаляет ключи —
	// именно тот шаблон использования, под который sync.Map
	// специально проектировался.
	deviceMu sync.Map
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
			p.scheduler.Tick(time.Now())
			p.drain(ctx)
		}
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

// drain разбирает ВСЕ задания, стоящие в очереди прямо сейчас. Каждое
// задание запускается в СВОЕЙ горутине (реальная параллельность между
// разными приборами), но сам drain() дожидается завершения ВСЕГО
// пакета целиком через WaitGroup, прежде чем вернуться — то есть один
// вызов drain() по-прежнему означает "весь текущий тик обработан", как
// и раньше, просто внутри этого тика приборы теперь не ждут друг
// друга по очереди, а идут параллельно. Такое сохранение прежнего
// контракта важно и для тестов (которые проверяют счётчики сразу
// после drain()), и для простоты рассуждения о поведении планировщика.
func (p *Poller) drain(ctx context.Context) {
	var wg sync.WaitGroup
	for {
		task, ok := p.scheduler.Next()
		if !ok {
			break
		}
		wg.Add(1)
		go func(t scheduler.Task) {
			defer wg.Done()
			p.dispatchLocked(ctx, t)
		}(task)
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

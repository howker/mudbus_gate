package main

import (
	"fmt"
	"log"
	"sort"
	"sync"
	"time"
)

// transportCloser is the part of transport.Transport needed by the server
// lifecycle. Keeping this tiny interface here makes the shutdown registry
// easy to test without opening real COM/TCP connections.
type transportCloser interface {
	Close() error
}

type transportCloseResult struct {
	deviceID string
	err      error
}

type transportCloseSummary struct {
	Total    int
	Closed   int
	Failed   int
	TimedOut int
	Errors   []string
}

// transportRegistry owns every transport successfully opened by production
// `mbgw server`. Device registration runs in parallel, so the registry is
// synchronized independently from the devices map.
type transportRegistry struct {
	mu      sync.Mutex
	items   map[string]transportCloser
	closing bool
}

func newTransportRegistry() *transportRegistry {
	return &transportRegistry{items: make(map[string]transportCloser)}
}

// Add transfers lifecycle ownership of c to the registry. It returns false
// only after shutdown has begun; callers must then close c themselves and
// abandon registration.
func (r *transportRegistry) Add(deviceID string, c transportCloser) bool {
	if r == nil || c == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing {
		return false
	}
	r.items[deviceID] = c
	return true
}

// CloseAll attempts all closes concurrently and waits no longer than timeout.
// This matters for the watchdog path: a transport Close may itself wait on an
// I/O mutex held by a stuck request, so one bad channel must not prevent the
// process from reaching SCM Recovery. The first call takes ownership of all
// registered transports; later calls are intentionally no-ops.
func (r *transportRegistry) CloseAll(timeout time.Duration) transportCloseSummary {
	if r == nil {
		return transportCloseSummary{}
	}

	r.mu.Lock()
	if r.closing {
		r.mu.Unlock()
		return transportCloseSummary{}
	}
	r.closing = true
	items := r.items
	r.items = make(map[string]transportCloser)
	r.mu.Unlock()

	summary := transportCloseSummary{Total: len(items)}
	if len(items) == 0 {
		return summary
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	results := make(chan transportCloseResult, len(items))
	for deviceID, c := range items {
		deviceID, c := deviceID, c
		go func() {
			results <- transportCloseResult{deviceID: deviceID, err: c.Close()}
		}()
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	remaining := len(items)
	for remaining > 0 {
		select {
		case res := <-results:
			remaining--
			if res.err != nil {
				summary.Failed++
				summary.Errors = append(summary.Errors, fmt.Sprintf("%s: %v", res.deviceID, res.err))
			} else {
				summary.Closed++
			}
		case <-timer.C:
			summary.TimedOut = remaining
			sort.Strings(summary.Errors)
			return summary
		}
	}
	sort.Strings(summary.Errors)
	return summary
}

func logTransportCloseSummary(reason string, summary transportCloseSummary) {
	if summary.Total == 0 {
		return
	}
	if summary.Failed == 0 && summary.TimedOut == 0 {
		log.Printf("[ОК] транспорты приборов закрыты (%s): %d/%d\n", reason, summary.Closed, summary.Total)
		return
	}
	log.Printf("[ПРЕДУПРЕЖДЕНИЕ] закрытие транспортов (%s): закрыто %d/%d, ошибок %d, не дождались %d\n",
		reason, summary.Closed, summary.Total, summary.Failed, summary.TimedOut)
	for _, msg := range summary.Errors {
		log.Printf("[ПРЕДУПРЕЖДЕНИЕ] закрытие транспорта: %s\n", msg)
	}
}

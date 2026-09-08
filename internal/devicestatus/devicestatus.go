// Package devicestatus хранит небольшие runtime-факты о приборах, которые
// нужны одновременно device и web, но не являются конфигурацией/архивом.
package devicestatus

import (
	"sync"
	"time"
)

type TimeDrift struct {
	CheckedAt    time.Time
	DriftSeconds float64
	Reliable     bool
	Note         string
}

// CorrectionStatus описывает только безопасно подтверждённую возможность
// коррекции часов и последнюю УСПЕШНО отправленную коррекцию.
// Mode: vkm_enabled | vkm_disabled | manual_service | not_implemented.
type CorrectionStatus struct {
	Mode               string
	LastAppliedAt      time.Time
	LastAppliedSeconds int
	LastAppliedKnown   bool
}

var state struct {
	sync.RWMutex
	drifts      map[string]TimeDrift
	corrections map[string]CorrectionStatus
}

func init() {
	state.drifts = make(map[string]TimeDrift)
	state.corrections = make(map[string]CorrectionStatus)
}

func Set(deviceID string, d TimeDrift) {
	state.Lock()
	state.drifts[deviceID] = d
	state.Unlock()
}

func Get(deviceID string) (TimeDrift, bool) {
	state.RLock()
	defer state.RUnlock()
	d, ok := state.drifts[deviceID]
	return d, ok
}

func All() map[string]TimeDrift {
	state.RLock()
	defer state.RUnlock()
	out := make(map[string]TimeDrift, len(state.drifts))
	for k, v := range state.drifts {
		out[k] = v
	}
	return out
}

func SetCorrectionMode(deviceID, mode string) {
	state.Lock()
	st := state.corrections[deviceID]
	st.Mode = mode
	state.corrections[deviceID] = st
	state.Unlock()
}

func MarkCorrectionApplied(deviceID string, seconds int, at time.Time) {
	state.Lock()
	st := state.corrections[deviceID]
	st.LastAppliedKnown = true
	st.LastAppliedSeconds = seconds
	st.LastAppliedAt = at
	state.corrections[deviceID] = st
	state.Unlock()
}

func AllCorrections() map[string]CorrectionStatus {
	state.RLock()
	defer state.RUnlock()
	out := make(map[string]CorrectionStatus, len(state.corrections))
	for k, v := range state.corrections {
		out[k] = v
	}
	return out
}

package web

import (
	"testing"
	"time"
)

func TestPhysicalOperationShutdownGateWaitsAndRejectsNewWork(t *testing.T) {
	s := NewServer(nil, 0)
	if !s.beginPhysicalOperation() {
		t.Fatal("первая физическая операция должна быть принята до остановки")
	}

	done := make(chan struct{})
	go func() {
		s.stopAcceptingPhysicalOperations()
		s.waitPhysicalOperations()
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("ожидание завершилось, хотя физическая операция ещё активна")
	case <-time.After(20 * time.Millisecond):
	}

	if s.beginPhysicalOperation() {
		s.endPhysicalOperation()
		t.Fatal("новая физическая операция не должна приниматься после начала остановки")
	}

	s.endPhysicalOperation()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("остановка не дождалась завершения физической операции")
	}
}

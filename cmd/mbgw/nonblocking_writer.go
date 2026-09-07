package main

import "io"

// nonBlockingWriter развязывает рабочие goroutine шлюза от потенциально
// блокирующего получателя лога. На старой Windows Console режим выделения
// текста (в заголовке окна появляется «Выбрать») способен приостановить
// запись в stdout. Если писать в os.Stdout синхронно через io.MultiWriter,
// такой режим может остановить и goroutine, которая в этот момент пишет лог.
//
// Этот writer используется только для консольной копии лога: файл
// mbgw_server.log и внутренний logbuf остаются синхронными и не теряются.
// Если консоль не успевает принимать строки, лишние строки именно в консоли
// отбрасываются, но работа шлюза не блокируется.
type nonBlockingWriter struct {
	ch chan []byte
}

func newNonBlockingWriter(dst io.Writer, buffer int) io.Writer {
	if buffer <= 0 {
		buffer = 256
	}
	w := &nonBlockingWriter{ch: make(chan []byte, buffer)}
	go func() {
		for p := range w.ch {
			_, _ = dst.Write(p)
		}
	}()
	return w
}

func (w *nonBlockingWriter) Write(p []byte) (int, error) {
	copyOfP := append([]byte(nil), p...)
	select {
	case w.ch <- copyOfP:
	default:
		// Консоль отстаёт/заблокирована. Файл и web-log уже получили строку
		// через остальные ветки io.MultiWriter, поэтому здесь безопаснее
		// потерять только консольную копию, чем остановить опрос.
	}
	return len(p), nil
}

package main

import (
	"io"
	"strings"
)

// russianLogWriter нормализует старые англоязычные служебные метки перед
// тем, как строка попадёт в консоль, mbgw_server.log и web-log. Это позволяет
// не переписывать одномоментно каждый исторический log.Printf во всех
// пакетах, но гарантирует оператору единый русский эксплуатационный лог.
// Технические имена протоколов/полей (Modbus, SQL Server, ID_PP,
// PointMains, SQLITE_BUSY и т.п.) намеренно не переводятся.
type russianLogWriter struct {
	dst      io.Writer
	replacer *strings.Replacer
}

func newRussianLogWriter(dst io.Writer) io.Writer {
	return &russianLogWriter{
		dst: dst,
		replacer: strings.NewReplacer(
			"[INFO]", "[ИНФО]",
			"[OK]", "[ОК]",
			"[WARN]", "[ПРЕДУПРЕЖДЕНИЕ]",
			"[ERROR]", "[ОШИБКА]",
			"[FATAL]", "[КРИТИЧЕСКАЯ ОШИБКА]",
			"[WEB]", "[ВЕБ]",
			"[SAVE]", "[СОХРАНЕНО]",
			"[poller]", "[ОПРОС]",
			"[es-sync]", "[СИНХРОНИЗАЦИЯ С ЭС]",
			"gap-scan:", "проверка пропусков:",
			"VKM архив main:", "архив ВКМ:",
			"VKM дозабор main:", "дозабор архива ВКМ:",
			"архив hourly:", "часовой архив:",
			"дозабор hourly:", "дозабор часового архива:",
		),
	}
}

func (w *russianLogWriter) Write(p []byte) (int, error) {
	translated := w.replacer.Replace(string(p))
	n, err := io.WriteString(w.dst, translated)
	if err != nil {
		return 0, err
	}
	if n != len(translated) {
		return 0, io.ErrShortWrite
	}
	// Для контракта io.Writer возвращаем длину исходного буфера, который
	// принял этот writer, хотя русская метка может быть длиннее в байтах.
	return len(p), nil
}

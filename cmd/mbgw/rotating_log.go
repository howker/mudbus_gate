package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// rotatingFile — простой io.Writer с ротацией по размеру, без сторонних
// библиотек (сервер работает офлайн, -mod=vendor, старый Windows Server —
// внешние пакеты логротации сюда не годятся, см. общий принцип проекта
// "минимум зависимостей", уже применённый и к остальным частям mbgw).
//
// Найдено оператором живьём (2026-08-29): mbgw_server.log и *_akron_live.jsonl
// открываются с os.O_APPEND и растут БЕСКОНЕЧНО, пока процесс работает —
// без перезапуска сервера (что на проде случается редко и не по расписанию)
// файл никогда не обрезается и не архивируется.
//
// Правило: как только текущий файл превышает maxBytes, он закрывается,
// переименовывается в файл с меткой времени (например
// mbgw_server.log.20260829-153000), и открывается новый пустой файл с
// прежним именем — так что log.SetOutput(...) не нужно вызывать заново,
// сам io.Writer остаётся тем же объектом. После ротации лишние архивные
// файлы (сверх keepFiles штук, считая от самых новых) удаляются.
type rotatingFile struct {
	path      string
	maxBytes  int64
	keepFiles int

	mu   sync.Mutex
	f    *os.File
	size int64
}

// newRotatingFile открывает (или создаёт) path и сразу узнаёт его текущий
// размер — важно при перезапуске сервера с уже существующим большим
// файлом: ротация должна сработать на первой же следующей записи, а не
// только после того, как файл разрастётся ЕЩЁ на maxBytes сверху.
func newRotatingFile(path string, maxBytes int64, keepFiles int) (*rotatingFile, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		return nil, err
	}
	var size int64
	if info, statErr := f.Stat(); statErr == nil {
		size = info.Size()
	}
	return &rotatingFile{path: path, maxBytes: maxBytes, keepFiles: keepFiles, f: f, size: size}, nil
}

// Write реализует io.Writer. Ротация проверяется ПЕРЕД записью очередной
// порции — так однократная огромная запись не может разово "проскочить"
// далеко за maxBytes перед тем, как сработает проверка.
//
// Про потокобезопасность: пакет log сериализует вызовы через собственный
// внутренний мьютекс перед тем, как передать байты сюда (см. log.Logger.
// Output), так что конкурентные log.Printf из разных горутин сюда и так
// приходят по одному — mu здесь для дополнительной надёжности и на случай,
// если этот же rotatingFile когда-нибудь используют напрямую в обход
// пакета log.
func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.maxBytes > 0 && r.size+int64(len(p)) > r.maxBytes {
		r.rotateLocked()
	}

	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// rotateLocked выполняет саму ротацию — вызывающая сторона уже держит r.mu.
// Ошибки здесь намеренно не прерывают запись: лучше продолжить писать в
// старый (пусть и большой) файл, чем совсем потерять логирование, если,
// например, антивирус временно держит файл открытым при переименовании.
func (r *rotatingFile) rotateLocked() {
	_ = r.f.Close()

	backupName := fmt.Sprintf("%s.%s", r.path, time.Now().Format("20060102-150405"))
	if err := os.Rename(r.path, backupName); err != nil {
		// Переименовать не удалось — пробуем всё равно открыть/дописать
		// тот же файл заново, не теряя логирование вовсе.
		if f, openErr := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666); openErr == nil {
			r.f = f
			if info, statErr := f.Stat(); statErr == nil {
				r.size = info.Size()
			}
		}
		return
	}

	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		// Совсем не смогли открыть новый файл — переоткрываем старое имя
		// как есть (уже переименованное) в качестве аварийного варианта,
		// чтобы хоть куда-то писать.
		if f2, err2 := os.OpenFile(backupName, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666); err2 == nil {
			r.f = f2
		}
		return
	}
	r.f = f
	r.size = 0

	r.cleanupOldLocked()
}

// cleanupOldLocked удаляет самые старые архивные файлы (path + "." +
// метка_времени) сверх keepFiles штук. Имена архивов включают дату-время
// в сортируемом формате (ГГГГММДД-ЧЧММСС), поэтому простой лексикографический
// sort.Strings уже даёт хронологический порядок — отдельный парсинг времени
// не нужен.
func (r *rotatingFile) cleanupOldLocked() {
	if r.keepFiles <= 0 {
		return // 0 или отрицательное — хранить архивы бессрочно, ничего не удалять
	}

	dir := filepath.Dir(r.path)
	base := filepath.Base(r.path)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	var backups []string
	prefix := base + "."
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), prefix) {
			backups = append(backups, filepath.Join(dir, e.Name()))
		}
	}
	if len(backups) <= r.keepFiles {
		return
	}

	sort.Strings(backups) // ГГГГММДД-ЧЧММСС в имени -> лексикографический порядок = хронологический
	toDelete := backups[:len(backups)-r.keepFiles]
	for _, path := range toDelete {
		_ = os.Remove(path)
	}
}

// Close закрывает текущий файл — вызывается через defer в runServer(),
// как и раньше с обычным os.File.
func (r *rotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}

# CONTRACTS — Окончательные контракты «Модбас-Шлюз»

Контрактные сигнатуры, стейт-машины, правила кодеков и протоколов. **Эти определения изменять нельзя** без явного решения владельца архитектуры. Все байтовые векторы ниже проверены вычислением и обязательны как golden-фикстуры (`testdata/golden_vectors.json`).

Соглашения Go: пакет указан в заголовке блока; `context.Context` обязателен в блокирующих вызовах; ошибки — обёрнутые значения из раздела 9; время — `time.Time` (UTC) в коде, epoch-ms на границе хранилища/API.

---

## 1. Transport (пакет `internal/transport`) — КОНТРАКТ

```go
type Kind string
const (
    KindModbusTCP Kind = "modbus_tcp"
    KindRTUSerial Kind = "rtu_serial"
    KindTCPSerial Kind = "tcp_serial"
    KindGSM       Kind = "gsm"   // post-MVP
    KindVPN       Kind = "vpn"   // post-MVP
)

type Params struct {
    Kind            Kind
    Host            string        // tcp/tcp_serial
    Port            int
    COM             string        // rtu_serial: "COM3" / "/dev/ttyUSB0"
    Baudrate        int           // 9600 по умолчанию
    Parity          string        // "none"|"even"|"odd"
    StopBits        int           // 1|2
    ResponseTimeout time.Duration // таймаут ответа
    InterframeDelay time.Duration // межкадровая пауза RTU
    Retries         int
}

type Transport interface {
    Open(ctx context.Context) error
    Close() error
    Send(ctx context.Context, frame []byte) error
    Receive(ctx context.Context, timeout time.Duration) ([]byte, error)
    Info() Params
}
```
- **Входы/выходы:** `Send` отправляет готовый кадр целиком; `Receive` возвращает один принятый кадр (для RTU — собранный по межкадровой паузе; для TCP — по длине из MBAP).
- **Ошибки:** `ErrTimeout`, `ErrClosed`, `ErrTransport` (раздел 9).
- **Инварианты:** один `Transport` = один физический канал; **не потокобезопасен** — внешняя сериализация через `lease`/`channel`. `Open` идемпотентен (повторный `Open` на открытом — no-op). После `Close` любой вызов → `ErrClosed`.
- **Edge-cases:** частичный кадр RTU → ждать до `InterframeDelay`, затем вернуть собранное; мусор на линии → вернуть как есть, разбор отдаёт `ErrFrame` выше.
- **Конкурентность:** вызовы одного экземпляра только из одной горутины-владельца.

---

## 2. Protocol (пакет `internal/protocol`) — КОНТРАКТ

```go
type Request struct {
    Unit     uint8         // modbus slave id / merkuriy network addr
    Function uint8         // код функции/команды
    Address  uint16        // стартовый адрес (modbus)
    Quantity uint16        // кол-во регистров (modbus)
    Payload  []byte        // данные для записи / параметры команды
}

type Response struct {
    Unit     uint8
    Function uint8
    Data     []byte        // тело ответа без адреса/CRC
    Exception uint8        // 0 если нет; иначе код исключения modbus
}

type Protocol interface {
    Name() string                                  // "modbus" | "merkuriy"
    BuildFrame(req Request) ([]byte, error)        // собрать кадр (с CRC)
    ParseFrame(raw []byte) (Response, error)       // разобрать кадр (проверить CRC)
    Transact(ctx context.Context, t Transport, req Request) (Response, error)
}
```
- **Инварианты:** `Transact` = BuildFrame → Send → Receive → ParseFrame, с ретраями по `Params.Retries` и backoff (200/400/800 мс). Любая транзакция атомарна.
- **Ошибки:** `ErrCRC`, `ErrFrame`, `ErrException{Code}`, `ErrTimeout`. Исключение Modbus 06 (BUSY) → `ErrException{Code:6}`, обрабатывается вызывающим (archive/session) как «повторить позже».
- **Edge-cases:** эхо-несовпадение Unit/Function → `ErrFrame`; длина < минимальной → `ErrFrame`.

### 2.1. Modbus framing и CRC — golden-векторы (ОБЯЗАТЕЛЬНЫ)
CRC16 Modbus: poly `0xA001`, init `0xFFFF`, на проводе младший байт первым.

| Сценарий | Кадр (hex) |
| :-- | :-- |
| Read Input (04), slave 1, addr 2000, qty 2 | `01 04 07 D0 00 02 71 46` |
| Ответ: 2 рег = float 123.4567 | `01 04 04 42 F6 E9 D5 81 C1` |
| Исключение 06 BUSY на функцию 04 | `01 84 06 C3 02` |
| Write Single (06), reg 200, value 1 | `01 06 00 C8 00 01 C9 F4` |

### 2.2. Modbus исключения (полная карта)
`01` ILLEGAL FUNCTION, `02` ILLEGAL DATA ADDRESS, `03` ILLEGAL DATA VALUE, `04` SLAVE DEVICE FAILURE, `05` ACKNOWLEDGE, `06` SLAVE DEVICE BUSY, `07` NEGATIVE ACKNOWLEDGE, `08` MEMORY PARITY ERROR. Признак исключения: бит 7 в коде функции ответа (`Function | 0x80`).

### 2.3. Суб-диалект ВЗЛЕТ
- **Функция 17 (0x11):** Report Slave ID / инфо о приборе (версия ПО → выбор `firmware_variant`).
- **Функция 65 (0x41):** чтение архива. Параметры: тип архива (индекс), режим доступа (по индексу/по времени), значение индекса/времени. Ответ — бинарная запись фикс. длины (`record_size`), на запись — CRC (см. 5.4). Доступ по времени: запрашиваемое время округляется до периода архивации; при отсутствии записи возвращается пустая запись той же длины (все нули). Маркер несуществующей записи: время `0x00000000` или `0xFFFFFFFF` — запись считать несуществующей, предыдущая — последней.
- **DST/перевод времени:** при чтении часового архива учитывать пропуск записи (летнее время) и удвоение (зимнее). `dst_aware: true` в профиле включает соответствующую логику.

---

## 3. Merkuriy (пакет `internal/protocol/merkuriy`) — КОНТРАКТ
Кадр: `[адрес 1Б][код 1Б][№параметра 0..1Б][расширение 0..1Б][данные N][CRC16-modbus 2Б]`. CRC — тот же полином, что Modbus. Конец кадра — по межбайтовой паузе. Адреса: `0x00` универсальный, `0x01..0xF0` индивидуальные, `0xFE` широковещательный.

Golden-векторы:
| Сценарий | Кадр (hex) |
| :-- | :-- |
| Тест связи, addr 1, cmd 0x00 | `01 00 00 20` |
| Открытие канала, addr 1, уровень 1, пароль 6×0x01 | `01 01 01 01 01 01 01 01 01 7A 11` |

Байт состояния обмена (младшая тетрада кода в ответе): `X0` норма, `X1` недопустимая команда/параметр, `X2` внутренняя ошибка счётчика, `X3` недостаточен уровень доступа, `X4` внутренние часы не корректированы, `X5` канал связи не открыт. Где `X` — копия запрошенного кода.

Режимы: **длинный ответ** (до 255 байт — для архивов/профилей мощности), **режим повтора кода запроса** (для нестабильных каналов GPRS — прибор повторяет код в ответе).

---

## 4. Session (пакет `internal/session`) — КОНТРАКТ + СТЕЙТ-МАШИНА

```go
type Session interface {
    Open(ctx context.Context) error      // open/auth/preamble
    KeepAlive(ctx context.Context) error  // продление сессии (Меркурий 240c)
    Close(ctx context.Context) error
    State() SessionState
}

type SessionState string
const (
    SessClosed     SessionState = "closed"
    SessOpening    SessionState = "opening"
    SessPreamble   SessionState = "preamble"   // byte order detect/write
    SessAuth       SessionState = "auth"
    SessReady      SessionState = "ready"
    SessError      SessionState = "error"
)
```

### 4.1. Стейт-машина сессии (lifecycle open/auth/preamble/keepalive/close)
```
closed --Open()--> opening --(connect ok)--> preamble --(byteorder ok)--> auth --(auth ok)--> ready
opening --(connect fail)--> error
preamble/auth --(fail)--> error
ready --(idle > keepalive_interval)--> KeepAlive() --(ok)--> ready
ready --(keepalive fail)--> error
any --Close()--> closed
error --Open()--> opening   (повторная попытка)
```

### 4.2. Профили сессий
- **modbus_byteorder_auth (ВКМ):** preamble = byte order **detect** по контрольным константам (рег.110 int32 = 1234567890, рег.112 float = 123.4567, рег.114 double = 123.4567890123456); затем auth (запись рег.200/201), чтение уровня рег.203. Троттлинг записи пароля: 1/30c; нарушение → исключение 06 BUSY → ждать и повторить. byte order сбрасывается при перезапуске прибора → detect выполнять при каждом `Open`.
- **modbus_access_level (ИВК):** установить требуемый уровень доступа (Работа/Сервис/Настройка) перед операциями, требующими его.
- **merkuriy_channel:** `Open` = тест связи (cmd 0x00) → открытие канала (cmd 0x01, уровень+пароль 6Б). `KeepAlive` — повторять до истечения 240 c (интервал по умолчанию 200 c). `Close` — закрытие канала (cmd 0x02).
- **none (ТСРВ):** сессия не требуется; `Open`/`Close` — no-op, `State` сразу `ready`.

---

## 5. Codec (пакет `internal/codec`) — КОНТРАКТ + ТАБЛИЦЫ

```go
type DataType string // см. перечень в profile.schema.json (definitions.datatype)

type ByteOrder string // "0123"|"3210"|"1032"|"2301" (32-бит); "01234567"... (64-бит)

type Decoder interface {
    Decode(regs []uint16, order ByteOrder) (value any, err error)
}
type Encoder interface {
    Encode(value any, order ByteOrder) (regs []uint16, err error)
}
```
Регистры Modbus — 16-бит, на проводе big-endian внутри регистра. Многорегистровые типы (int32/uint32/float/double/композиты) **читаются одним запросом** (атомарно).

### 5.1. Порядок байт — golden (для значения с байтами ABCD = `42 F6 E9 D5`, float 123.4567)
| ByteOrder | Байты | Расшифровка |
| :-- | :-- | :-- |
| 0123 | `42 F6 E9 D5` | ABCD (high word first) |
| 3210 | `D5 E9 F6 42` | DCBA |
| 1032 | `F6 42 D5 E9` | BADC (byte-swap внутри слов) |
| 2301 | `E9 D5 42 F6` | CDAB (word-swap) |

### 5.2. Тип-векторы (ABCD, golden)
| Тип | Значение | Байты ABCD |
| :-- | :-- | :-- |
| float | 123.4567 | `42 F6 E9 D5` |
| int32 | 1234567890 | `49 96 02 D2` |
| double | 123.4567890123456 | `40 5E DD 3C 07 FB 4C 93` |
| scaled_int (×0.01) | 25.37 | uint16 `09 E9` (=2537) |
| u32+float | 12345 + 0.6789 = 12345.6789 | u32 `00 00 30 39` + float `3F 2D CC 64` |
| long+float | 4200 + 0.5 = 4200.5 | i32 `00 00 10 68` + float `3F 00 00 00` |
| stInfoEvent | возникновение НС №5 | `05`; снятие НС №5 → `85` |

### 5.3. Правила композитов
- **u32+float (ИВК):** целая часть = uint32, дробная = float; итог = `float64(int_part) + float64(frac)`. Адреса полей берутся из профиля (`compose.int_part`, `compose.frac_part`) либо последовательно.
- **long+float (ТСРВ):** целая часть = signed int32, дробная = float; итог = `float64(long) + float64(frac)`.
- **scaled_int:** `value = raw * scale` (scale из профиля, напр. 0.01, 0.001).
- **stInfoEvent (1 байт):** бит7 = тип события (0 возникновение / 1 снятие); биты 0..4 = номер события (для журнала НС ТС: 0..31; для журнала НС: 0 пропажа связи, 1 пропажа питания; для журнала отказов — карта типов отказа).

### 5.4. CRC архивной записи (ТСРВ)
Последние 2 байта записи (`crc: true` в record_layout) — контрольная сумма записи. При чтении проверять; если не сходится — пометить `crc_ok=false` и не использовать значения (прибор при повреждении возвращает кадр с флагом ошибки).

---

## 6. ArchiveReader (пакет `internal/archive`) — КОНТРАКТ

```go
type ArchiveQuery struct {
    DeviceID  string
    ArchiveID string        // id из профиля
    Instance  int           // трубопровод/теплосистема (0 если нет)
    From, To  time.Time     // период (для time-access)
    FromIndex, ToIndex int  // диапазон индексов (для index-access)
}

type ArchiveRecord struct {
    RecordTS time.Time
    Fields   map[string]any  // декодированные поля record_layout
    CRCOK    bool
    Raw      []byte
}

type ArchiveReader interface {
    Strategy() string
    Read(ctx context.Context, sess Session, p Protocol, t Transport, q ArchiveQuery,
         prof ArchiveProfile) ([]ArchiveRecord, error)
}
```
Реестр стратегий: имена `mb_request_poll_string`, `mb_indexed_binary`, `mb_func65`, `merkuriy_long_response` (регистрируются в `init()`).

### 6.1. mb_request_poll_string (ВКМ) — алгоритм (КОНТРАКТ)
1. Занять lease контекста (per_interface).
2. Записать рег. запроса: id(7900), pipe(7901), start_time(7902-7907), end_time(7908-7913); **последним** — options(7914) (запуск сбора).
3. Опрашивать status(8000) с интервалом 200 мс до `collect_timeout_s` (15 c):
   - 0 expired/not accepted → ошибка; 1 collecting → ждать; 2 ready → читать; 3 no_records → пусто; 4/5/6 ошибки параметров; 7 too_large.
4. При ready: прочитать len(8002), затем строку 8003.. длиной len; распарсить формат `тег{шапка}=значение ед.изм;`.
5. Если при шаге 2 пришло исключение 06 BUSY — предыдущий запрос ещё жив; ждать (до result_retention_s=300c) и повторить.
6. Освободить lease.

### 6.2. mb_indexed_binary (ИВК) — алгоритм
1. Определить адрес/индекс записи (по времени или индексу) в кольцевом буфере.
2. Прочитать блок записей (по `record_size`), декодировать каждую по record_layout.
3. Учесть цикличность буфера и `firmware_variant` (разный формат по версии ПО).

### 6.3. mb_func65 (ТСРВ) — алгоритм
1. Сформировать запрос функции 65: тип архива (type_index), режим (время/индекс), значение.
2. Прочитать бинарную запись (record_size); проверить CRC записи (5.4).
3. Для time-access: округлить время до периода; при отсутствии — пустая запись; маркеры 0x00000000/0xFFFFFFFF → конец данных.
4. `dst_aware`: обработать пропуск/удвоение записи.

### 6.4. merkuriy_long_response (Меркурий) — алгоритм
1. Открыть канал (session). Отправить команду чтения профиля/журнала (cmd из params).
2. Принять длинный ответ (до 255 байт), при необходимости — постранично; режим повтора кода для нестабильного канала.
3. Декодировать записи по record_layout.

---

## 7. Channel (пакет `internal/channel`) — СТЕЙТ-МАШИНА
```
ok --(timeout/ошибки>=порог/деградация)--> degraded --(порог превышен)--> down
down --(failover)--> [переключение на backup-канал], запись comm_event(stage=failover)
down/degraded --(успешный тест/опрос)--> ok   (failback ТОЛЬКО если явно включён; по умолчанию ВЫКЛ)
```
Порог по умолчанию: 3 подряд неуспеха → degraded; 5 → down → failover. Тест канала: лёгкая транзакция (чтение известного регистра / тест связи Меркурия).

---

## 8. Redundancy (пакет `internal/redundancy`) — СТЕЙТ-МАШИНА (post-MVP, интерфейс в MVP)
```
Normal(active) --(heartbeat standby потерян)--> Degraded
Normal(standby) --(heartbeat active потерян > grace)--> Failover --(захват lease всех устройств)--> active
Failover --(старый active вернулся)--> Recovery --(нет авто-failback)--> остаётся как есть до ручного switchover
```
Защита от split-brain: захват устройства возможен только через `device_leases` (единственный владелец). Узел без действующего lease не опрашивает.

---

## 9. Ошибки (пакет `internal/errs`) — КОНТРАКТ
```go
var (
    ErrTimeout   = errors.New("timeout")
    ErrClosed    = errors.New("transport closed")
    ErrTransport = errors.New("transport error")
    ErrCRC       = errors.New("crc mismatch")
    ErrFrame     = errors.New("malformed frame")
    ErrNoData    = errors.New("no data")
    ErrBusy      = errors.New("device busy")        // из исключения 06
    ErrAuth      = errors.New("auth failed")
    ErrLease     = errors.New("device lease held by another owner")
)
type ErrException struct { Code uint8 }
func (e ErrException) Error() string { ... }   // "modbus exception N"
```
Правило: библиотечные функции возвращают обёрнутые ошибки (`fmt.Errorf("...: %w", ErrX)`); вызывающий сравнивает через `errors.Is`.

---

## 10. Lease (пакет `internal/lease`) — КОНТРАКТ
```go
type Lease interface {
    Acquire(ctx context.Context, deviceID, context string, ttl time.Duration) (release func(), err error)
}
```
- **Инвариант:** одно устройство — один владелец одновременно (анти-двойной-опрос). Stateful-сессия (ВКМ-архив) сериализуется в пределах `context` (per_interface). MVP — локальный (in-memory + таблица device_leases); post-MVP — распределённый (для резерва).
- **Ошибка:** занято другим → `ErrLease`.

---

## 11. Storage (пакет `internal/storage`) — КОНТРАКТ
Интерфейс `Repo` скрывает СУБД. Доменные методы (полный список в LLD): `UpsertDevice`, `ListDevices`, `GetDevice`, `SaveReadingCurrent`, `ListReadingsCurrent`, `AppendHistory`, `SaveArchiveRecord`, `AppendCommEvent`, `RaiseAlarm`, `WriteAudit`, `AcquireLease`/`ReleaseLease`, и т.д. Схема — `sql/schema.sql`. Время — epoch-ms. JSON-поля — валидный JSON-текст.

---

## 12. Конкурентность (сводно)
- `Transport` и `Session` — не потокобезопасны; владелец — одна горутина опроса под `lease`.
- `Repo` — потокобезопасен (внутренняя сериализация записи; SQLite — один писатель).
- `monitor.Bus` — потокобезопасен (publish/subscribe).
- `Protocol` (stateless) — потокобезопасен.

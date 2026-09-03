# LLD — Low-Level Design «Модбас-Шлюз»

Описание каждого пакета: назначение, файлы, публичные сущности, что контрактно (нельзя менять без решения), разрешённые/запрещённые зависимости, обязательные тесты, DoD. Сигнатуры контрактных интерфейсов — в CONTRACTS.md (источник истины).

**Глобальные запреты зависимостей:** ни один пакет не обращается в интернет; импорт `net/http` только в `api`; импорт СУБД-драйверов только в `storage/sqlite` и `storage/postgres`; `protocol/*` не зависит от `storage`, `api`, `scheduler`. Слои зависят только «вниз»: api→(poller,storage,monitor,auth); poller→(scheduler,channel,session,protocol,archive,pollcore,lease,quality,storage,monitor); pollcore→(protocol,codec,profile,pointresolver); archive→(protocol,codec,session); session→(protocol,codec); protocol→(transport,codec); codec→∅; profile→∅.

**Условные обозначения:** [C] — контрактный файл (менять только по ADR); [M] — изменяемый агентом.

---

## cmd/mbgw
- **Назначение:** точка входа и режимы запуска.
- **Файлы:** `main.go` [M], `run.go` [M], `cli.go` [M], `simulate.go` [M], `service_windows.go` [M], `service_unix.go` [M].
- **Публичное:** функция `main`; режимы `run|cli|simulate|install-service`.
- **Зависимости:** все internal-пакеты верхнего уровня. **Запрещено:** бизнес-логика в cmd (только сборка зависимостей и запуск).
- **Тесты:** smoke (binary стартует, `simulate` отдаёт ответы).
- **DoD:** `mbgw run --config testdata/config.mock.yaml` поднимает API; `mbgw simulate vkm360|ivk-ter|tsrv024|merkuriy` работает.

## internal/transport
- **Назначение:** байтовый канал (TCP/serial/tcp-serial).
- **Файлы:** `transport.go` [C], `tcp.go` [M], `serial.go` [M], `tcpserial.go` [M], `transport_test.go` [M].
- **Публичное:** `Transport`, `Params`, `Kind` (CONTRACTS §1).
- **Зависимости:** stdlib `net`, `go.bug.st/serial`. **Запрещено:** protocol/codec/storage.
- **Тесты:** unit на loopback (send/receive/timeout/close); RTU-сборка кадра по паузе.
- **DoD:** обмен байтами по TCP и serial-loopback; таймаут возвращает `ErrTimeout`.

## internal/protocol
- **Назначение:** Modbus + Меркурий, CRC, транзакции.
- **Файлы:** `protocol.go` [C], `crc.go` [C], `crc_test.go` [M], `modbus/modbus.go` [M], `modbus/vzlet.go` [M], `modbus/exceptions.go` [C], `modbus/modbus_test.go` [M], `merkuriy/merkuriy.go` [M], `merkuriy/status.go` [C], `merkuriy/merkuriy_test.go` [M].
- **Публичное:** `Protocol`, `Request`, `Response` (CONTRACTS §2); `CRC16Modbus(data) uint16`.
- **Зависимости:** transport, codec. **Запрещено:** storage/api/scheduler.
- **Тесты (обязательны):** golden CRC и кадры из CONTRACTS §2.1/§3; исключение 06; функции 17/65; ошибочные кадры (битый CRC → `ErrCRC`).
- **DoD:** все golden-векторы из `testdata/golden_vectors.json` проходят round-trip build/parse.

## internal/codec
- **Назначение:** типы данных ↔ регистры, порядок байт, композиты, bitfield, stInfoEvent.
- **Файлы:** `codec.go` [C], `numeric.go` [M], `composite.go` [M], `strings.go` [M], `bitfield.go` [M], `byteorder.go` [C], `codec_test.go` [M].
- **Публичное:** `DataType`, `ByteOrder`, `Decoder`, `Encoder`, реестр по имени типа.
- **Зависимости:** только stdlib (`encoding/binary`, `math`). **Запрещено:** всё остальное.
- **Тесты (обязательны):** таблицы CONTRACTS §5.1/§5.2 для каждого типа и порядка байт; ошибки длины.
- **DoD:** decode/encode совпадают с golden для всех типов и 4 порядков байт.

## internal/profile
- **Назначение:** модель профиля, загрузка, валидация по JSON Schema.
- **Файлы:** `profile.go` [C], `load.go` [M], `validate.go` [M], `schema.json` [C], `profile_test.go` [M].
- **Публичное:** структуры профиля (соответствуют `profile.schema.json`); `Load(path)`, `Validate(data)`.
- **Зависимости:** `gopkg.in/yaml.v3`, `jsonschema/v5`. **Запрещено:** protocol/transport/storage.
- **Тесты:** загрузка 4 эталонных профилей; отклонение невалидного.
- **DoD:** все профили из `profiles/` грузятся и валидируются; невалидный отклоняется с понятной ошибкой.

## internal/pointresolver
- **Назначение:** разбор `addr_formula` → адреса для экземпляров.
- **Файлы:** `resolver.go` [M], `resolver_test.go` [M].
- **Публичное:** `Resolve(formula string, vars map[string]int) (uint16, error)`.
- **Зависимости:** только stdlib. **Запрещено:** всё остальное.
- **Тесты:** формулы ВКМ `2000+(pipe-1)*100+8`, `4000+(logical_input-1)*4+2`.
- **DoD:** формулы дают верные адреса для разных номеров экземпляра.

## internal/session
- **Назначение:** жизненный цикл сессии (CONTRACTS §4).
- **Файлы:** `session.go` [C], `modbus_session.go` [M], `merkuriy_session.go` [M], `session_test.go` [M].
- **Публичное:** `Session`, `SessionState`.
- **Зависимости:** protocol, codec. **Запрещено:** storage/api.
- **Тесты:** byte order detect (ВКМ) по golden-константам; стейт-машина переходов; Меркурий keepalive.
- **DoD:** стейт-машина CONTRACTS §4.1 реализована; для каждого прибора `Open` приводит в `ready` на симуляторе.

## internal/archive
- **Назначение:** стратегии чтения архивов (CONTRACTS §6).
- **Файлы:** `archive.go` [C], `registry.go` [C], `recorddecode.go` [M], `mb_request_poll_string.go` [M], `mb_indexed_binary.go` [M], `mb_func65.go` [M], `merkuriy_long_response.go` [M], `archive_test.go` [M].
- **Публичное:** `ArchiveReader`, `ArchiveQuery`, `ArchiveRecord`; реестр стратегий.
- **Зависимости:** protocol, codec, session, profile. **Запрещено:** storage/api.
- **Тесты (обязательны):** golden-записи на симуляторе для каждой стратегии; CRC записи ТСРВ; пустая запись/маркеры; DST.
- **DoD:** все 4 стратегии читают архив с симулятора в `ArchiveRecord`.

## internal/pollcore
- **Назначение:** один сквозной опрос (профиль+протокол+codec → значения).
- **Файлы:** `reader.go` [M], `result.go` [C], `reader_test.go` [M].
- **Публичное:** `Reader.ReadCurrent(...) ([]Reading, error)`; тип `Reading` (point/value/unit/quality/ts).
- **Зависимости:** protocol, codec, profile, pointresolver. **Запрещено:** scheduler/channel/storage.
- **Тесты:** сквозное чтение ВКМ через профиль на симуляторе.
- **DoD:** текущие значения каждого прибора читаются через профиль; ноль захардкоженных адресов.

## internal/quality
- **Назначение:** теги качества (слияние статус-бит прибора + проверки шлюза).
- **Файлы:** `quality.go` [C], `rules.go` [M], `quality_test.go` [M].
- **Публичное:** `Tag` (VALID..NO_DATA), `Evaluate(reading, deviceStatus, rules) Tag`.
- **Зависимости:** profile (quality_map). **Запрещено:** storage/api/protocol.
- **Тесты:** sensor_break бит → INVALID; stale → STALE; диапазон → INVALID; нормальное → VALID.
- **DoD:** на mock-сценарии с ошибкой датчика значение помечается INVALID/NO_DATA с причиной.

## internal/scheduler
- **Назначение:** расписания, окна архивов, приоритеты, очередь.
- **Файлы:** `scheduler.go` [M], `queue.go` [M], `scheduler_test.go` [M].
- **Публичное:** `Scheduler.Next() Task`; `Task{DeviceID, Kind, Priority}`; ограничение глубины очереди (10000).
- **Зависимости:** storage (расписания), monitor. **Запрещено:** protocol/transport.
- **Тесты:** формирование задач по периоду; приоритет ручного опроса; отбрасывание при переполнении.
- **DoD:** задачи формируются по расписанию; ручной опрос вклинивается; переполнение очереди логируется.

## internal/channel
- **Назначение:** каналы, тест, failover (CONTRACTS §7).
- **Файлы:** `manager.go` [M], `failover.go` [M], `channel_test.go` [M].
- **Публичное:** `Manager.Pick(deviceID) (Transport, error)`; `Manager.Test(channelID)`; политика failover/failback.
- **Зависимости:** transport, storage, monitor. **Запрещено:** protocol-специфика.
- **Тесты:** переключение на backup при сбое; отсутствие auto-failback; запись события failover.
- **DoD:** failover работает и журналируется; failback по умолчанию выключен.

## internal/lease
- **Назначение:** аренда устройства (CONTRACTS §10).
- **Файлы:** `lease.go` [C], `lease_test.go` [M].
- **Публичное:** `Lease.Acquire(...)`.
- **Зависимости:** storage (device_leases). **Запрещено:** protocol/api.
- **Тесты:** второй захват того же устройства → `ErrLease`; сериализация stateful-контекста.
- **DoD:** один владелец на устройство; stateful-запросы сериализуются.

## internal/poller
- **Назначение:** оркестратор (scheduler+channel+session+protocol+archive+lease+quality+storage+monitor).
- **Файлы:** `poller.go` [M], `poller_test.go` [M].
- **Публичное:** `Poller.Run(ctx)`.
- **Зависимости:** почти все нижние пакеты. **Запрещено:** api/web.
- **Тесты:** сквозной опрос 4 mock-приборов (текущие + архив) с записью в SQLite; нет двойного опроса.
- **DoD:** все 4 прибора опрашиваются по расписанию; результаты в БД; события в monitor.

## internal/storage
- **Назначение:** репозитории (CONTRACTS §11).
- **Файлы:** `storage.go` [C], `models.go` [C], `sqlite/sqlite.go` [M], `sqlite/migrations.go` [M], `sqlite/migrations/*.sql` [C], `sqlite/repo.go` [M], `postgres/repo.go` [M, post-MVP].
- **Публичное:** `Repo`; доменные структуры `models.go`.
- **Зависимости:** `modernc.org/sqlite`. **Запрещено:** protocol/api/scheduler.
- **Тесты:** миграции применяются; CRUD каждой сущности; seed-данные.
- **DoD:** все сущности сохраняются/читаются; смена бэкенда не трогает домен.

## internal/monitor
- **Назначение:** события сеанса, шина, тревоги, SSE-источник.
- **Файлы:** `events.go` [C], `bus.go` [M], `alarms.go` [M], `monitor_test.go` [M].
- **Публичное:** `Bus.Publish/Subscribe`; типы событий (CONTRACTS §SQL comm_events.stage).
- **Зависимости:** storage (запись событий). **Запрещено:** protocol/transport.
- **Тесты:** publish→subscribe доставка; правило тревоги срабатывает.
- **DoD:** события доставляются подписчику; тревога поднимается/квитируется.

## internal/auth
- **Назначение:** аутентификация, роли, RBAC (MVP — каркас, см. FINAL_TRD §3.4).
- **Файлы:** `auth.go` [M], `roles.go` [C], `rbac.go` [C], `session_web.go` [M], `auth_test.go` [M].
- **Публичное:** `Authenticate(login,pwd)`, `Authorize(user, perm)`, роли operator/engineer/admin/support.
- **Зависимости:** storage, `golang.org/x/crypto/bcrypt`. **Запрещено:** protocol/transport.
- **Тесты:** верный/неверный пароль; запрет действия без права.
- **DoD:** вход выдаёт токен; RBAC запрещает действие без права (когда включён).

## internal/api
- **Назначение:** REST + SSE + TLS + отдача web/dist.
- **Файлы:** `server.go` [M], `middleware.go` [M], `handlers_*.go` [M], `api_test.go` [M].
- **Публичное:** `NewServer(deps).ListenTLS(addr)`.
- **Зависимости:** `net/http`, `go-chi/chi`, poller, storage, monitor, auth. **Контракт:** соответствие `OPENAPI.yaml`.
- **Тесты:** каждый эндпоинт OPENAPI на mock-бэкенде; SSE отдаёт события; коды ошибок.
- **DoD:** ответы соответствуют OPENAPI; SSE работает; TLS поднимается с самоподписанным сертификатом.

## internal/config
- **Назначение:** версионируемая конфигурация, аудит.
- **Файлы:** `config.go` [M], `audit.go` [M], `config_test.go` [M].
- **Публичное:** `Load/Save/Diff/Rollback`; `Audit(entry)`.
- **DoD:** изменение пишется в audit_log; откат возвращает прежнюю версию.

## internal/library (post-MVP-ядро, каркас в MVP)
- **Файлы:** `library.go` [M], `sign.go` [M], `library_test.go` [M].
- **Публичное:** реестр профилей, статусы, версии, подпись.
- **DoD:** профиль публикуется со статусом и контрольной суммой.

## internal/update (post-MVP)
- **Файлы:** `package.go` [C], `server.go` [M], `importer.go` [M], `installer.go` [M], `update_test.go` [M].
- **DoD:** пакет импортируется/устанавливается/откатывается офлайн.

## internal/integration
- **Файлы:** `export.go` [M] (MVP: CSV/JSON/XML), `rest_out.go` [M], `mqtt.go` [M, post-MVP], `extdb.go` [M, post-MVP].
- **DoD:** экспорт CSV/JSON/XML работает.

## internal/wizard (post-MVP)
- **Файлы:** `sniffer.go` [M], `decode_assist.go` [M], `ai_assist.go` [M], `wizard.go` [M].
- **DoD:** см. AI_ASSIST_SPEC (раздел в MOCK_DATA_SPEC §AI).

## internal/redundancy (post-MVP, интерфейс в MVP)
- **Файлы:** `fsm.go` [C], `heartbeat.go` [M], `fencing.go` [M], `redundancy_test.go` [M].
- **DoD:** стейт-машина CONTRACTS §8; захват только через lease.

## internal/i18n
- **Файлы:** `i18n.go` [M], `locales/ru.json` [C].
- **Публичное:** `T(key, args...)`, выбор языка.
- **DoD:** все строки UI/сообщений из каталога; русский по умолчанию.

## internal/simulator
- **Назначение:** эмуляторы приборов для тестов без железа (MOCK_DATA_SPEC).
- **Файлы:** `simulator.go` [M], `vkm360.go` [M], `ivkter.go` [M], `tsrv024.go` [M], `merkuriy.go` [M].
- **Публичное:** `Run(device string, addr string)`.
- **Зависимости:** transport, protocol, codec. **Запрещено:** storage/api.
- **Тесты:** smoke — симулятор отдаёт golden-ответы.
- **DoD:** `mbgw simulate <device>` поднимает эмулятор, отвечающий golden-данными.

## web
- **Назначение:** TypeScript SPA, вшивается в бинарь.
- **Файлы:** `src/**` [M], `dist/**` [C — собранный артефакт].
- **Контракт:** соответствие OPENAPI; mock-фикстуры в `testdata/ui/`.
- **DoD:** UI на mock-бэкенде показывает устройства/значения/живой лог; собран в dist и вшит.

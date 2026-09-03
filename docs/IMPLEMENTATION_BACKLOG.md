# IMPLEMENTATION_BACKLOG — исполнимый backlog «Модбас-Шлюз»

Задачи в порядке реализации. Каждая задача завершается только при выполнении DoD и прохождении указанных smoke-проверок. Агент не меняет контрактные файлы ([C] в LLD) и не принимает архитектурных решений; при нехватке данных — берёт значения из FINAL_TRD/CONTRACTS, а не выдумывает.

Формат задачи: **Цель · Входы · Выходы · Файлы · Тесты · Mock/golden · Smoke · Запрещено · DoD**.

---

## T0. Скелет проекта и офлайн-сборка
- **Цель:** проект собирается офлайн, есть скелет CLI.
- **Входы:** FINAL_TRD §2, LLD cmd/mbgw.
- **Выходы:** собирающийся `mbgw` с режимами-заглушками.
- **Файлы:** `go.mod` (go 1.20 + зависимости), `cmd/mbgw/{main,run,cli,simulate}.go`, `Makefile`, `.env.dev`, `vendor/`.
- **Тесты:** `go build ./...` офлайн.
- **Mock/golden:** —.
- **Smoke:** `make ci` зелёный; `make build-legacy` даёт бинарь; `./mbgw --help`.
- **Запрещено:** добавлять зависимости вне vendor; бизнес-логику в cmd.
- **DoD:** офлайн-сборка и `make ci` проходят; smoke S1, S2 (TEST_STRATEGY) зелёные.

## T1. Transport
- **Цель:** TCP/serial/tcp-serial каналы.
- **Входы:** CONTRACTS §1.
- **Выходы:** реализации Transport.
- **Файлы:** `internal/transport/{transport.go[C],tcp.go,serial.go,tcpserial.go,transport_test.go}`.
- **Тесты:** loopback send/receive/timeout/close; сборка RTU-кадра по паузе.
- **Mock/golden:** loopback-пара кадров.
- **Smoke:** unit-тесты пакета зелёные.
- **Запрещено:** менять `transport.go`.
- **DoD:** обмен по TCP и serial-loopback; `ErrTimeout` при простое.

## T2. Modbus core + CRC
- **Цель:** функции 03/04/06/16, CRC16, исключения.
- **Входы:** CONTRACTS §2, §2.1, §2.2.
- **Выходы:** `Protocol` (modbus), `CRC16Modbus`.
- **Файлы:** `internal/protocol/{protocol.go[C],crc.go[C],crc_test.go}`, `internal/protocol/modbus/{modbus.go,exceptions.go[C],modbus_test.go}`.
- **Тесты:** все golden-кадры §2.1; CRC по `golden_vectors.json`; исключение 06; битый CRC → `ErrCRC`.
- **Mock/golden:** `testdata/golden_vectors.json`.
- **Smoke:** S? (unit) зелёные.
- **Запрещено:** менять protocol.go/crc.go/exceptions.go.
- **DoD:** round-trip build/parse для всех golden-кадров; 06 → `ErrException{6}`.

## T3. Codec
- **Цель:** все типы и порядки байт.
- **Входы:** CONTRACTS §5, profile.schema datatype.
- **Выходы:** Decoder/Encoder для всех типов.
- **Файлы:** `internal/codec/{codec.go[C],numeric.go,composite.go,strings.go,bitfield.go,byteorder.go[C],codec_test.go}`.
- **Тесты:** таблицы §5.1/§5.2 для каждого типа и 4 порядков; ошибки длины.
- **Mock/golden:** golden_vectors.json (float/int32/double/scaled/composite/stInfoEvent).
- **Smoke:** unit зелёные.
- **Запрещено:** менять codec.go/byteorder.go.
- **DoD:** decode/encode совпадают с golden для всех типов.

## T4. Profile + schema + pointresolver
- **Цель:** прибор управляется профилем.
- **Входы:** profile.schema.json, profiles/*.yaml.
- **Выходы:** загрузка/валидация профиля; резолвер формул.
- **Файлы:** `internal/profile/{profile.go[C],load.go,validate.go,schema.json[C],profile_test.go}`, `internal/pointresolver/{resolver.go,resolver_test.go}`.
- **Тесты:** загрузка 4 профилей; отклонение невалидного; формулы ВКМ.
- **Mock/golden:** `profiles/vkm360.yaml` и др.
- **Smoke:** валидация всех профилей в CI.
- **Запрещено:** менять schema.json/profile.go.
- **DoD:** 4 профиля грузятся и валидируются; формулы дают верные адреса.

## T5. Симулятор ВКМ + sweep текущих
- **Цель:** читать текущие ВКМ без железа.
- **Входы:** MOCK_DATA_SPEC (vkm360), profiles/vkm360.yaml.
- **Выходы:** эмулятор ВКМ; `pollcore.Reader`.
- **Файлы:** `internal/simulator/{simulator.go,vkm360.go}`, `internal/pollcore/{reader.go,result.go[C],reader_test.go}`, `cmd/mbgw/simulate.go`.
- **Тесты:** сквозное чтение ВКМ через профиль на симуляторе.
- **Mock/golden:** `testdata/vkm360/*`, expected JSON.
- **Smoke:** S5 (`mbgw simulate vkm360` + чтение).
- **Запрещено:** хардкод адресов ВКМ в pollcore.
- **DoD:** текущие ВКМ читаются через профиль; значения совпадают с expected.

## T6. Session (ВКМ byte order + auth) + архив ВКМ
- **Цель:** преамбула/авторизация и stateful-архив.
- **Входы:** CONTRACTS §4, §6.1.
- **Выходы:** session modbus_byteorder_auth; стратегия mb_request_poll_string; lease.
- **Файлы:** `internal/session/{session.go[C],modbus_session.go,session_test.go}`, `internal/archive/{archive.go[C],registry.go[C],recorddecode.go,mb_request_poll_string.go,archive_test.go}`, `internal/lease/{lease.go[C],lease_test.go}`; доработать `simulator/vkm360.go`.
- **Тесты:** detect byte order по golden-константам; выгрузка архива; BUSY-сериализация.
- **Mock/golden:** сценарий архива ВКМ (строковый ответ).
- **Smoke:** S? архив ВКМ на симуляторе.
- **Запрещено:** менять session.go/archive.go/registry.go/lease.go.
- **DoD:** архив ВКМ парсится; параллельный запрос сериализуется/получает BUSY.

## T7. ИВК-ТЭР и ТСРВ-024 (стратегии архивов)
- **Цель:** бинарные архивы по индексу/времени + функция 65 + CRC записи + DST.
- **Входы:** CONTRACTS §2.3, §6.2, §6.3, §5.4.
- **Выходы:** vzlet 17/65; mb_indexed_binary; mb_func65; симуляторы.
- **Файлы:** `internal/protocol/modbus/vzlet.go`, `internal/archive/{mb_indexed_binary.go,mb_func65.go}`, `internal/simulator/{ivkter.go,tsrv024.go}`, `profiles/{ivk-ter.yaml,tsrv024.yaml}`.
- **Тесты:** декод всех полей record_layout; CRC записи; пустая запись/маркеры; DST-кейс.
- **Mock/golden:** `testdata/ivk-ter/*`, `testdata/tsrv024/*`, expected.
- **Smoke:** S5 для ivk-ter и tsrv024.
- **Запрещено:** хардкод форматов записей вне профиля.
- **DoD:** оба прибора читают текущие+архивы; CRC проверяется; DST покрыт.

## T8. Меркурий
- **Цель:** не-Modbus прибор тем же ядром.
- **Входы:** CONTRACTS §3, §6.4, §4.2 (merkuriy_channel).
- **Выходы:** protocol/merkuriy; merkuriy_session; merkuriy_long_response; симулятор.
- **Файлы:** `internal/protocol/merkuriy/{merkuriy.go,status.go[C],merkuriy_test.go}`, `internal/session/merkuriy_session.go`, `internal/archive/merkuriy_long_response.go`, `internal/simulator/merkuriy.go`, `profiles/merkuriy.yaml`.
- **Тесты:** golden-кадры §3; открытие канала; keepalive; длинный ответ.
- **Mock/golden:** `testdata/merkuriy/*`.
- **Smoke:** S5 для merkuriy.
- **Запрещено:** протокол-специфика вне `protocol/merkuriy`/`session`.
- **DoD:** Меркурий опрашивается; сессия 240c; статусы X0..X5 обрабатываются.

## T9. Storage (SQLite)
- **Цель:** сохранять конфигурацию/данные/события.
- **Входы:** sql/schema.sql, CONTRACTS §11.
- **Выходы:** Repo (SQLite), миграции, seed.
- **Файлы:** `internal/storage/{storage.go[C],models.go[C],sqlite/{sqlite.go,migrations.go,migrations/*.sql[C],repo.go}}`.
- **Тесты:** миграции; CRUD каждой сущности; seed.
- **Mock/golden:** `testdata/seed.sql`.
- **Smoke:** S6 (poller пишет в SQLite).
- **Запрещено:** менять storage.go/models.go/migrations/*.sql.
- **DoD:** все сущности сохраняются/читаются; миграции применяются на чистой БД.

## T10. Quality
- **Цель:** теги качества (статус прибора + проверки шлюза).
- **Входы:** CONTRACTS §5.3, профильные quality_map.
- **Файлы:** `internal/quality/{quality.go[C],rules.go,quality_test.go}`.
- **Тесты:** sensor_break→INVALID; stale→STALE; диапазон; норм→VALID.
- **Mock/golden:** сценарий «обрыв датчика» (vkm360).
- **Smoke:** часть S6 (quality в записях).
- **Запрещено:** менять quality.go.
- **DoD:** на mock-ошибке датчика значение помечено INVALID/NO_DATA с причиной.

## T11. Scheduler + Channel + Lease + Poller
- **Цель:** опрос по расписанию с failover, без двойного опроса.
- **Входы:** CONTRACTS §7, §10.
- **Файлы:** `internal/scheduler/*`, `internal/channel/*`, `internal/poller/{poller.go,poller_test.go}`; доработать `cmd/mbgw/run.go`.
- **Тесты:** формирование задач; failover на backup; нет двойного опроса; запись событий.
- **Mock/golden:** mock-конфиг с основным+резервным каналом.
- **Smoke:** S6 (полный опрос 4 mock-приборов → SQLite).
- **Запрещено:** auto-failback по умолчанию.
- **DoD:** все 4 прибора опрашиваются по расписанию; failover журналируется.

## T12. Monitor + SSE
- **Цель:** события сеанса и тревоги в реальном времени.
- **Входы:** CONTRACTS comm_events; OPENAPI /monitor/stream.
- **Файлы:** `internal/monitor/{events.go[C],bus.go,alarms.go,monitor_test.go}`.
- **Тесты:** publish→subscribe; тревога.
- **Mock/golden:** mock-поток событий.
- **Smoke:** S8 (SSE на mock events).
- **Запрещено:** менять events.go.
- **DoD:** события доставляются; SSE отдаёт их.

## T13. REST API + TLS
- **Цель:** API по OPENAPI.yaml.
- **Входы:** OPENAPI.yaml, CONTRACTS.
- **Файлы:** `internal/api/{server.go,middleware.go,handlers_*.go,api_test.go}`.
- **Тесты:** каждый эндпоинт на mock-бэкенде; коды ошибок; SSE.
- **Mock/golden:** `testdata/api/*` фикстуры.
- **Smoke:** S4, S7, S8.
- **Запрещено:** расхождение с OPENAPI.
- **DoD:** ответы соответствуют OPENAPI; TLS поднимается.

## T14. i18n + Web UI (на mock-бэкенде)
- **Цель:** русский UI на mock-данных.
- **Входы:** OPENAPI, `testdata/ui/*`.
- **Файлы:** `internal/i18n/{i18n.go,locales/ru.json[C]}`, `web/src/**`, `web/dist/**`.
- **Тесты:** UI smoke (рендер устройств/значений/лога на mock).
- **Mock/golden:** `testdata/ui/fixtures.json`.
- **Smoke:** S9 (UI на mock-бэкенде).
- **Запрещено:** захардкоженные строки UI (только через i18n).
- **DoD:** UI на mock-бэкенде показывает устройства/значения/живой лог; собран в dist и вшит.

## T15. Config + Audit + Export (MVP-минимум)
- **Цель:** версионирование настроек, аудит, экспорт.
- **Файлы:** `internal/config/*`, `internal/integration/{export.go,rest_out.go}`.
- **Тесты:** аудит изменения; откат; экспорт CSV/JSON/XML.
- **Smoke:** часть S7.
- **DoD:** изменения в audit_log; откат работает; экспорт отдаёт корректные файлы.

---

## Пост-MVP (заводятся интерфейсами, реализуются после MVP)
- **T16. Auth/RBAC/роли** (auth включается флагом) · **T17. Redundancy** (active/standby) · **T18. Update server** · **T19. Integration gRPC/MQTT/extDB** · **T20. Wizard + AI assist** · **T21. PostgreSQL/Timescale backend**.
Для каждой — тот же формат задачи; DoD см. соответствующие разделы LLD/CONTRACTS.

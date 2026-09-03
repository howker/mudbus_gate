# TEST_STRATEGY — стратегия тестирования «Модбас-Шлюз»

Весь проект разрабатывается и проверяется **без реального оборудования**: через симуляторы, testdata и golden-фикстуры. Реальные приборы — только финальная приёмка на стенде, вне ответственности агента.

## 1. Уровни тестов
1. **Unit** — на каждый пакет (codec, crc, pointresolver, quality, …). Table-driven, детерминированные.
2. **Golden** — байтовые векторы (`testdata/golden_vectors.json`) для CRC, кодеков, кадров. Меняются только при изменении контракта.
3. **Integration** — сквозной путь профиль→протокол→codec→quality→storage через симулятор прибора.
4. **Smoke** — быстрый набор после сборки (раздел 3): проект «живой» без железа.
5. **Soak (post-MVP)** — длительный опрос с инъекцией ошибок канала.

## 2. Правила
- Любой прибор «поддержан» только при наличии симулятора + golden-данных + smoke-сценария.
- Тесты не ходят в сеть. Симуляторы слушают localhost/loopback.
- Каждый PR/коммит проходит `make ci` (build-all + build-legacy + test + lint + vet + валидация профилей/схемы).
- Golden-фикстуры — единственный источник «правильных» байтов; при расхождении правится код, а не фикстура (если контракт не менялся).

## 3. Обязательные smoke-тесты (формализованы)
Каждый — с входными данными, командой, ожидаемым кодом возврата, строками в логах и данными в SQLite/API/UI. Реализуются как `make smoke` (скрипт) + Go integration-тесты.

### S1. Офлайн-сборка
- **Вход:** чистый clone, `vendor/` на месте, переменные офлайн.
- **Команда:** `make ci`
- **Код возврата:** 0
- **Логи:** `build ok`, `tests passed`, `lint ok`
- **Результат:** бинарь `bin/mbgw` создан.

### S2. Legacy-сборка
- **Команда:** `make build-legacy` (тулчейн go1.20.x, GOOS=windows GOARCH=amd64)
- **Код возврата:** 0
- **Результат:** `bin/mbgw_win_legacy.exe` создан.

### S3. Старт с mock-конфигом
- **Вход:** `testdata/config.mock.yaml` (4 устройства на симуляторах, SQLite в temp).
- **Команда:** `mbgw run --config testdata/config.mock.yaml --selftest`
- **Код возврата:** 0
- **Логи:** `config loaded devices=4`, `storage migrated version=1`, `api listening :8443 tls=on`
- **Результат:** процесс поднялся и завершился по `--selftest` без паники.

### S4. API живой
- **Вход:** запущенный `mbgw run` (mock).
- **Команда:** `GET /api/v1/status`
- **Код возврата HTTP:** 200
- **Тело:** `{"version":...,"devices_total":4,...}`
- **Логи:** `GET /status 200`.

### S5. Симулятор прибора
- **Команда (для каждого):** `mbgw simulate vkm360|ivk-ter|tsrv024|merkuriy --addr 127.0.0.1:15020`
- **Проверка:** клиент `mbgw cli read --device <profile> --addr ...` возвращает текущие значения.
- **Код возврата:** 0
- **Данные:** значения совпадают с `testdata/<device>/expected_current.json` (напр., для ВКМ «Массовый расход» = 123.4567 кг/с, quality=VALID).

### S6. Поллер → SQLite
- **Вход:** mock-конфиг (S3).
- **Команда:** `mbgw run --config testdata/config.mock.yaml --run-for 10s`
- **Код возврата:** 0
- **SQLite:** в `readings_current` ≥ N строк по 4 устройствам; в `comm_events` есть `stage=success`; для сценария «обрыв датчика» в `readings_current.quality` = `INVALID`/`NO_DATA`.
- **Логи:** `poll device=vkm360 ok`, `archive device=tsrv024 records=...`.

### S7. REST: устройства и последний опрос
- **Команда:** `GET /api/v1/devices`, `GET /api/v1/devices/{id}/readings`, `POST /api/v1/devices/{id}/poll`
- **Коды:** 200/200/200
- **Данные:** список из 4 устройств; readings с quality; poll возвращает свежие значения.

### S8. SSE / live-monitor
- **Команда:** `GET /api/v1/monitor/stream` (держать 3 c при работающем поллере на mock)
- **Проверка:** приходят события `event: comm` с `stage` из набора; ≥ 1 события.
- **Код возврата:** 200 (поток).

### S9. Web UI на mock-бэкенде
- **Вход:** `web/dist` + mock-фикстуры `testdata/ui/fixtures.json` (или реальный mock-бэкенд).
- **Проверка:** страница «Устройства» рендерит 4 устройства; «Онлайн-опрос» показывает живой лог; значения с quality видны.
- **Тип:** UI smoke (headless или snapshot компонента на фикстурах).

## 4. Карта golden-данных
- `testdata/golden_vectors.json` — CRC, кодеки, кадры Modbus/Меркурий (проверены вычислением).
- `testdata/<device>/expected_current.json` — ожидаемые текущие значения.
- `testdata/<device>/expected_archive.json` — ожидаемые архивные записи.
- `testdata/<device>/scenarios/*.yaml` — сценарии симулятора (см. MOCK_DATA_SPEC).
- `testdata/config.mock.yaml` — конфиг с 4 устройствами на симуляторах.
- `testdata/seed.sql` — seed для SQLite.
- `testdata/ui/fixtures.json` — фикстуры для UI smoke.

## 5. Команды make (обязательный набор)
```
make build         # хост-сборка
make build-all     # кросс-матрица (linux/amd64, windows/amd64, linux/arm64)
make build-legacy  # go1.20.x, windows/amd64 (ярус legacy)
make test          # go test ./...
make lint          # go vet + линтер
make smoke         # S1..S9
make ci            # build-all + build-legacy + test + lint + валидация профилей/схемы
make simulate D=vkm360  # запуск симулятора
```

## 6. Критерий «зелёного» MVP
`make ci` и `make smoke` проходят офлайн; S1–S9 зелёные; все 4 прибора читаются на симуляторах; quality на сценарии ошибки датчика корректен.

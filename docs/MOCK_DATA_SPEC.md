# MOCK_DATA_SPEC — моковые данные, симуляторы, AI-слой

Цель: весь проект разрабатывается дома/в тестовой среде без реальных приборов. Здесь зафиксированы обязательные моковые данные, форматы, сценарии и команды симуляторов.

## 1. Обязательный комплект на каждый прибор
Для каждого из `vkm360`, `ivk-ter`, `tsrv024`, `merkuriy`:
- `profiles/<device>.yaml` — профиль (есть, валиден по схеме).
- `internal/simulator/<device>.go` — эмулятор.
- `testdata/<device>/expected_current.json` — ожидаемые текущие значения.
- `testdata/<device>/expected_archive.json` — ожидаемые архивные записи.
- `testdata/<device>/scenarios/*.yaml` — сценарии поведения симулятора.
- smoke-сценарий (TEST_STRATEGY S5), integration-сценарий, golden-assertions.

## 2. Форматы хранения моков
| Что | Формат | Где |
| :-- | :-- | :-- |
| Байтовые векторы (CRC, кодеки, кадры) | JSON (hex-строки) | `testdata/golden_vectors.json` |
| Ожидаемые текущие значения | JSON | `testdata/<device>/expected_current.json` |
| Ожидаемые архивные записи | JSON | `testdata/<device>/expected_archive.json` |
| Сценарии симулятора | YAML | `testdata/<device>/scenarios/*.yaml` |
| Конфиг тестового стенда | YAML | `testdata/config.mock.yaml` |
| Seed БД | SQL | `testdata/seed.sql` |
| Фикстуры UI | JSON | `testdata/ui/fixtures.json` |

### 2.1. Формат сценария симулятора (YAML) — КОНТРАКТ
```yaml
name: "vkm360 normal"
device: "vkm360"
address: 1
transport: { kind: "modbus_tcp", listen: "127.0.0.1:15020" }
registers:                       # начальное состояние регистров
  IR:
    2008: { type: "float", value: 123.4567 }   # массовый расход ТП1
    4002: { type: "int16", value: 0 }          # слово статуса лог.входа (норма)
  HR:
    110: { type: "int32",  value: 1234567890 }
    112: { type: "float",  value: 123.4567 }
    114: { type: "double", value: 123.4567890123456 }
behaviors:                       # инъекция поведения (см. §3)
  - { on: "any", action: "respond" }
```

### 2.2. Формат expected_current.json — КОНТРАКТ
```json
[
  { "point_name": "Массовый расход", "instance": 1, "value_num": 123.4567, "unit": "кг/с", "quality": "VALID" }
]
```

## 3. Обязательные сценарии симулятора (для каждого прибора)
Симулятор должен уметь воспроизводить, и для каждого есть golden-ожидание:
1. **normal** — корректный ответ.
2. **timeout** — не отвечать (проверка `ErrTimeout`, ретраев, failover).
3. **crc_error** — ответ с битым CRC (`ErrCRC`).
4. **busy** — Modbus-исключение 06 (для ВКМ-архива — сериализация/повтор).
5. **partial_frame** — оборванный кадр (сборка по паузе / `ErrFrame`).
6. **malformed_frame** — мусор/неверная длина (`ErrFrame`).
7. **reconnect** — разрыв и восстановление соединения.
8. **archive_pagination** — архив, читаемый частями (ИВК/ТСРВ/Меркурий long-response).
9. **dst_crossing** — переход летнее/зимнее время (ТСРВ): пропуск/удвоение записи.
10. **stale_quality** — данные не обновляются → quality STALE.
11. **sensor_break** — статус-бит обрыва датчика → quality INVALID/NO_DATA.

Соответствие «сценарий → прибор» (минимум):
| Сценарий | vkm360 | ivk-ter | tsrv024 | merkuriy |
| :-- | :-: | :-: | :-: | :-: |
| normal/timeout/crc/partial/malformed/reconnect/stale | ✔ | ✔ | ✔ | ✔ |
| busy | ✔ | — | — | — |
| archive_pagination | ✔(string) | ✔(index) | ✔(func65) | ✔(long-resp) |
| dst_crossing | — | — | ✔ | — |
| sensor_break | ✔ | ✔ | ✔ | — |

## 4. Запуск симуляторов локально
```
mbgw simulate vkm360   --addr 127.0.0.1:15020 [--scenario testdata/vkm360/scenarios/normal.yaml]
mbgw simulate ivk-ter  --addr 127.0.0.1:15021
mbgw simulate tsrv024  --addr 127.0.0.1:15022
mbgw simulate merkuriy --addr 127.0.0.1:15023 [--scenario .../busy.yaml]
```
- Без `--scenario` грузится `scenarios/normal.yaml`.
- Симулятор отвечает строго по golden-данным прибора.
- `mbgw run --config testdata/config.mock.yaml` поднимает все 4 симулятора + поллер + API (для S3/S6).

## 5. config.mock.yaml (структура) — КОНТРАКТ
```yaml
storage: { backend: "sqlite", dsn: "file:./mbgw_test.db" }
api: { listen: ":8443", tls: { self_signed: true } }
channels:
  - { id: "ch1", kind: "modbus_tcp", params: { host: "127.0.0.1", port: 15020 } }
  - { id: "ch2", kind: "modbus_tcp", params: { host: "127.0.0.1", port: 15022 } }
devices:
  - { id: "d_vkm",  name: "ВКМ mock",  profile: "vkm360",  channel: "ch1", address: 1 }
  - { id: "d_tsrv", name: "ТСРВ mock", profile: "tsrv024", channel: "ch2", address: 1 }
  # ivk-ter, merkuriy аналогично
schedules:
  - { device: "d_vkm",  kind: "current", period_s: 5 }
  - { device: "d_tsrv", kind: "archive", period_s: 3600, window: "02:00-04:00" }
```

## 6. Golden-векторы (уже вычислены, обязательны)
`testdata/golden_vectors.json` содержит проверенные значения: Modbus CRC и кадры (03/04/06/16, исключение 06), IEEE-754 float/double, int32, scaled_int, композиты u32+float и long+float, stInfoEvent, кадры Меркурия (тест связи, открытие канала). Эти значения — эталон для unit/golden-тестов кодеков и протоколов.

---

## 7. AI-слой (dev-LLM и продуктовый ИИ) — спецификация

### 7.1. Dev-LLM (помощник разработки)
- **Назначение:** помогать писать код по этим спецификациям офлайн (генерация кода/тестов, объяснение ошибок сборки, анализ дампов).
- **Границы:** не является частью продукта; не имеет доступа в рантайм; не принимает архитектурных решений — только реализует по контрактам.
- **Тестируется:** косвенно — через прохождение unit/smoke кода, который он помог написать.

### 7.2. Продуктовый ИИ (wizard/ai_assist) — post-MVP
- **Назначение:** помощь инженеру при подключении нового прибора (анализ дампов сниффера, гипотезы о структуре кадра/CRC/полях, **черновик профиля**).
- **Входные данные:** дампы кадров (`testdata/sniffer/*.bin`), документация (текст), частично заполненный профиль.
- **Выходные данные:** предложение структуры (поля/смещения/типы), черновик `profiles/<new>.yaml` со статусом `draft`.
- **Запрещено автоматически:** публиковать профиль в боевой опрос; менять approved-профили; писать в приборы; выходить в интернет. Только предложение, утверждает человек.
- **Офлайн-тест:** на `testdata/sniffer/known_*.bin` (кадры известного прибора со скрытой разметкой) ИИ должен предложить структуру, совпадающую с эталоном ≥ по ключевым полям (адрес/функция/CRC/время). Smoke: `ai_assist analyze testdata/sniffer/known_vkm.bin` возвращает непустой черновик профиля, проходящий валидацию по схеме.

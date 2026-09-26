# P0 post-review — приёмка

База пакета: рабочее дерево после D1–D3 (`584a674` по рабочему репозиторию пользователя; ZIP без `.git`, поэтому хэш внутри этого артефакта не проверяется).

Пакет закрывает только согласованный P0. P1/техдолг намеренно не смешиваются с этим изменением.

## Приёмочные сценарии

1. **Миграция / ИВК-ТЭР 9 точек / ВКМ max 4**
   - `internal/storage/sqlite/repo_device_config_test.go`
   - `TestESVKMChannelsMigrationAllowsNineIVKSlotsIsIdempotentAndKeepsCursor`
   - проверяет 9 старых точек ИВК-ТЭР, сохранение курсора ЭС, идемпотентный повтор миграции, сохранение 9 свежих IVK mappings и отказ пятого слота ВКМ.

2. **Уже мигрированная D1-база со старым глобальным CHECK <= 4**
   - `TestESVKMChannelsRebuildsAlreadyD1ConstrainedSchema`
   - проверяет отдельную повторную миграцию существующей новой таблицы без глобального ограничения слотов.

3. **RTU: чужой адрес, затем свой**
   - `internal/protocol/modbus/modbus_test.go`
   - `TestTransactRTUDiscardsForeignAddressThenAcceptsOwn`.

4. **RTU: неверный byte count для 03/04**
   - `TestTransactRTURejectsWrongReadByteCount`.

5. **RTU: правильный адрес, неверная функция, затем правильная**
   - `TestTransactRTUDiscardsWrongFunctionThenAcceptsOwn`.

6. **RTU: только чужие кадры**
   - `TestTransactRTUOnlyForeignFramesEndsWithError` — ответ не принимается и данные не возвращаются.

7. **Нестандартная функция 0x41**
   - `TestTransactRTUCustomFunctionBindsResponseFunction` — ответ другой нестандартной функции отбрасывается, принимается только ответ 0x41.

8. **Mercury: чужой адрес, затем свой**
   - `internal/protocol/merkuriy/merkuriy_test.go`
   - `TestTransactDiscardsForeignAddressThenAcceptsOwn`.

9. **Dirty healer: одна постоянная плохая точка не удерживает диапазон**
   - `internal/integration/energosphere_dirty_heal_test.go`
   - `TestDirtyHealerIsolatesESFailureAndClosesSourceRange`.
   - `TestDirtyHealerThreeDaysOnePoisonPointDoesNotHoldHealthyWork`.

10. **Старый длинный dirty range дробится до обработки**
    - `internal/storage/sqlite/repo_es_dirty_test.go`
    - `TestOldLongDirtyRangeIsSplitBeforeFirstPass`.
    - соседние периоды больше не склеиваются: `TestDirtyRangesStayBoundedAndDoNotMergeAdjacentPeriods`.

11. **VKM catch-up после задержки 70 минут по всем активным трубам**
    - `internal/device/vkm_hourly_test.go`
    - `TestVKMCatchUpDelayedSeventyMinutesReadsAllMissingPeriodsOnAllPipes`.

12. **Подтверждённое прибором отсутствие записи не спрашивается каждый такт**
    - `TestMissingVKMPeriodsSkipsConfirmedNoRecordsUntilRetryTime`.

13. **Discovery уступает lease плановому архиву между трубами**
    - `internal/device/vkm_discovery_test.go`
    - `TestDiscoveryReleasesLeaseBetweenPipesSoScheduledPollCanInterleave`.

14. **Force reload не обрывается на `ErrVKMNoRecords`**
    - `internal/device/vkm_reload_contract_test.go`
    - `TestForceReloadVKMContinuesPastNoRecords`.
    - production-путь дополнительно пишет пустой период в журнал и продолжает диапазон.

15. **Poller race**
    - существующий `TestPollerAddDeviceWhileRunningDispatches` должен пройти под `-race`; стартовый `len(p.devices)` теперь читается под `devicesMu`.

## Обязательная проверка перед ревью/production

На настоящем рабочем дереве с `vendor/` и Go 1.20.14:

```powershell
$ErrorActionPreference='Stop'; if((go version) -notmatch 'go1\.20\.14'){throw 'ERROR: нужен Go 1.20.14'}; $env:GOFLAGS='-mod=vendor'; $env:GOPROXY='off'; go test ./...; if($LASTEXITCODE){throw 'ERROR: go test'}; go vet ./...; if($LASTEXITCODE){throw 'ERROR: go vet'}; go build ./...; if($LASTEXITCODE){throw 'ERROR: go build'}; git diff --check; if($LASTEXITCODE){throw 'ERROR: git diff --check'}; Write-Host 'OK: P0 WINDOWS/VENDOR' -ForegroundColor Green
```

На Linux, также Go 1.20.14 и тот же vendor:

```sh
GOFLAGS=-mod=vendor GOPROXY=off go test -race ./... && echo 'OK: P0 RACE'
```

Production exe собирать только после полного diff-review.

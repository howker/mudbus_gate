package web

import "net/http"

// api_admin_ui.go отдаёт минимальный административный веб-интерфейс на
// /admin — одна цельная HTML-страница на чистом ES5 (XMLHttpRequest, var,
// конкатенация строк — см. пояснение в handleDashboard: на Windows
// Server 2008 R2/2012 единственный доступный браузер — Internet Explorer,
// у него нет ни Fetch, ни async/await, а отдельной сборки фронтенда в
// проекте нет). Работает только с уже реализованным JSON API
// (api_devices.go/api_probe.go и другие api_*.go).
//
// Намеренно отдаётся на /admin, а не на /: старый диагностический
// дашборд на / (handleDashboard) не трогается и продолжает работать как
// раньше — это новый, дополнительный экран, а не замена, поэтому ничего
// уже работающего сломать нельзя.
//
// Структура: одна HTML-страница, вкладки переключаются показом/скрытием
// блоков (без отдельного роутера, без перезагрузки страницы):
//  0. Главная — сводная таблица статуса всех приборов (отставание
//     архива, расхождение времени прибора), с сортировкой по клику на
//     заголовок столбца; кнопка «Принудительная пересинхронизация с ЭС»
//     прямо в строке прибора (добавлено 2026-08-29, заменила собой
//     «Синхронизировать сейчас» и перенесённый со вкладки «Подключение
//     к ЭС» диапазон дат — 2026-09-02). Активна по умолчанию.
//  1. Приборы — список + форма добавления/редактирования (включая
//     «Проверить прибор» -> POST /api/devices/probe) + удаление.
//  2. Точки ЭС — таблица тег->точка(ID_PP)+множитель для одного прибора;
//     ОБЪЕДИНЕНО (2026-08-31): раньше было отдельно для ВКМ («Каналы ЭС»,
//     4 тега) и отдельно для Akron (вкладка «Приём Акрона (ЭС)», просто
//     адрес для эмуляции прибора, без сопоставления точек вообще) —
//     теперь один общий механизм и одна вкладка для обоих типов
//     приборов, различие только в числе строк таблицы (4 у ВКМ, 1 у
//     Akron).
//  3. Подключение к ЭС — форма подключения к SQL Server + «Проверить
//     подключение» (POST /api/es-connection/test, ничего не сохраняет).
//  5. Последний опрос (раньше называлась «Текущие данные» — переименовано
//     2026-08-29, оператор указал, что старое название вводило в
//     заблуждение, будто это живой поток с прибора) — та же таблица
//     только для чтения, что показывает дашборд на /, использует тот же
//     GET /api/current: значения из последнего успешно завершённого
//     опроса, не мгновенный снимок.
func (s *Server) handleAdminUI(w http.ResponseWriter, r *http.Request) {
	// Запрет кеширования — постоянно, без условий: за один день эту
	// страницу правили много раз, и устаревшая закешированная копия
	// (браузер показывает СТАРЫЙ JS-код, а оператор думает, что тестирует
	// ПОСЛЕДНИЙ фикс) неотличима от настоящего бага без этого заголовка.
	// Ничего не стоит держать всегда включённым — страница маленькая,
	// живые данные и так читаются отдельными XHR-запросами, реальной
	// потери в скорости от отказа от кеша самой HTML-оболочки нет.
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(adminUIHTML))
}

const adminUIHTML = `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>mbgw — Управление приборами</title>
<style>
body {
  font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
  padding: 0;
  margin: 0;
  background: #1e1e1e;
  color: #cccccc;
}
.topbar { background: #252526; padding: 15px 30px; border-bottom: 1px solid #3e3e42; }
.topbar h2 { margin: 0; color: #ffffff; font-weight: 500; }
.tabs { display: block; padding: 0 30px; background: #252526; border-bottom: 1px solid #3e3e42; }
.tab-btn {
  display: inline-block; padding: 12px 18px; margin-right: 4px; cursor: pointer;
  color: #969696; border: none; background: none; font-size: 14px;
  border-bottom: 2px solid transparent;
}
.tab-btn.active { color: #ffffff; border-bottom: 2px solid #0e639c; }
.content { padding: 30px; max-width: 1100px; }
.panel { display: none; }
.panel.active { display: block; }
h3 { color: #ffffff; font-weight: 500; margin-top: 0; }
table { border-collapse: collapse; width: 100%; margin-top: 10px; background: #252526; }
th, td { border: 1px solid #3e3e42; padding: 10px; text-align: left; font-size: 13px; }
th { background: #333337; color: #ffffff; font-weight: 600; text-transform: uppercase; font-size: 12px; }
.form-row { margin-bottom: 12px; }
.form-row label { display: inline-block; width: 220px; color: #969696; font-size: 13px; vertical-align: top; }
.form-row input, .form-row select {
  background: #3c3c3c; border: 1px solid #3e3e42; color: #cccccc; padding: 6px 8px;
  font-size: 13px; width: 260px; border-radius: 2px;
}
.form-row input[type=checkbox] { width: auto; }
.btn {
  background: #0e639c; color: #fff; border: none; padding: 8px 16px; cursor: pointer;
  font-size: 13px; border-radius: 2px; margin-right: 8px;
}
.btn.secondary { background: #3c3c3c; }
.btn.danger { background: #a1260d; }
.btn:disabled { opacity: 0.5; cursor: default; }
.msg { margin-top: 10px; font-size: 13px; padding: 8px; border-radius: 2px; display: none; }
.msg.ok { display: block; background: #1e3d1e; color: #4caf50; }
.msg.err { display: block; background: #3d1e1e; color: #f44336; }
.section { background: #252526; padding: 20px; border-radius: 4px; margin-bottom: 20px; border: 1px solid #3e3e42; }
.small-note { color: #858585; font-size: 12px; margin-top: 4px; }
.status-good { color: #4caf50; font-weight: bold; }
.status-bad { color: #f44336; font-weight: bold; }
.log-warn { color: #ffb300; font-weight: bold; }
.poll-dot { display:inline-block; width:16px; height:16px; border-radius:50%; background:#666; border:2px solid #444; vertical-align:middle; }
.poll-dot.active { background:#4caf50; border-color:#7bd17f; box-shadow:0 0 8px rgba(76,175,80,0.8); }
.play-btn { width:34px; height:30px; padding:0; font-size:17px; line-height:30px; text-align:center; }
.manual-poll-overlay { display:none; position:fixed; z-index:10000; left:0; top:0; right:0; bottom:0; background:rgba(0,0,0,0.68); }
.manual-poll-box { width:620px; max-width:90%; margin:90px auto 0 auto; background:#252526; border:1px solid #555; border-radius:4px; padding:18px; box-shadow:0 4px 24px rgba(0,0,0,0.6); }
.manual-poll-log { background:#111; border:1px solid #3e3e42; color:#d4d4d4; padding:12px; min-height:150px; max-height:330px; overflow:auto; white-space:pre-wrap; font-family:Consolas,monospace; font-size:12px; }
.log-filter-btn { cursor:pointer; border:2px solid transparent; }
.log-filter-btn.selected { border-color:#ffffff !important; opacity:1 !important; }
.service-log { width:100%; border-collapse:collapse; }
.service-log td { vertical-align:top; }
.service-level-info { color:#cccccc; }
.service-level-ok { color:#6fdc73; }
.service-level-warn { color:#ffbf47; font-weight:bold; }
.service-level-error { color:#ff6b6b; font-weight:bold; }
.service-level-critical { color:#ff3b30; font-weight:bold; background:#3d1e1e; }
.watchdog-box { padding:10px; margin:10px 0; border:1px solid #3e3e42; background:#1e1e1e; }
</style>
</head>
<body>

<div class="topbar"><h2>mbgw — Управление приборами</h2></div>
<div class="tabs">
  <button class="tab-btn active" onclick="showTab('dashboard')">Главная</button>
  <button class="tab-btn" onclick="showTab('devices')">Приборы</button>
  <button class="tab-btn" onclick="showTab('channels')">Точки ЭС</button>
  <button class="tab-btn" onclick="showTab('esconn')">Подключение к ЭС</button>
  <button class="tab-btn" onclick="showTab('settings')">Настройки</button>
  <button class="tab-btn" onclick="showTab('archive')">Архив</button>
  <button class="tab-btn" onclick="showTab('pollmonitor')">Монитор опроса</button>
  <button class="tab-btn" onclick="showTab('log')">Лог</button>
  <button class="tab-btn" onclick="showTab('service')">Служба</button>
</div>

<div class="content">

  <!-- ===================== ПРИБОРЫ ===================== -->
  <!-- ===================== ГЛАВНАЯ (ДАШБОРД) ===================== -->
  <div id="panel-dashboard" class="panel active">
    <div class="section">
      <h3>Статус приборов</h3>
      <p class="small-note">Кнопка «Принудительная пересинхронизация с ЭС» — когда нужно обновить данные в БД ЭС.</p>
      <div id="dashboardSystemHealth" class="small-note" style="margin:8px 0 12px 0;">Состояние шлюза: проверка...</div>
      <table>
        <thead><tr>
          <th style="cursor:pointer;" onclick="sortDashboard('name')">Прибор ⇅</th>
          <th style="cursor:pointer;" onclick="sortDashboard('kind')">Тип ⇅</th>
          <th>Включён</th>
          <th style="cursor:pointer;" onclick="sortDashboard('lag')">Отставание архива ⇅</th>
          <th style="cursor:pointer;" onclick="sortDashboard('drift')">Расхождение времени ⇅</th>
          <th>Последняя успешная работа</th>
          <th>Действие</th>
        </tr></thead>
        <tbody id="dashboardTable"><tr><td colspan="7">Загрузка...</td></tr></tbody>
      </table>

      <!-- Всплывающий блок «Принудительная пересинхронизация с ЭС» —
           переехал сюда с вкладки «Подключение к ЭС» (2026-09-02,
           прямой запрос оператора) вместе с кнопкой «Синхронизировать
           сейчас», которую заменил в таблице выше: раз есть более
           мощный механизм (явно перезаписывает уже отправленное), не
           нужно два разных действия рядом — один явный путь понятнее.
           Скрыт по умолчанию, появляется при нажатии кнопки в строке
           конкретного прибора (см. openDashboardResyncBox). -->
      <div id="dashboardResyncBox" style="display:none;margin-top:20px;padding:15px;border:1px solid #3e3e42;border-radius:4px;background:#252526;">
        <h3 style="margin-top:0;">Принудительная пересинхронизация с ЭС — <span id="dashboardResyncDeviceName"></span></h3>
        <p class="small-note">Перезаписывает уже отправленные в ЭС точки за указанный период свежими значениями (по ТЕКУЩИМ настройкам множителя точки) — обычная фоновая синхронизация только добавляет новое и никогда не трогает то, что уже есть в ЭС. Нужно, например: с прибора один раз пришли искажённые данные, вы их переопросили и получили верные — но в ЭС уже успело уйти старое; либо поменяли множитель точки (вкладка «Точки ЭС») — новые точки и так пойдут в правильных единицах, а вот уже отправленная история сама не пересчитается, пока её явно не переписать.</p>
        <div class="form-row"><label>С какой даты</label>
          <input id="dr_from" type="text" style="width:120px;" placeholder="ГГГГ-ММ-ДД">
          <select id="dr_from_h" style="width:55px;"></select>:<select id="dr_from_m" style="width:55px;"><option value="00">00</option><option value="30">30</option></select>
        </div>
        <div class="form-row"><label>По какую дату</label>
          <input id="dr_to" type="text" style="width:120px;" placeholder="ГГГГ-ММ-ДД (пусто = сейчас)">
          <select id="dr_to_h" style="width:55px;"></select>:<select id="dr_to_m" style="width:55px;"><option value="00">00</option><option value="30">30</option></select>
        </div>
        <p>
          <button class="btn secondary" onclick="doDashboardResync()">Проверить и переписать в ЭС</button>
          <button class="btn secondary" onclick="closeDashboardResyncBox()">Отмена</button>
        </p>
        <div id="dashboardResyncMsg" class="msg"></div>
      </div>
    </div>
  </div>

  <div id="panel-devices" class="panel">
    <div class="section">
      <h3>Список приборов</h3>
      <table>
        <thead><tr><th>ID</th><th>Название</th><th>Тип</th><th>Транспорт</th><th>Включён</th><th></th></tr></thead>
        <tbody id="devicesTable"><tr><td colspan="6">Загрузка...</td></tr></tbody>
      </table>
    </div>

    <div class="section">
      <h3 id="deviceFormTitle">Добавить прибор</h3>
      <p id="deviceFormOpenRow"><button class="btn" onclick="openNewDeviceForm()">Добавить прибор</button></p>
      <div id="deviceFormBody" style="display:none;">
      <div id="editWarning" style="display:none;background:#4d3800;border:1px solid #8a6d00;color:#ffd479;padding:10px;border-radius:4px;margin-bottom:15px;">
        Вы редактируете СУЩЕСТВУЮЩИЙ прибор «<span id="editWarningName"></span>» (ID: <span id="editWarningID"></span>).
        Сохранение изменит именно этот прибор, а не создаст новый.
        <button class="btn secondary" style="margin-left:10px;" onclick="resetDeviceForm()">Отменить и создать новый</button>
      </div>
      <div class="form-row"><label>Название</label><input id="d_name" type="text" onkeyup="autoFillID()"></div>
      <p class="small-note" style="margin-left:220px;margin-top:-8px;">ID: <span id="d_id_display">—</span> <input id="d_id" type="text" style="display:none;"></p>
      <div class="form-row"><label>Тип прибора</label>
        <select id="d_kind" onchange="onKindChange()">
          <option value="vkm360">ВКМ-360</option>
          <option value="akron">Акрон</option>
          <option value="ivk-ter">ИВК-ТЭР</option>
        </select>
      </div>
      <div class="form-row"><label>Путь к профилю</label>
        <select id="d_profile"></select>
      </div>
      <div class="form-row"><label>Тип связи</label>
        <select id="d_transport_kind" onchange="onTransportKindChange()">
          <option value="modbus_tcp">Modbus TCP</option>
          <option value="rtu_serial">Последовательный порт (Modbus RTU)</option>
          <option value="tcp_serial">TCP-последовательный (конвертер)</option>
        </select>
      </div>
      <div id="tcpFields">
        <div class="form-row"><label>IP-адрес</label><input id="d_host" type="text" placeholder="10.48.228.126"></div>
        <div class="form-row"><label>Порт</label><input id="d_port" type="text" value="502"></div>
      </div>
      <div id="serialFields">
        <div class="form-row"><label>Номер COM-порта</label><input id="d_com" type="number" min="1" max="255" step="1" placeholder="1"></div>
        <div class="form-row"><label>Скорость (бод)</label><input id="d_baudrate" type="text" value="9600"></div>
        <div class="form-row"><label>Чётность</label>
          <select id="d_parity"><option value="none">нет</option><option value="even">чётная</option><option value="odd">нечётная</option></select>
        </div>
        <div class="form-row"><label>Стоп-биты</label><input id="d_stopbits" type="text" value="1"></div>
      </div>
      <div class="form-row"><label>Адрес прибора на линии</label><input id="d_unit_id" type="text" value="1"></div>
      <div class="form-row"><label>Таймаут (мс)</label><input id="d_timeout_ms" type="text" value="1000"></div>
      <div class="form-row"><label>Количество повторов при ошибке</label><input id="d_retries" type="number" min="1" step="1" value="3"></div>
      <p class="small-note" style="margin-left:220px;margin-top:-8px;">По умолчанию: 3. При ошибке шлюз повторяет запрос с паузой. Значение 0 не используется: сервер всё равно применит 3.</p>
      <div class="form-row"><label>Включён</label><input id="d_enabled" type="checkbox" checked></div>

      <p><a href="#" onclick="toggleAdvanced(); return false;" style="color:#0e639c;font-size:13px;" id="advancedToggle">▸ Дополнительные настройки</a></p>
      <div id="advancedFields" style="display:none;">
        <div class="form-row"><label>Глубина восстановления архива при старте (часов)</label><input id="d_backfill_max_depth_hours" type="number" min="0" step="1" value="0"></div>
        <p class="small-note">По умолчанию: 0. Это означает — проверять максимально доступную глубину архива, указанную профилем данного прибора.</p>
        <div class="form-row"><label>Опрос каждые N периодов</label><input id="d_archive_every_periods" type="number" min="1" step="1" value="1"></div>
        <p class="small-note">По умолчанию: 1 — опрашивать каждый архивный период. 2 — каждый второй и т.д. Длительность периода задаётся профилем прибора.</p>
        <div class="form-row"><label>Дни опроса</label><div style="flex:1">
          <label><input class="d_sched_day" type="checkbox" value="1" checked> Пн</label>
          <label><input class="d_sched_day" type="checkbox" value="2" checked> Вт</label>
          <label><input class="d_sched_day" type="checkbox" value="4" checked> Ср</label>
          <label><input class="d_sched_day" type="checkbox" value="8" checked> Чт</label>
          <label><input class="d_sched_day" type="checkbox" value="16" checked> Пт</label>
          <label><input class="d_sched_day" type="checkbox" value="32" checked> Сб</label>
          <label><input class="d_sched_day" type="checkbox" value="64" checked> Вс</label>
        </div></div>
        <p class="small-note">По умолчанию выбраны все дни недели.</p>
        <div class="form-row"><label>Разрешённое время суток</label><div style="flex:1"><input id="d_archive_window_start" type="time"> — <input id="d_archive_window_end" type="time"></div></div>
        <p class="small-note">По умолчанию оба поля пустые — опрос разрешён круглосуточно. Интервал может переходить через полночь.</p>
        <div class="form-row"><label>Сдвиг от границы периода (мин)</label><input id="d_archive_at_minute" type="number" min="0" max="59" step="1" value="5"></div>
        <p class="small-note">Через сколько минут после границы архивного периода начинать штатный опрос. По умолчанию: +5 минут. Следующее фактическое время показывается в «Мониторе опроса».</p>
        <p class="small-note" style="color:#ffd479;">Время прибора проверяется автоматически при опросе, если его профиль поддерживает чтение часов. Возможность автоматической коррекции зависит от типа прибора.</p>

        <div id="vkmTimeCorrectionFields" style="display:none;margin-top:18px;padding-top:12px;border-top:1px solid #3e3e42;">
          <p style="margin-top:0;color:#ffffff;font-size:13px;"><b>Коррекция времени ВКМ-360</b></p>
          <div class="form-row"><label>Допустимое расхождение времени, сек</label><input id="d_time_correction_deadband_seconds" type="number" min="0" step="1" value="0"></div>
          <p class="small-note" style="margin-left:220px;margin-top:-8px;">По умолчанию: 0. Если автокоррекция включена, 0 означает корректировать любое ненулевое расхождение; значение больше 0 задаёт порог, внутри которого часы не изменяются.</p>
          <div class="form-row"><label>Макс. коррекция за один раз, сек</label><input id="d_time_correction_max_step_seconds" type="number" min="0" max="99" step="1" value="0"></div>
          <p class="small-note" style="margin-left:220px;margin-top:-8px;">По умолчанию: 0 — автокоррекция выключена. Для ВКМ допустимо 1–99 сек за одну коррекцию.</p>
          <div class="form-row"><label>Макс. коррекция за 24 часа, сек</label><input id="d_time_correction_daily_limit_seconds" type="number" min="0" step="1" value="0"></div>
          <p class="small-note" style="margin-left:220px;margin-top:-8px;">По умолчанию: 0 — автокоррекция выключена. При ненулевом значении ограничивается суммарная величина коррекций за последние 24 часа.</p>
          <p class="small-note" style="color:#ffd479;">Для включения автокоррекции ВКМ задайте оба ограничения выше 0. Проверка расхождения времени работает и при выключенной автокоррекции.</p>
        </div>
      </div>

      <p>
        <button class="btn secondary" onclick="probeDevice()">Проверить прибор</button>
        <button class="btn" onclick="saveDevice()">Сохранить</button>
        <button class="btn secondary" onclick="resetDeviceForm()">Очистить форму</button>
      </p>
      <div id="probeMsg" class="msg"></div>
      <div id="deviceMsg" class="msg"></div>
      </div>
    </div>
  </div>

  <!-- ===================== ТОЧКИ ЭС ===================== -->
  <div id="panel-channels" class="panel">
    <div class="section">
      <h3>Точки ЭС</h3>
      <p class="small-note">Каждая величина прибора сопоставляется с конкретной точкой ЭС (полем ID_PP в таблице PointMains). Для ВКМ-360 можно включить до 10 трубопроводов; у каждого — четыре независимых слота ID_PP с выбором параметра из реально прочитанной архивной строки.</p>
      <div class="form-row"><label>Прибор</label>
        <select id="ch_device" onchange="loadChannels()"></select>
      </div>
      <div id="channelsLegacyWrap">
        <table>
          <thead><tr><th>Величина</th><th>ID_PP</th><th>Множитель</th></tr></thead>
          <tbody id="channelsTableBody"><tr><td colspan="3">Выберите прибор</td></tr></tbody>
        </table>
      </div>
      <div id="channelsVKMWrap" style="display:none;">
        <p class="small-note">Флажок «Опрос» задаёт трубопроводы, которые шлюз реально читает. «Пересканировать трубопроводы» физически проверяет архивные экземпляры 1–10, но сам флажки не меняет. Пустой архивный период не считается отсутствием трубопровода: шлюз проверяет соседний завершённый период. Параметры в выпадающих списках берутся из последней архивной строки, а для ещё не опрашиваемой найденной трубы — из результата сканирования.</p>
        <p><button class="btn secondary" type="button" onclick="rescanVKMPipes()">Пересканировать трубопроводы</button> <button class="btn secondary" type="button" onclick="refreshVKMSourceTags()">Обновить параметры из архива</button></p>
        <div id="vkmPipeGroups"></div>
      </div>
      <p><button class="btn" onclick="saveChannels()">Сохранить точки</button></p>
      <div id="channelsMsg" class="msg"></div>
    </div>
  </div>

  <!-- ===================== ПОДКЛЮЧЕНИЕ К ЭС ===================== -->
  <div id="panel-esconn" class="panel">
    <div class="section">
      <h3>Подключение к БД Энергосферы (SQL Server)</h3>
      <div id="es_status_configured" style="display:none;background:#1e3d1e;border:1px solid #2d5a2d;color:#4caf50;padding:10px;border-radius:4px;margin-bottom:15px;">
        Подключение настроено (сервер: <span id="es_status_server"></span>, база: <span id="es_status_db"></span>).
      </div>
      <div id="es_status_not_configured" style="display:none;background:#3d1e1e;border:1px solid #5a2d2d;color:#f44336;padding:10px;border-radius:4px;margin-bottom:15px;">
        Подключение ещё НЕ настроено — заполните поля ниже и нажмите «Сохранить».
      </div>
      <div class="form-row"><label>Сервер</label><input id="es_server" type="text"></div>
      <p class="small-note" style="margin-left:220px;margin-top:-8px;">например: localhost</p>
      <div class="form-row"><label>База данных</label>
        <select id="es_database_select" style="display:none;"></select>
        <input id="es_database" type="text">
      </div>
      <div class="form-row"><label>Логин</label><input id="es_user" type="text"></div>
      <div class="form-row"><label>Пароль</label><input id="es_password" type="password"></div>
      <div class="form-row"><label>Порт</label><input id="es_port" type="text" value="1433"></div>
      <div class="form-row"><label>Сдвиг времени при записи в ЭС (минут)</label><input id="es_time_shift" type="text" value="0"></div>
      <p class="small-note" style="margin-left:220px;margin-top:-8px;color:#ffd479;">Отрицательное значение сдвигает метку назад; например, -90 — на 90 минут назад. 0 — без сдвига.</p>
      <p class="small-note" id="es_password_note"></p>
      <p>
        <button class="btn secondary" onclick="testESConnection()">Проверить подключение</button>
        <button class="btn" onclick="saveESConnection()">Сохранить</button>
      </p>
      <div id="esTestMsg" class="msg"></div>
      <div id="esSaveMsg" class="msg"></div>
      <!-- Кнопка «Синхронизировать сейчас» переехала на вкладку «Главная»
           (2026-08-29, прямой запрос оператора) — там она стоит рядом с
           каждым прибором в общей таблице статуса, а не отдельно здесь,
           вдалеке от общего обзора приборов.

           «Принудительная пересинхронизация с ЭС» тоже переехала на
           «Главную» (2026-09-02, прямой запрос оператора) — тем более
           логично, что и «Синхронизировать сейчас» уехала туда же раньше:
           оба действия относятся к конкретному прибору из общей таблицы
           статуса, здесь им было не место. -->
    </div>
  </div>

  <!-- ===================== МОНИТОР ОПРОСА ===================== -->
  <div id="panel-pollmonitor" class="panel">
    <div class="section">
      <h3>Монитор опроса</h3>
      <p class="small-note">Зелёный индикатор означает, что прямо сейчас выполняется опрос этого прибора. Серый — прибор в данный момент не опрашивается.</p>
      <table>
        <thead><tr>
          <th>Статус</th>
          <th>Имя прибора</th>
          <th>Начало опроса</th>
          <th>Следующий опрос</th>
          <th>Статус последнего опроса</th>
          <th>Время прибора</th>
          <th>Действие</th>
        </tr></thead>
        <tbody id="pollMonitorTable"><tr><td colspan="7">Загрузка...</td></tr></tbody>
      </table>
    </div>
  </div>

  <div id="manualPollOverlay" class="manual-poll-overlay">
    <div class="manual-poll-box">
      <h3 style="margin-top:0;">Ручной опрос — <span id="manualPollDeviceName"></span></h3>
      <p class="small-note">Ручной запрос использует тот же канал и очередь, что и автоматический опрос. Поэтому два обмена с одним прибором одновременно не выполняются.</p>
      <pre id="manualPollLog" class="manual-poll-log"></pre>
      <p><button class="btn secondary" id="manualPollCloseBtn" onclick="closeManualPollPopup()">Закрыть</button></p>
    </div>
  </div>

  <!-- ===================== ТЕКУЩИЕ ДАННЫЕ ===================== -->
  <div id="panel-current" class="panel">
    <div class="section">
      <h3>Последний опрос</h3>
      <p class="small-note">Это НЕ живой поток с прибора — таблица показывает значения из последнего успешно завершённого опроса (когда именно — смотрите столбец «Время» у каждой строки; для точек с редким опросом может быть значительно старше текущего момента).</p>
      <div class="form-row"><label>Прибор</label>
        <select id="cur_device" onchange="loadCurrentData()">
          <option value="">— все приборы —</option>
        </select>
      </div>
      <table>
        <thead><tr><th>Прибор</th><th>Параметр</th><th>№</th><th>Значение</th><th>Ед.изм.</th><th>Статус</th><th>Время</th></tr></thead>
        <tbody id="currentData"><tr><td colspan="7">Загрузка...</td></tr></tbody>
      </table>
    </div>
  </div>

  <!-- ===================== ЛОГ ===================== -->
  <div id="panel-log" class="panel">
    <div class="section">
      <h3>Лог сервера</h3>
      <div id="deviceActivityStrip" style="display:flex;flex-wrap:wrap;gap:8px;margin-bottom:12px;"></div>
      <div style="margin-bottom:12px;">
        <label style="color:#969696;font-size:13px;margin-right:8px;">События</label>
        <select id="logEventFilter" onchange="setLogEventFilter(this.value)" style="background:#3c3c3c;border:1px solid #3e3e42;color:#cccccc;padding:6px 8px;">
          <option value="all">Все события</option>
          <option value="error">Ошибки</option>
          <option value="poll">Опрос прибора</option>
          <option value="archive">Архив и восстановление</option>
          <option value="time">Коррекция времени</option>
          <option value="es">Запись в БД ЭС</option>
          <option value="system">Система</option>
        </select>
      </div>
      <p>
        <button class="btn secondary" id="logPauseBtn" onclick="toggleLogPause()">Пауза</button>
        <button class="btn secondary" onclick="copyLog()">Скопировать всё</button>
        <button class="btn secondary" onclick="downloadLog()">Сохранить в файл</button>
        <button class="btn secondary" onclick="clearLogView()">Очистить экран (сам лог на сервере не трогает)</button>
      </p>
      <div id="logMsg" class="msg"></div>
      <pre id="logView" style="background:#0c0c0c;color:#cccccc;padding:12px;height:520px;overflow-y:scroll;font-family:Consolas,'Courier New',monospace;font-size:12px;white-space:pre-wrap;word-break:break-all;border:1px solid #3e3e42;"></pre>
    </div>
  </div>

  <!-- ===================== СЛУЖБА ===================== -->
  <div id="panel-service" class="panel">
    <div class="section">
      <h3>Служба</h3>
      <div id="serviceContent">Загрузка...</div>
      <div id="serviceMsg" class="msg"></div>
      <div style="margin-top:18px;">
        <h3>Журнал службы</h3>
        <div style="margin-bottom:10px;">
          <label style="margin-right:8px;">Показать</label>
          <select id="serviceLogFilter" onchange="loadServiceLog()">
            <option value="all">Все события</option>
            <option value="lifecycle">Запуск и остановка</option>
            <option value="watchdog">Контроль зависания</option>
            <option value="problems">Предупреждения и ошибки</option>
          </select>
        </div>
        <table class="service-log">
          <thead><tr><th>Время</th><th>Уровень</th><th>Событие</th><th>Сообщение</th></tr></thead>
          <tbody id="serviceLogTable"><tr><td colspan="4">Загрузка...</td></tr></tbody>
        </table>
      </div>
    </div>
  </div>

  <!-- ===================== АРХИВ ===================== -->
  <div id="panel-archive" class="panel">
    <div class="section">
      <h3>Архив по прибору</h3>
      <div class="form-row"><label>Прибор</label>
        <select id="ar_device" onchange="onArchiveDeviceChange()"></select>
      </div>
      <div class="form-row" id="ar_pipe_row" style="display:none;"><label>Трубопровод</label>
        <select id="ar_pipe"></select>
      </div>
      <div class="form-row"><label>Период</label>
        <input id="ar_from" type="text" style="width:120px;" placeholder="ГГГГ-ММ-ДД">
        <select id="ar_from_h" style="width:55px;"></select>:<select id="ar_from_m" style="width:55px;"></select>
        &nbsp;—&nbsp;
        <input id="ar_to" type="text" style="width:120px;" placeholder="ГГГГ-ММ-ДД">
        <select id="ar_to_h" style="width:55px;"></select>:<select id="ar_to_m" style="width:55px;"></select>
      </div>
      <div class="form-row"><label></label>
        <button class="btn secondary" onclick="setArchivePreset('today')">Сегодня</button>
        <button class="btn secondary" onclick="setArchivePreset('week')">Неделя</button>
        <button class="btn secondary" onclick="setArchivePreset('month')">Месяц</button>
      </div>
      <div class="form-row"><label>Группировка</label>
        <select id="ar_granularity">
          <option value="raw">Подробно (как хранится)</option>
          <option value="hourly">По часам (сумма за час)</option>
          <option value="daily">По суткам (сумма за день)</option>
          <option value="monthly">По месяцам (сумма за месяц)</option>
        </select>
      </div>
      <p>
        <button class="btn" onclick="loadArchiveTable()">Показать</button>
        <button class="btn secondary" onclick="exportArchiveCSV()">Выгрузить в Excel (CSV)</button>
      </p>
      <div id="archiveMsg" class="msg"></div>
      <table>
        <thead id="archiveTableHead"><tr><th>Период</th></tr></thead>
        <tbody id="archiveTableBody"><tr><td>Выберите прибор и период, затем нажмите «Показать»</td></tr></tbody>
      </table>
    </div>

    <div class="section" id="reloadSection" style="display:none;">
      <h3>Принудительный переопрос</h3>
      <p class="small-note">Немедленно переопрашивает прибор за указанный период и перезаписывает данные в БД МодбасШлюза. В БД ЭС эта операция напрямую ничего не записывает. Чтобы затем обновить уже отправленные данные в ЭС, используйте кнопку «Принудительная пересинхронизация с ЭС» на вкладке «Главная».</p>
      <div class="form-row"><label>Переопросить с</label>
        <input id="rl_from" type="text" style="width:120px;" placeholder="ГГГГ-ММ-ДД">
        <select id="rl_from_h" style="width:55px;"></select>:<select id="rl_from_m" style="width:55px;"><option value="00">00</option><option value="30">30</option></select>
      </div>
      <div class="form-row" id="rl_to_row"><label>Переопросить по</label>
        <input id="rl_to" type="text" style="width:120px;" placeholder="ГГГГ-ММ-ДД">
        <select id="rl_to_h" style="width:55px;"></select>:<select id="rl_to_m" style="width:55px;"><option value="00">00</option><option value="30">30</option></select>
      </div>
      <p>
        <button class="btn danger" onclick="forceReload()">Переопросить принудительно</button>
        <button class="btn secondary" id="reloadCancelBtn" style="display:none;" onclick="cancelReload()">Отменить</button>
      </p>
      <div id="reloadMsg" class="msg"></div>
    </div>
  </div>

  <!-- ===================== НАСТРОЙКИ ===================== -->
  <div id="panel-settings" class="panel">
    <div class="section">
      <h3>Настройки</h3>
      <div class="form-row"><label>Порт веб-интерфейса</label><input id="s_port" type="text" placeholder="8080"></div>
      <div id="s_port_conflict" style="display:none;background:#4d3800;border:1px solid #8a6d00;color:#ffd479;padding:10px;border-radius:4px;margin-bottom:15px;">
        Настроенный порт занят чем-то другим на сервере — сейчас реально работаете на порту <b id="s_port_actual"></b>.
        Если порт <span id="s_port_configured_repeat"></span> занят постоянно, есть смысл сделать рабочий порт основным:
        <button class="btn secondary" style="margin-left:10px;" onclick="adoptActualPort()">Использовать <span id="s_port_actual2"></span> как основной</button>
      </div>
      <p class="small-note" id="s_port_note"></p>
      <div class="form-row"><label>Отладочный лог (подробные байты)</label><input id="s_debug" type="checkbox"></div>
      <p class="small-note">Включает подробный вывод сырых байт обмена с приборами в лог-файл — полезно при диагностике, но создаёт много лишних записей при обычной работе.</p>
      <div class="form-row"><label>Таймаут контроля зависания (мин)</label><input id="s_watchdog_timeout" type="text" value="10"></div>
      <p class="small-note">Допустимо 2–120 минут. Ошибки связи с приборами не считаются зависанием. При запуске службой подтверждённое глобальное зависание приводит к аварийному перезапуску через Windows SCM; при ручном запуске процесс не завершается автоматически.</p>
      <p><button class="btn" onclick="saveSettings()">Сохранить</button></p>
      <div id="settingsMsg" class="msg"></div>
    </div>
  </div>

</div>

<script>
var allDevices = [];
var allProfiles = [];
var editingOriginalKind = null; // set by editDevice(), cleared by resetDeviceForm() — used to warn if the operator changes "Тип прибора" while editing an EXISTING device (root cause of the 2026-08-23 incident: switching kind mid-edit silently repurposed one device's saved row into a different device).
var channelSafetyByTag = {}; // non-VKM legacy rows keep backend min/max even though that compact table does not expose them.
var vkmChannelBySlot = {};   // key "pipe:slot" -> saved mapping row
var vkmSourceTagsByPipe = {}; // pipe -> tags parsed from the newest raw archive row
var vkmPipeDiscoveryByPipe = {}; // pipe -> last persisted physical discovery result

function loadProfiles() {
  var xhr = new XMLHttpRequest();
  xhr.open('GET', '/api/profiles', true);
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4 || xhr.status !== 200) { return; }
    allProfiles = JSON.parse(xhr.responseText) || [];
    var sel = document.getElementById('d_profile');
    var current = sel.value;
    var html = '';
    for (var i = 0; i < allProfiles.length; i++) {
      html += '<option value="' + allProfiles[i] + '">' + allProfiles[i] + '</option>';
    }
    if (html === '') { html = '<option value="">— папка profiles/ пуста или не найдена —</option>'; }
    sel.innerHTML = html;
    if (current) {
      sel.value = current; // сохранить выбор при повторной загрузке
    } else {
      // При первом открытии список профилей приходит асинхронно. Не даём
      // браузеру молча выбрать первый файл по алфавиту, который может не
      // соответствовать выбранному типу прибора.
      var kind = document.getElementById('d_kind').value;
      if (kind === 'vkm360') { sel.value = 'profiles/vkm360.yaml'; }
      if (kind === 'akron') { sel.value = 'profiles/acron-01.yaml'; }
      if (kind === 'ivk-ter') { sel.value = 'profiles/ivk-ter.yaml'; }
    }
  };
  xhr.send();
}

function showTab(name) {
  var panels = document.getElementsByClassName('panel');
  for (var i = 0; i < panels.length; i++) { panels[i].className = 'panel'; }
  document.getElementById('panel-' + name).className = 'panel active';

  var btns = document.getElementsByClassName('tab-btn');
  for (var j = 0; j < btns.length; j++) { btns[j].className = 'tab-btn'; }
  event.target.className = 'tab-btn active';

  // Сброс ВСЕХ сообщений об успехе/ошибке по всему интерфейсу при
  // КАЖДОМ переключении вкладки (добавлено 2026-08-30, прямой запрос
  // оператора: "Синхронизировать сейчас" на Главной оставляло сообщение
  // висеть навсегда, даже после перехода на другие вкладки). Раньше
  // это лечилось точечно для отдельных элементов (deviceMsg/probeMsg
  // при смене прибора, счётчик поколений для статуса переопроса) — те
  // точечные фиксы остаются, но теперь ЕЩЁ и общий проход по всем
  // элементам с классом "msg" разом закрывает весь этот класс багов,
  // а не только конкретные места, где мы его уже ловили руками.
  var allMsgs = document.getElementsByClassName('msg');
  for (var m = 0; m < allMsgs.length; m++) { allMsgs[m].className = 'msg'; }

  if (name === 'current') { populateDeviceSelect('cur_device', null); loadCurrentData(); }
  if (name === 'channels') { populateDeviceSelect('ch_device', null); }
  if (name === 'esconn') { loadESConnection(); }
  if (name === 'settings') { loadSettings(); }
  if (name === 'archive') { populateDeviceSelect('ar_device', null); setArchivePreset('week'); }
  if (name === 'dashboard') { loadDashboard(); }
  if (name === 'pollmonitor') { loadPollMonitor(); }
  if (name === 'service') { loadServiceStatus(); }
  if (name === 'log') { renderDeviceActivityStrip(); }
}

function showMsg(elId, ok, text) {
  var el = document.getElementById(elId);
  el.className = 'msg ' + (ok ? 'ok' : 'err');
  el.innerText = text;
}

function onKindChange() {
  var kind = document.getElementById('d_kind').value;
  var idField = document.getElementById('d_id');
  // Подтверждение перед сменой типа УЖЕ СУЩЕСТВУЮЩЕГО прибора посреди
  // редактирования — именно это действие (смена «Тип прибора» во время
  // редактирования сохранённого прибора) однажды тихо превратило
  // сохранённый прибор Akron в прибор ВКМ с тем же ID, 2026-08-23. У
  // совсем нового прибора (d_id ещё не отключено) терять нечего,
  // подтверждение не нужно.
  if (idField.disabled && editingOriginalKind && kind !== editingOriginalKind) {
    var ok = confirm('Вы редактируете существующий прибор и меняете его тип с "' + deviceKindLabel(editingOriginalKind) +
      '" на "' + deviceKindLabel(kind) + '". Это изменит СУЩЕСТВУЮЩИЙ прибор, а не создаст новый. Продолжить?');
    if (!ok) {
      document.getElementById('d_kind').value = editingOriginalKind;
      return;
    }
  }
  var tk = document.getElementById('d_transport_kind');
  var profileEl = document.getElementById('d_profile');
  if (kind === 'vkm360') {
    tk.value = 'modbus_tcp';
    profileEl.value = 'profiles/vkm360.yaml';
  }
  if (kind === 'akron') {
    tk.value = 'rtu_serial';
    profileEl.value = 'profiles/acron-01.yaml';
  }
  if (kind === 'ivk-ter') {
    // ИВК-ТЭР часто подключается по RS-485; оператор при необходимости
    // может переключить тип связи на Modbus TCP или TCP-конвертер.
    tk.value = 'rtu_serial';
    profileEl.value = 'profiles/ivk-ter.yaml';
    if (!idField.disabled) { document.getElementById('d_baudrate').value = '4800'; }
  }
  onTransportKindChange();
  updateVKMTimeCorrectionVisibility();
}
function onTransportKindChange() {
  var tk = document.getElementById('d_transport_kind').value;
  var showTCP = (tk === 'modbus_tcp' || tk === 'tcp_serial');
  var showSerial = (tk === 'rtu_serial' || tk === 'tcp_serial');
  document.getElementById('tcpFields').style.display = showTCP ? 'block' : 'none';
  document.getElementById('serialFields').style.display = showSerial ? 'block' : 'none';
}

function updateVKMTimeCorrectionVisibility() {
  var el = document.getElementById('vkmTimeCorrectionFields');
  if (!el) { return; }
  el.style.display = document.getElementById('d_kind').value === 'vkm360' ? 'block' : 'none';
}

function toggleAdvanced() {
  var el = document.getElementById('advancedFields');
  var link = document.getElementById('advancedToggle');
  var showing = el.style.display !== 'none';
  el.style.display = showing ? 'none' : 'block';
  link.innerText = showing ? '▸ Дополнительные настройки' : '▾ Дополнительные настройки';
}

function transliterate(s) {
  var map = {
    'а':'a','б':'b','в':'v','г':'g','д':'d','е':'e','ё':'e','ж':'zh','з':'z','и':'i',
    'й':'y','к':'k','л':'l','м':'m','н':'n','о':'o','п':'p','р':'r','с':'s','т':'t',
    'у':'u','ф':'f','х':'h','ц':'ts','ч':'ch','ш':'sh','щ':'sch','ъ':'','ы':'y','ь':'',
    'э':'e','ю':'yu','я':'ya'
  };
  var out = '';
  s = s.toLowerCase();
  for (var i = 0; i < s.length; i++) {
    var c = s.charAt(i);
    out += (map[c] !== undefined) ? map[c] : c;
  }
  return out;
}

function slugify(s) {
  var t = transliterate(s);
  t = t.replace(/[^a-z0-9]+/g, '_').replace(/^_+|_+$/g, '');
  return t;
}

// autoFillID держит скрытое поле d_id (то самое значение, что реально
// отправляется в API) синхронизированным с видимым полем «Название» —
// оператору никогда не нужно самому думать об ID или вводить его руками
// (см. другие комментарии в этом файле про случай 2026-08-23 с
// подсказкой-вместо-значения, который и подтолкнул максимально упростить
// эту форму). Автозаполнение работает только для НОВОГО прибора (d_id ещё
// не отключено); editDevice() отключает поле для уже существующих
// приборов, чей ID никогда не должен меняться после сохранения (именно
// на него ссылаются строки es_vkm_channels/es_akron_northbound).
function autoFillID() {
  var idField = document.getElementById('d_id');
  if (idField.disabled) { return; } // editing an existing device — ID is fixed
  var name = document.getElementById('d_name').value;
  var id = slugify(name);
  idField.value = id;
  document.getElementById('d_id_display').innerText = id || '—';
}

function intOrZero(v) { var n = parseInt(v, 10); return isNaN(n) ? 0 : n; }
function floatOrOne(v) { var n = parseFloat(v); return isNaN(n) ? 1.0 : n; }

// validateTransportFields проверяет ОДНО поле, у которого нет разумного
// значения по умолчанию и которое реально нужно ввести руками (COM-порт
// для последовательных транспортов, IP для TCP) — возвращает текст
// ошибки, или пустую строку, если всё в порядке. Появилось из-за
// реального случая (2026-08-23): поля с текстом-подсказкой (атрибут
// placeholder) НА ВИД выглядят заполненными на скриншоте, а на самом деле
// пустая строка, пока оператор реально не кликнет и не введёт что-то
// сам — отправка такого молча на сервер давала непонятную ошибку
// транспорта вместо чёткого «вы забыли заполнить это поле» прямо там, где
// была допущена ошибка. У всех ОСТАЛЬНЫХ полей формы теперь реальное
// значение по умолчанию (не просто подсказка), так что эта проверка
// нужна только для двух полей, для которых разумного значения по
// умолчанию в принципе не бывает.
function scheduleDaysMask() {
  var els = document.getElementsByClassName('d_sched_day');
  var mask = 0;
  for (var i = 0; i < els.length; i++) { if (els[i].checked) { mask += parseInt(els[i].value, 10); } }
  return mask;
}

function setScheduleDays(mask) {
  var els = document.getElementsByClassName('d_sched_day');
  for (var i = 0; i < els.length; i++) { els[i].checked = (mask & parseInt(els[i].value, 10)) !== 0; }
}

function validateScheduleFields(body) {
  if (body.archive_every_periods < 1) { return 'Поле "Опрос каждые N периодов" должно быть не меньше 1'; }
  if (body.archive_days_mask < 1) { return 'Выберите хотя бы один день опроса'; }
  if (body.archive_at_minute < 0 || body.archive_at_minute > 59) { return 'Сдвиг от границы периода должен быть от 0 до 59 минут'; }
  if ((body.archive_window_start && !body.archive_window_end) || (!body.archive_window_start && body.archive_window_end)) { return 'Для временного окна задайте и начало, и конец либо оставьте оба поля пустыми'; }
  return '';
}

function validateTransportFields(body) {
  if (body.transport_kind === 'modbus_tcp') {
    if (!body.host) { return 'Заполните поле "IP-адрес"'; }
  } else {
    if (!/^COM[1-9][0-9]{0,2}$/.test(body.com)) { return 'Введите только номер COM-порта, например 1 или 105'; }
  }
  return '';
}

function validateTimeCorrectionFields(body) {
  if (body.kind !== 'vkm360') { return ''; }
  if (body.time_correction_deadband_seconds < 0) {
    return 'Поле "Допустимое расхождение времени" не может быть отрицательным';
  }
  if (body.time_correction_max_step_seconds < 0 || body.time_correction_max_step_seconds > 99) {
    return 'Поле "Макс. коррекция за один раз" должно быть от 0 до 99 секунд';
  }
  if (body.time_correction_daily_limit_seconds < 0) {
    return 'Поле "Макс. коррекция за 24 часа" не может быть отрицательным';
  }
  return '';
}

function onArchiveDeviceChange() {
  var deviceId = document.getElementById('ar_device').value;
  var d = findDevice(deviceId);

  var pipeRow = document.getElementById('ar_pipe_row');
  var pipeSel = document.getElementById('ar_pipe');
  if (d && d.kind === 'vkm360') {
    var pipes = d.vkm_active_pipes && d.vkm_active_pipes.length ? d.vkm_active_pipes : [1];
    var pipeHtml = '';
    for (var pi = 0; pi < pipes.length; pi++) {
      pipeHtml += '<option value="' + pipes[pi] + '">Трубопровод ' + pipes[pi] + '</option>';
    }
    pipeSel.innerHTML = pipeHtml;
    pipeRow.style.display = 'block';
  } else {
    pipeSel.innerHTML = '';
    pipeRow.style.display = 'none';
  }

  var showReload = !!(d && (d.kind === 'akron' || d.kind === 'vkm360' || d.kind === 'ivk-ter'));
  document.getElementById('reloadSection').style.display = showReload ? 'block' : 'none';
  // Поле "по" показываем для ВКМ и ИВК-ТЭР. ВКМ адресуется по времени
  // напрямую, ИВК-ТЭР — по индексу, но при проходе функции 65 мы можем
  // остановиться по временной метке записи и тем самым честно ограничить
  // диапазон. У Akron переопрос остаётся "с указанной даты и до сейчас".
  var hasReloadTo = !!(d && (d.kind === 'vkm360' || d.kind === 'ivk-ter'));
  document.getElementById('rl_to_row').style.display = hasReloadTo ? 'block' : 'none';

  // Для ИВК-ТЭР пока показываем архив строго в исходной часовой
  // дискретности. Суммировать/усреднять служебные поля архива до
  // live-проверки их семантики на реальном приборе небезопасно.
  var gran = document.getElementById('ar_granularity');
  var isIVK = !!(d && d.kind === 'ivk-ter');
  if (isIVK) {
    gran.value = 'raw';
    gran.disabled = true;
    showMsg('archiveMsg', true, 'Для ИВК-ТЭР архив показывается подробно, без суточной и месячной агрегации.');
  } else {
    gran.disabled = false;
    document.getElementById('archiveMsg').innerHTML = '';
    document.getElementById('archiveMsg').className = 'msg';
  }

  // Смена прибора в списке — сбрасываем отображение прежнего переопроса
  // немедленно, НЕ дожидаясь ответа сети. Без этого старое сообщение
  // ("Идёт переопрос...") от ПРЕЖНЕГО прибора продолжало висеть на
  // экране до следующего тика фонового цикла (см. reloadPollGeneration
  // ниже) — на практике оператор просто не успевал заметить разницу и
  // думал, что переопрос идёт для только что выбранного прибора, хотя
  // на самом деле это был "хвост" от предыдущего (баг, найден оператором
  // 2026-08-29: переключился на другой прибор, а внизу всё ещё "идёт
  // переопрос", хотя выбран уже другой прибор).
  document.getElementById('reloadMsg').innerHTML = '';
  document.getElementById('reloadMsg').className = 'msg';
  document.getElementById('reloadCancelBtn').style.display = 'none';
  currentReloadDeviceId = null;

  // reloadPollGeneration — счётчик "поколений" фонового опроса прогресса.
  // При каждой смене прибора увеличиваем его; каждый уже запущенный цикл
  // pollReloadProgress сверяет СВОЁ поколение с текущим глобальным перед
  // тем, как показать сообщение или запланировать следующий тик — если
  // они разошлись, значит оператор уже переключился на другой прибор, и
  // цикл прежнего прибора молча самоуничтожается, не трогая экран
  // (добавлено 2026-08-29, тот же фикс).
  reloadPollGeneration++;

  // Проверяем, не идёт ли УЖЕ переопрос для этого прибора — важно после
  // обновления страницы (F5): сам переопрос на сервере продолжает
  // работать независимо от браузера, но обычное состояние JS-переменных
  // (currentReloadDeviceId и т.п.) при перезагрузке страницы стирается,
  // и без этой проверки оператор увидел бы пустую форму, как будто
  // ничего не происходит, хотя на сервере переопрос по-прежнему идёт
  // (добавлено 2026-08-27, прямой вопрос). Та же проверка теперь ЕЩЁ и
  // при обычной смене прибора без перезагрузки страницы — покажет
  // прогресс, если у НОВОГО выбранного прибора реально что-то идёт
  // в фоне (например, оператор запустил переопрос, ушёл на другую
  // вкладку, вернулся и выбрал именно этот прибор).
  if (showReload && deviceId) {
    checkExistingReload(deviceId);
  }
}

function checkExistingReload(deviceId) {
  // Запоминаем поколение, актуальное НА МОМЕНТ запуска этого запроса —
  // если к моменту прихода ответа оператор уже переключился на другой
  // прибор (глобальное поколение успело вырасти), ответ безопасно
  // игнорируем, не трогая экран текущего прибора.
  var myGeneration = reloadPollGeneration;
  var xhr = new XMLHttpRequest();
  xhr.open('GET', '/api/devices/reload-progress?device_id=' + encodeURIComponent(deviceId), true);
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4 || xhr.status !== 200) { return; }
    if (myGeneration !== reloadPollGeneration) { return; } // прибор уже сменили, пока ждали ответ
    var data;
    try { data = JSON.parse(xhr.responseText); } catch (e) { return; }
    if (data.found && !data.finished) {
      // переопрос уже идёт (запущен раньше, страница была обновлена или
      // открыта заново, либо оператор просто переключился обратно на
      // прибор, для которого уже что-то идёт в фоне) — сразу
      // возобновляем отображение прогресса, как будто мы его и
      // запускали
      currentReloadDeviceId = deviceId;
      document.getElementById('reloadCancelBtn').style.display = 'inline-block';
      pollReloadProgress(deviceId, myGeneration);
    }
  };
  xhr.send();
}

var currentReloadDeviceId = null; // для кнопки «Отменить» — какой прибор сейчас переопрашивается

// reloadPollGeneration растёт при каждой смене прибора в списке
// (см. onArchiveDeviceChange) — используется, чтобы фоновые циклы
// pollReloadProgress прежних приборов узнавали, что они больше не
// актуальны, и переставали и опрашивать сервер, и подменять собой
// статус-сообщение под текущим выбранным прибором (баг, найден
// оператором 2026-08-29).
var reloadPollGeneration = 0;

function forceReload() {
  var deviceId = document.getElementById('ar_device').value;
  if (!deviceId) { showMsg('reloadMsg', false, 'Выберите прибор'); return; }
  if (!normalizeCalendarInput(document.getElementById('rl_from')) ||
      !normalizeCalendarInput(document.getElementById('rl_to'))) {
    showMsg('reloadMsg', false, 'Проверьте формат даты');
    return;
  }
  var fromDate = document.getElementById('rl_from').value;
  var toDate = document.getElementById('rl_to').value;
  if (!fromDate) { showMsg('reloadMsg', false, 'Укажите дату начала'); return; }
  if (!confirm('Это ПЕРЕЗАПИШЕТ уже сохранённые данные архива за этот период данными, заново прочитанными с прибора. Продолжить?')) { return; }

  // Время внутри суток — отдельные выпадающие списки часа и получаса
  // (не текстовый ввод — оператору не нужно гадать формат; не HTML5
  // "time", потому что Internet Explorer его не поддерживает вообще).
  // По умолчанию оба поля 00:00 (первый вариант в списке часов).
  var fromH = document.getElementById('rl_from_h').value;
  var fromM = document.getElementById('rl_from_m').value;
  var fromVal = fromDate + 'T' + fromH + ':' + fromM;

  var toVal = '';
  if (toDate) {
    var toH = document.getElementById('rl_to_h').value;
    var toM = document.getElementById('rl_to_m').value;
    toVal = toDate + 'T' + toH + ':' + toM;
  }

  document.getElementById('reloadMsg').className = 'msg';
  var xhr = new XMLHttpRequest();
  xhr.open('POST', '/api/devices/reload-archive', true);
  xhr.setRequestHeader('Content-Type', 'application/json');
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4) { return; }
    var data;
    try { data = JSON.parse(xhr.responseText); } catch (e) { showMsg('reloadMsg', false, 'Ошибка ответа сервера'); return; }
    if (data.started) {
      showMsg('reloadMsg', true, 'Переопрос запущен, идёт сбор данных с прибора...');
      currentReloadDeviceId = deviceId;
      document.getElementById('reloadCancelBtn').style.display = 'inline-block';
      pollReloadProgress(deviceId, reloadPollGeneration);
    } else {
      showMsg('reloadMsg', false, 'Не удалось запустить переопрос');
    }
  };
  xhr.send(JSON.stringify({ device_id: deviceId, from: fromVal, to: toVal }));
}

// cancelReload прерывает уже запущенный переопрос — по прямому запросу
// оператора должна быть возможность остановить долгую операцию (тысяча с
// лишним периодов может идти больше часа), не дожидаясь конца, если что-то
// выглядит не так (добавлено 2026-08-27).
function cancelReload() {
  if (!currentReloadDeviceId) { return; }
  if (!confirm('Прервать переопрос? Уже собранные данные останутся, недостающие периоды нужно будет переопросить отдельно.')) { return; }
  var xhr = new XMLHttpRequest();
  xhr.open('POST', '/api/devices/reload-cancel', true);
  xhr.setRequestHeader('Content-Type', 'application/json');
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4) { return; }
    showMsg('reloadMsg', true, 'Отмена запрошена, завершение текущего периода...');
  };
  xhr.send(JSON.stringify({ device_id: currentReloadDeviceId }));
}

// pollReloadProgress опрашивает состояние фонового переопроса каждые 1.5
// секунды и обновляет текст под кнопкой — оператор видит реальный прогресс
// (обработано X из Y периодов) вместо полной тишины на много минут
// (добавлено 2026-08-27 по прямому запросу — раньше было непонятно,
// работает ли вообще что-то, или процесс завис).
// openDashboardResyncBox/closeDashboardResyncBox/doDashboardResync —
// «Принудительная пересинхронизация с ЭС», кнопка напротив прибора на
// вкладке «Главная» (2026-09-02, прямой запрос оператора: заменила
// собой прежнюю кнопку «Синхронизировать сейчас» и перенесённый сюда же
// со вкладки «Подключение к ЭС» механизм с диапазоном дат — раз есть
// более мощное действие (явно перезаписывает уже отправленное), два
// разных рядом только путали).
//
// Всплывающий блок #dashboardResyncBox скрыт по умолчанию — появляется
// при нажатии кнопки конкретного прибора и заполняется его именем;
// один общий блок на всю страницу, а не по одному на строку таблицы,
// проще и не раздувает разметку.
function openDashboardResyncBox(deviceId) {
  var deviceName = deviceId;
  for (var i = 0; i < allDevices.length; i++) {
    if (allDevices[i].id === deviceId) { deviceName = allDevices[i].name || deviceId; break; }
  }
  dashboardResyncDeviceId = deviceId;
  document.getElementById('dashboardResyncDeviceName').innerText = deviceName + ' (' + deviceId + ')';
  document.getElementById('dashboardResyncBox').style.display = 'block';
  document.getElementById('dashboardResyncMsg').className = 'msg';
  document.getElementById('dashboardResyncBox').scrollIntoView({ behavior: 'smooth', block: 'center' });
}

function closeDashboardResyncBox() {
  document.getElementById('dashboardResyncBox').style.display = 'none';
  dashboardResyncDeviceId = null;
}

// dashboardResyncDeviceId — какой прибор сейчас открыт во всплывающем
// блоке; общая переменная модуля, а не аргумент doDashboardResync,
// потому что блок один на страницу и заполняется при открытии.
var dashboardResyncDeviceId = null;

function doDashboardResync() {
  if (!dashboardResyncDeviceId) { return; }
  if (!normalizeCalendarInput(document.getElementById('dr_from')) ||
      !normalizeCalendarInput(document.getElementById('dr_to'))) {
    showMsg('dashboardResyncMsg', false, 'Проверьте формат даты');
    return;
  }

  var fromDate = document.getElementById('dr_from').value;
  var toDate = document.getElementById('dr_to').value;
  if (!fromDate) {
    showMsg('dashboardResyncMsg', false, 'Укажите дату начала');
    return;
  }

  var fromH = document.getElementById('dr_from_h').value;
  var fromM = document.getElementById('dr_from_m').value;
  var fromVal = fromDate + 'T' + fromH + ':' + fromM;

  var toVal = '';
  if (toDate) {
    var toH = document.getElementById('dr_to_h').value;
    var toM = document.getElementById('dr_to_m').value;
    toVal = toDate + 'T' + toH + ':' + toM;
  }

  document.getElementById('dashboardResyncMsg').className = 'msg';
  showMsg('dashboardResyncMsg', true, 'Проверяю диапазон. В ЭС пока ничего не записывается...');

  sendDashboardResyncRequest('preview', fromVal, toVal, function(preview) {
    if (!preview.ok) {
      showMsg('dashboardResyncMsg', false, 'Ошибка предпросмотра: ' + (preview.error || 'неизвестная'));
      return;
    }

    var summary =
      'Проверка завершена.\n\n' +
      'Будет перезаписано: ' + preview.updated + '\n' +
      'Будет вставлено новых: ' + preview.inserted + '\n' +
      'Заблокировано/ошибок: ' + preview.failed + '\n\n' +
      'Только после подтверждения начнётся реальная запись в ЭС.\n' +
      'Выполнить?';

    showMsg(
      'dashboardResyncMsg',
      true,
      'Предпросмотр: будет переписано ' + preview.updated +
      ', вставлено новых ' + preview.inserted +
      ', заблокировано/ошибок ' + preview.failed + '.'
    );

    if (!confirm(summary)) {
      showMsg('dashboardResyncMsg', true, 'Отменено после предпросмотра. В ЭС ничего не записано.');
      return;
    }

    showMsg('dashboardResyncMsg', true, 'Идёт реальная пересинхронизация с ЭС, подождите...');
    sendDashboardResyncRequest('execute', fromVal, toVal, function(result) {
      if (result.ok) {
        showMsg(
          'dashboardResyncMsg',
          true,
          'Готово: переписано ' + result.updated +
          ', вставлено новых ' + result.inserted +
          ', ошибок ' + result.failed + '.'
        );
      } else {
        showMsg('dashboardResyncMsg', false, 'Ошибка выполнения: ' + (result.error || 'неизвестная'));
      }
    });
  });
}

function sendDashboardResyncRequest(action, fromVal, toVal, done) {
  var xhr = new XMLHttpRequest();
  xhr.open('POST', '/api/es-sync/force-resync', true);
  xhr.setRequestHeader('Content-Type', 'application/json');
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4) { return; }
    var data;
    try {
      data = JSON.parse(xhr.responseText);
    } catch (e) {
      done({ ok: false, error: 'Ошибка ответа сервера' });
      return;
    }
    done(data);
  };
  xhr.send(JSON.stringify({
    device_id: dashboardResyncDeviceId,
    from: fromVal,
    to: toVal,
    action: action
  }));
}

// pollReloadProgress(deviceId, generation) — generation фиксируется
// вызывающей стороной (checkExistingReload / forceReload) в момент
// запуска ЭТОГО конкретного цикла отслеживания. Если к моменту прихода
// очередного ответа сервера оператор уже сменил прибор в списке
// (глобальный reloadPollGeneration успел вырасти) — цикл молча
// останавливается: не показывает сообщение (не затирает статус нового
// выбранного прибора) и не планирует следующий тик (баг, найден
// оператором 2026-08-29: сообщение "идёт переопрос" продолжало висеть
// после переключения на другой прибор). Сам фоновый переопрос на
// сервере при этом никак не останавливается — это только отключение
// ОТОБРАЖЕНИЯ в браузере, ровно как и задумано (переключение вкладок
// без выбора другого прибора прогресс не теряет).
function pollReloadProgress(deviceId, generation) {
  var xhr = new XMLHttpRequest();
  xhr.open('GET', '/api/devices/reload-progress?device_id=' + encodeURIComponent(deviceId), true);
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4 || xhr.status !== 200) { return; }
    if (generation !== reloadPollGeneration) { return; } // прибор сменили — этот цикл больше не актуален
    var data;
    try { data = JSON.parse(xhr.responseText); } catch (e) { return; }
    if (!data.found) { return; }

    if (!data.finished) {
      var pct = data.total > 0 ? Math.round(100 * data.done / data.total) : 0;
      showMsg('reloadMsg', true, 'Идёт переопрос: обработано ' + data.done + ' из ' + data.total + ' периодов (' + pct + '%). Данные перезаписываются в БД МодбасШлюза...');
      setTimeout(function() {
        if (generation !== reloadPollGeneration) { return; } // сменили прибор, пока ждали таймаут
        pollReloadProgress(deviceId, generation);
      }, 1500);
      return;
    }

    document.getElementById('reloadCancelBtn').style.display = 'none';
    currentReloadDeviceId = null;

    if (data.error) {
      showMsg('reloadMsg', false, 'Завершено с ошибкой: ' + data.error + ' (успело перезаписать записей: ' + data.saved + ')');
    } else {
      showMsg('reloadMsg', true, 'Готово, перезаписано записей в БД МодбасШлюза: ' + data.saved + '. Чтобы обновить ЭС, используйте «Принудительную пересинхронизацию с ЭС» на вкладке «Главная».');
    }
  };
  xhr.send();
}

function pad2(n) { return (n < 10 ? '0' : '') + n; }
function dateToInputValue(d) {
  return d.getFullYear() + '-' + pad2(d.getMonth() + 1) + '-' + pad2(d.getDate());
}

function setArchivePreset(preset) {
  var to = new Date();
  var from = new Date();
  if (preset === 'today') {
    // from остаётся = сегодня
  } else if (preset === 'week') {
    from.setDate(from.getDate() - 7);
  } else if (preset === 'month') {
    from.setMonth(from.getMonth() - 1);
  }
  document.getElementById('ar_from').value = dateToInputValue(from);
  document.getElementById('ar_to').value = dateToInputValue(to);
  // Пресеты по-прежнему означают полный календарный диапазон.
  // Время затем можно сузить вручную, например сегодня 12:00—15:00.
  document.getElementById('ar_from_h').value = '00';
  document.getElementById('ar_from_m').value = '00';
  document.getElementById('ar_to_h').value = '23';
  document.getElementById('ar_to_m').value = '59';
}

function archiveQueryString() {
  var deviceId = document.getElementById('ar_device').value;
  var fromDate = document.getElementById('ar_from').value;
  var toDate = document.getElementById('ar_to').value;
  var from = fromDate + 'T' + document.getElementById('ar_from_h').value + ':' + document.getElementById('ar_from_m').value;
  var to = toDate + 'T' + document.getElementById('ar_to_h').value + ':' + document.getElementById('ar_to_m').value;
  var granularity = document.getElementById('ar_granularity').value;
  var q = 'device_id=' + encodeURIComponent(deviceId) +
    '&from=' + encodeURIComponent(from) +
    '&to=' + encodeURIComponent(to) +
    '&granularity=' + encodeURIComponent(granularity);
  var d = findDevice(deviceId);
  if (d && d.kind === 'vkm360') {
    q += '&pipe=' + encodeURIComponent(document.getElementById('ar_pipe').value || '1');
  }
  return q;
}

function loadArchiveTable() {
  var deviceId = document.getElementById('ar_device').value;
  if (!deviceId) { showMsg('archiveMsg', false, 'Выберите прибор'); return; }
  if (!normalizeCalendarInput(document.getElementById('ar_from')) ||
      !normalizeCalendarInput(document.getElementById('ar_to'))) {
    showMsg('archiveMsg', false, 'Проверьте формат даты');
    return;
  }
  if (!document.getElementById('ar_from').value || !document.getElementById('ar_to').value) {
    showMsg('archiveMsg', false, 'Укажите период (с и по)');
    return;
  }
  document.getElementById('archiveMsg').className = 'msg';

  var xhr = new XMLHttpRequest();
  xhr.open('GET', '/api/archive?' + archiveQueryString(), true);
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4) { return; }
    if (xhr.status !== 200) {
      var err = 'HTTP ' + xhr.status;
      try { err = JSON.parse(xhr.responseText).error; } catch (e) {}
      showMsg('archiveMsg', false, 'Ошибка: ' + err);
      return;
    }
    var data = JSON.parse(xhr.responseText);
    renderArchiveTable(data);
  };
  xhr.send();
}

function renderArchiveTable(data) {
  var headHtml = '<tr><th>Период</th>';
  for (var i = 0; i < data.param_labels.length; i++) {
    headHtml += '<th>' + data.param_labels[i] + '</th>';
  }
  headHtml += '</tr>';
  document.getElementById('archiveTableHead').innerHTML = headHtml;

  var bodyHtml = '';
  if (!data.rows || data.rows.length === 0) {
    bodyHtml = '<tr><td colspan="' + (data.params.length + 1) + '">Нет данных за выбранный период</td></tr>';
  } else {
    for (var r = 0; r < data.rows.length; r++) {
      var row = data.rows[r];
      bodyHtml += '<tr><td>' + row.period + '</td>';
      for (var c = 0; c < data.params.length; c++) {
        var v = row.values[data.params[c]];
        bodyHtml += '<td>' + (v === undefined ? '' : v.toFixed(5)) + '</td>';
      }
      bodyHtml += '</tr>';
    }
  }
  document.getElementById('archiveTableBody').innerHTML = bodyHtml;
}

function exportArchiveCSV() {
  var deviceId = document.getElementById('ar_device').value;
  if (!deviceId) { showMsg('archiveMsg', false, 'Выберите прибор'); return; }
  if (!normalizeCalendarInput(document.getElementById('ar_from')) ||
      !normalizeCalendarInput(document.getElementById('ar_to'))) {
    showMsg('archiveMsg', false, 'Проверьте формат даты');
    return;
  }
  if (!document.getElementById('ar_from').value || !document.getElementById('ar_to').value) {
    showMsg('archiveMsg', false, 'Укажите период (с и по)');
    return;
  }
  window.location = '/api/archive/export?' + archiveQueryString();
}

// ===================== ГЛАВНАЯ (ДАШБОРД) =====================
// Добавлено 2026-08-29 по прямому запросу оператора: "мы никак не
// отслеживаем какое время сейчас в приборе... нужен дашборд". Данные
// приходят одним запросом с сервера (GET /api/dashboard, см.
// internal/web/api_dashboard.go) — вся сортировка/раскраска чисто на
// стороне браузера, сервер всегда отдаёт один и тот же порядок (по id
// прибора), сортировка не сохраняется между обновлениями (перегрузка
// каждые 30с — см. вызов setInterval в самом низу файла — сбросила бы
// её всё равно, так что запоминать выбранную сортировку смысла нет).
var dashboardData = [];
var dashboardSortKey = null;
var dashboardSortAsc = true;

// ===================== ЛОГ =====================
// Добавлено 2026-08-30 по прямому запросу оператора: "добавить в юай
// вкладку где будет крутится лог нашего сервера опроса как он сейчас в
// окне крутится". Опрос GET /api/log?after=N каждые 2с (см.
// internal/web/api_log.go — сервер отдаёт только НОВЫЕ строки с
// прошлого опроса, не всё заново). Раскраска/подсветка — не переписывает
// сами термины лога (это открытый, субъективный список, лучше уточнять
// по мере конкретных жалоб на конкретные фразы, а не пытаться угадать
// всё сразу), а лишь визуально помогает быстро находить главное:
// уровень сообщения, какого прибора касается строка, метки периода.
var logPaused = false;
var logAfterSeq = 0;
var logRawLines = []; // сырой текст без HTML-разметки — для копирования и скачивания
var logDeviceFilter = ''; // пусто = все приборы
var logEventFilter = 'all';

function toggleLogPause() {
  logPaused = !logPaused;
  document.getElementById('logPauseBtn').innerText = logPaused ? 'Продолжить' : 'Пауза';
}

function escapeHtmlForLog(s) {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

// humanizeLogLine переводит известные технические шаблоны из строки
// лога в понятные обычному оператору формулировки — ДОБАВЛЕНО
// 2026-08-30 по прямому запросу оператора: "из этого не понятно что
// за архив hourly, VKM архив main, [es-sync] проход... половина
// зашифрована половина латиницей — понятно только тебе но не обычному
// человеку".
//
// Работает ТОЛЬКО на отображении в браузере — сам файл mbgw_server.log
// на диске остаётся техническим (как есть, для более глубокой
// диагностики разработчиком) — переписывать все log.Printf по всему
// Go-коду разом было бы намного более крупной и рискованной работой,
// а эта вкладка и так уже показывает "живой" пересказ того же самого
// лога, не обязанный побайтово совпадать с файлом.
//
// Список замен НЕ исчерпывающий — покрывает шаблоны, на которые прямо
// пожаловался оператор, плюс несколько соседних той же природы. Если
// встретится ещё непонятная фраза — скажите, какая именно, добавим.
function humanizeLogLine(html) {
  var s = html;

  s = s.replace(/\[es-sync\]/g, '[синхронизация с ЭС]');
  s = s.replace(/архив hourly:/g, 'часовой архив:');
  s = s.replace(/VKM архив main:/g, 'архив ВКМ (получасовка):');
  s = s.replace(/дозабор (hourly|main):/g, 'восстановление недостающих архивных данных:');
  s = s.replace(/дозабор:/g, 'восстановление недостающих архивных данных:');
  s = s.replace(/backfill/gi, 'восстановление архива');
  s = s.replace(/manual_archive/g, 'ручной архивный опрос');
  s = s.replace(/сохранено часовок: (\d+)\/(\d+)/g, 'сохранено записей за час: $1 из $2');
  s = s.replace(/сохранено полей: (\d+)\/(\d+)/g, 'сохранено показателей: $1 из $2');
  s = s.replace(/проход завершён:/g, 'цикл отправки в ЭС завершён:');
  s = s.replace(/\[ВРЕМЯ\] часы ВКМ скорректированы на/g, '[КОРРЕКЦИЯ ВРЕМЕНИ] ВКМ скорректирован на');
  s = s.replace(
    /записано (\d+), пропущено \(уже есть\) (\d+), ошибок (\d+), окно (\S+)\.\.(\S+)/g,
    'отправлено новых точек: $1, уже были в ЭС ранее: $2, ошибок: $3, проверенный период: $4 — $5'
  );
  // Полный сырой дамп всех полей ответа прибора (map[...]) оператору не
  // нужен целиком на экране — короткое пояснение вместо простыни текста;
  // подробности при необходимости всё ещё доступны в файле лога на диске.
  s = s.replace(/поля=map\[[^\]]*\]/g, 'сырые данные с прибора (подробности — в файле лога)');

  return s;
}

// ===================== ПОЛОСКА ЖИВОЙ АКТИВНОСТИ ПРИБОРОВ =====================
// Добавлено 2026-09-02 по прямому запросу оператора: "нужно чтобы было
// понятно что опрос идёт в несколько потоков" (после того как поллер
// стал параллельным, см. internal/poller/poller.go). Явного процента
// прогресса на отдельный опрос у нас нет и не будет (опрос — это одно
// быстрое чтение, не скачивание файла), поэтому вместо шкал прогресса —
// вспышка активности: индикатор прибора ярко загорается при каждой
// новой строке лога об этом приборе и плавно гаснет. Несколько
// индикаторов, вспыхивающих почти одновременно, наглядно показывают
// параллельную работу — тот же дух, что и цветные полосы в консольных
// менеджерах пакетов, просто по событиям лога, а не по байтам.

// colorForDevice — детерминированный цвет по ID прибора (простой хеш
// строки в оттенок HSL) — один и тот же прибор ВСЕГДА получает один и
// тот же цвет и в полоске активности, и в самих строках лога, так что
// визуальный язык между ними общий.
function colorForDevice(id) {
  var hash = 0;
  for (var i = 0; i < id.length; i++) { hash = (hash * 31 + id.charCodeAt(i)) >>> 0; }
  var hue = hash % 360;
  return 'hsl(' + hue + ', 65%, 55%)';
}

// renderDeviceActivityStrip перестраивает полоску — вызывается при
// открытии вкладки «Лог» и после каждой загрузки списка приборов
// (loadDevices), чтобы новые/удалённые приборы сразу отражались.
function renderDeviceActivityStrip() {
  var strip = document.getElementById('deviceActivityStrip');
  if (!strip) { return; }

  var allSelected = (logDeviceFilter === '');
  var html = '<span class="log-filter-btn' + (allSelected ? ' selected' : '') +
    '" onclick="setLogDeviceFilter(\'\')" style="display:inline-block;padding:4px 10px;border-radius:12px;' +
    'background:#0e639c;opacity:' + (allSelected ? '1' : '0.45') +
    ';font-size:12px;color:#fff;font-weight:bold;white-space:nowrap;">Все приборы</span>';

  for (var i = 0; i < allDevices.length; i++) {
    var d = allDevices[i];
    var color = colorForDevice(d.id);
    var selected = logDeviceFilter === d.id;
    html += '<span id="activity_' + d.id + '" class="log-filter-btn' + (selected ? ' selected' : '') +
      '" onclick="setLogDeviceFilter(\'' + d.id + '\')" style="display:inline-block;padding:4px 10px;border-radius:12px;background:' + color +
      ';opacity:' + (selected ? '1' : '0.35') +
      ';transition:opacity 0.2s ease-out;font-size:12px;color:#111;font-weight:bold;white-space:nowrap;">' +
      (d.name || d.id) + '</span>';
  }
  strip.innerHTML = html;
}

function setLogDeviceFilter(deviceId) {
  logDeviceFilter = deviceId || '';
  renderDeviceActivityStrip();
  renderFilteredLog();
}

function setLogEventFilter(value) {
  logEventFilter = value || 'all';
  renderFilteredLog();
}

// flashDeviceActivity — вызывается для каждой новой строки лога,
// упомянувшей конкретный прибор: ярко "зажигает" его индикатор и через
// секунду плавно гасит обратно. Выбранный фильтр остаётся ярким постоянно.
function flashDeviceActivity(deviceId) {
  var el = document.getElementById('activity_' + deviceId);
  if (!el) { return; }
  el.style.opacity = '1';
  clearTimeout(el._flashTimer);
  if (logDeviceFilter === deviceId) { return; }
  el._flashTimer = setTimeout(function () {
    if (logDeviceFilter !== deviceId) { el.style.opacity = '0.35'; }
  }, 900);
}

function deviceIdFromLogLine(line) {
  // Реальная строка лога начинается с timestamp, поэтому искать [device]
  // только в самом начале строки нельзя. Сверяем квадратные скобки с
  // фактическими ID настроенных приборов, чтобы не принять [INFO],
  // [ERROR], [WARN] и другие служебные метки за ID прибора.
  for (var i = 0; i < allDevices.length; i++) {
    var id = allDevices[i].id;
    if (line.indexOf('[' + id + ']') !== -1) { return id; }
  }
  return null;
}


function logEventCategory(line) {
  var lower = (line || '').toLowerCase();
  if (line.indexOf('[НЕТ СВЯЗИ]') !== -1 ||
      line.indexOf('[ERROR]') !== -1 || line.indexOf('[ОШИБКА]') !== -1 ||
      line.indexOf('[FATAL]') !== -1 || line.indexOf('SQLITE_BUSY') !== -1 ||
      lower.indexOf('ошибка') !== -1) {
    return 'error';
  }
  if (line.indexOf('[ВРЕМЯ]') !== -1 || lower.indexOf('коррекц') !== -1) {
    return 'time';
  }
  if (line.indexOf('[es-sync]') !== -1 || lower.indexOf('[синхронизация с эс]') !== -1 ||
      line.indexOf('PointMains') !== -1 || lower.indexOf('бд эс') !== -1) {
    return 'es';
  }
  if (lower.indexOf('архив') !== -1 || lower.indexOf('дозабор') !== -1 ||
      lower.indexOf('довыгруз') !== -1 || lower.indexOf('переопрос') !== -1) {
    return 'archive';
  }
  if (line.indexOf('[SAVE]') !== -1 || line.indexOf('[СОХРАНЕНО]') !== -1 ||
      line.indexOf('[poller]') !== -1 || lower.indexOf('[опрос]') !== -1 ||
      lower.indexOf('текущий опрос') !== -1) {
    return 'poll';
  }
  return 'system';
}

function logLineMatchesFilters(line) {
  if (logDeviceFilter) {
    if (deviceIdFromLogLine(line) !== logDeviceFilter) { return false; }
  }
  if (logEventFilter !== 'all' && logEventCategory(line) !== logEventFilter) {
    return false;
  }
  return true;
}

function renderFilteredLog() {
  var view = document.getElementById('logView');
  if (!view) { return; }
  var html = '';
  for (var i = logRawLines.length - 1; i >= 0; i--) {
    if (logLineMatchesFilters(logRawLines[i])) {
      html += formatLogLine(logRawLines[i]) + '\n';
    }
  }
  view.innerHTML = html;
}

function russifyVisibleLogLine(line) {
  // Старые технические метки остаются в файле лога для обратной
  // совместимости, но оператор в UI всегда видит русские названия.
  return line
    .replace(/\[INFO\]/g, '[ИНФО]')
    .replace(/\[OK\]/g, '[ОК]')
    .replace(/\[WARN\]/g, '[ПРЕДУПРЕЖДЕНИЕ]')
    .replace(/\[ERROR\]/g, '[ОШИБКА]')
    .replace(/\[FATAL\]/g, '[КРИТИЧЕСКАЯ ОШИБКА]')
    .replace(/\[WEB\]/g, '[ВЕБ]')
    .replace(/\[SAVE\]/g, '[СОХРАНЕНО]')
    .replace(/\[poller\]/g, '[ОПРОС]')
    .replace(/\[es-sync\]/g, '[СИНХРОНИЗАЦИЯ С ЭС]');
}

function formatLogLine(line) {
  var html = escapeHtmlForLog(russifyVisibleLogLine(line));
  var cls = '';
  if (line.indexOf('[НЕТ СВЯЗИ]') !== -1 || line.indexOf('[ERROR]') !== -1 || line.indexOf('[ОШИБКА]') !== -1 || line.indexOf('[FATAL]') !== -1) {
    cls = 'status-bad';
  } else if (line.indexOf('[WARN]') !== -1 || line.indexOf('[ПРЕДУПРЕЖДЕНИЕ]') !== -1) {
    cls = 'log-warn';
  } else if (line.indexOf('[ВРЕМЯ]') !== -1) {
    cls = (line.indexOf('часы ВКМ скорректированы на') !== -1) ? 'status-good' : 'log-warn';
  } else if (line.indexOf('[OK]') !== -1 || line.indexOf('[ОК]') !== -1) {
    cls = 'status-good';
  }
  html = humanizeLogLine(html);

  // Метка периода архива ("период 29.08.2026 22:30") — жирным.
  html = html.replace(/(период \d{2}\.\d{2}\.\d{4} \d{2}:\d{2})/g, '<b>$1</b>');

  // Подсвечиваем реальный ID прибора независимо от timestamp и
  // служебных префиксов перед ним.
  var devId = deviceIdFromLogLine(line);
  if (devId) {
    var bracketed = '[' + devId + ']';
    html = html.replace(
      bracketed,
      '<b style="color:' + colorForDevice(devId) + '">' + bracketed + '</b>'
    );
  }

  if (cls) { return '<span class="' + cls + '">' + html + '</span>'; }
  return html;
}

function loadLog() {
  if (logPaused) { return; }
  var xhr = new XMLHttpRequest();
  xhr.open('GET', '/api/log?after=' + logAfterSeq, true);
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4 || xhr.status !== 200) { return; }
    var data;
    try { data = JSON.parse(xhr.responseText); } catch (e) { return; }
    if (!data.entries || !data.entries.length) { return; }

    for (var i = 0; i < data.entries.length; i++) {
      logRawLines.push(data.entries[i].text);
      var deviceId = deviceIdFromLogLine(data.entries[i].text);
      if (deviceId) { flashDeviceActivity(deviceId); }
    }
    logAfterSeq = data.latest_seq;

    if (logRawLines.length > 5000) {
      logRawLines = logRawLines.slice(logRawLines.length - 5000);
    }
    renderFilteredLog();
  };
  xhr.send();
}

function copyLog() {
  var text = logRawLines.join('\n');
  var ta = document.createElement('textarea');
  ta.value = text;
  ta.style.position = 'fixed';
  ta.style.left = '-9999px';
  document.body.appendChild(ta);
  ta.select();
  var ok = false;
  try { ok = document.execCommand('copy'); } catch (e) { ok = false; }
  document.body.removeChild(ta);
  if (ok) {
    showMsg('logMsg', true, 'Скопировано в буфер обмена (' + logRawLines.length + ' строк).');
  } else {
    showMsg('logMsg', false, 'Не удалось скопировать автоматически — выделите текст в окне лога вручную и нажмите Ctrl+C.');
  }
}

function downloadLog() {
  // Обычная ссылка на серверный эндпоинт вместо Blob/File API в
  // браузере — работает в любом браузере, включая старый IE на целевой
  // платформе (см. doc-комментарий handleDashboard в server.go про
  // ES5/IE-совместимость всего фронтенда этого проекта).
  window.location.href = '/api/log/download';
}

function clearLogView() {
  document.getElementById('logView').innerHTML = '';
  logRawLines = [];
}

// ===================== МОНИТОР ОПРОСА =====================
function pollKindLabel(kind) {
  if (kind === 'current') { return 'текущие данные'; }
  if (kind === 'archive') { return 'архив'; }
  if (kind === 'manual_archive') { return 'ручной архивный опрос'; }
  if (kind === 'backfill') { return 'восстановление архива'; }
  if (kind === 'manual_reload') { return 'принудительный переопрос'; }
  return kind || 'опрос';
}

function correctionText(st) {
  if (st.correction_mode === 'vkm_enabled') {
    if (st.last_correction_known) {
      var step = st.last_correction_seconds;
      var signed = (step > 0 ? '+' : '') + step + ' с';
      return 'Коррекция: включена<br/><span class="small-note">Последняя коррекция: ' + signed + ', ' + (st.last_correction_at || '—') + '</span>';
    }
    return 'Коррекция: включена, срабатываний не было';
  }
  if (st.correction_mode === 'vkm_disabled') { return 'Коррекция: отключена'; }
  if (st.correction_mode === 'manual_service') { return 'Коррекция: только вручную, в сервисном режиме прибора'; }
  if (st.correction_mode === 'not_implemented') { return 'Коррекция: не реализована'; }
  return 'Коррекция: нет данных';
}

function deviceTimeText(st) {
  var drift = 'Расхождение: нет данных';
  if (st.time_drift_known) {
    if (st.time_drift_reliable) {
      var v = Number(st.time_drift_seconds || 0);
      drift = 'Расхождение: ' + (v > 0 ? '+' : '') + v.toFixed(1) + ' с';
    } else {
      drift = 'Расхождение: нет надёжных данных';
    }
    if (st.time_drift_note) {
      drift += '<br/><span class="small-note">' + escapeHtmlForLog(st.time_drift_note) + '</span>';
    }
  }
  return drift + '<br/>' + correctionText(st);
}

function loadPollMonitor() {
  var panel = document.getElementById('panel-pollmonitor');
  if (!panel || panel.className.indexOf('active') === -1) { return; }

  var xhr = new XMLHttpRequest();
  xhr.open('GET', '/api/poll-monitor', true);
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4) { return; }
    var body = document.getElementById('pollMonitorTable');
    if (!body) { return; }
    if (xhr.status !== 200) {
      body.innerHTML = '<tr><td colspan="7" class="status-bad">Не удалось получить состояние опроса</td></tr>';
      return;
    }

    var runtime;
    try { runtime = JSON.parse(xhr.responseText) || []; } catch (e) {
      body.innerHTML = '<tr><td colspan="7" class="status-bad">Ошибка ответа сервера</td></tr>';
      return;
    }

    var byId = {};
    for (var i = 0; i < runtime.length; i++) { byId[runtime[i].id] = runtime[i]; }

    var html = '';
    for (var j = 0; j < allDevices.length; j++) {
      var d = allDevices[j];
      if (!d.enabled) { continue; }
      var st = byId[d.id] || {};
      var active = !!st.poll_in_progress;
      var statusTitle = active ? ('Сейчас выполняется: ' + pollKindLabel(st.poll_kind)) : 'Сейчас прибор не опрашивается';
      var dot = '<span class="poll-dot' + (active ? ' active' : '') + '" title="' + statusTitle + '"></span>';

      var started = st.poll_started_at || '—';
      var next = st.next_poll_at || 'ожидание расписания';

      var last;
      if (!st.last_poll_known) {
        last = '<span>ещё нет завершённого планового опроса</span>';
      } else {
        var kind = pollKindLabel(st.last_poll_kind);
        if (st.last_poll_ok) {
          last = (st.last_poll_finished_at || '—') + ' — <span class="status-good">успешно</span> (' + kind + ')';
        } else {
          last = (st.last_poll_finished_at || '—') + ' — <span class="status-bad">неуспешно</span> (' + kind + ')';
          if (st.last_poll_error) {
            last += '<br/><span class="muted">' + escapeHtmlForLog(st.last_poll_error) + '</span>';
          }
        }
      }

      var action = '<button class="btn play-btn" title="Опросить этот прибор сейчас" onclick="startManualDevicePoll(\'' + d.id + '\')">▶</button>';
      html += '<tr><td style="text-align:center;">' + dot + '</td><td>' +
        escapeHtmlForLog(d.name || d.id) + '</td><td>' + started + '</td><td>' + next + '</td><td>' + last + '</td><td>' + deviceTimeText(st) + '</td><td style="text-align:center;">' + action + '</td></tr>';
    }
    if (html === '') {
      html = '<tr><td colspan="7">Приборов пока нет</td></tr>';
    }
    body.innerHTML = html;
  };
  xhr.send();
}

var manualPollState = null;

function appendManualPollLog(text) {
  var el = document.getElementById('manualPollLog');
  if (!el) { return; }
  el.innerText += (el.innerText ? '\n' : '') + text;
  el.scrollTop = el.scrollHeight;
}

function closeManualPollPopup() {
  document.getElementById('manualPollOverlay').style.display = 'none';
  if (manualPollState && manualPollState.timer) { window.clearInterval(manualPollState.timer); }
  manualPollState = null;
}

function transportLabelForDevice(d) {
  if (!d) { return 'Канал прибора'; }
  if (d.transport_kind === 'rtu_serial') { return d.com || 'COM-порт'; }
  if (d.host) { return d.host + (d.port ? ':' + d.port : ''); }
  return 'Канал прибора';
}

function loadPollStateOnce(deviceId, done) {
  var xhr = new XMLHttpRequest();
  xhr.open('GET', '/api/poll-monitor', true);
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4) { return; }
    if (xhr.status !== 200) { done(null); return; }
    var arr;
    try { arr = JSON.parse(xhr.responseText) || []; } catch (e) { done(null); return; }
    for (var i = 0; i < arr.length; i++) {
      if (arr[i].id === deviceId) { done(arr[i]); return; }
    }
    done({});
  };
  xhr.send();
}

function startManualDevicePoll(deviceId) {
  if (manualPollState && manualPollState.timer) { window.clearInterval(manualPollState.timer); }
  manualPollState = null;
  var d = findDevice(deviceId);
  if (!d) { return; }
  document.getElementById('manualPollDeviceName').innerText = d.name || d.id;
  document.getElementById('manualPollLog').innerText = '';
  document.getElementById('manualPollOverlay').style.display = 'block';
  appendManualPollLog('Запрос ручного опроса прибора ' + (d.name || d.id) + '.');
  appendManualPollLog('Ручной опрос использует уже зарегистрированный службой канал прибора; второй COM-порт не открывается.');

  loadPollStateOnce(deviceId, function(before) {
    before = before || {};
    manualPollState = {
      deviceId: deviceId,
      baselineFinished: before.last_poll_finished_at || '',
      seenStarted: false,
      waitingLogged: false,
      timer: null
    };
    if (before.poll_in_progress) {
      appendManualPollLog('Сейчас выполняется ' + pollKindLabel(before.poll_kind) + '. Ручной запрос будет ждать своей очереди.');
      manualPollState.waitingLogged = true;
    }

    var xhr = new XMLHttpRequest();
    xhr.open('POST', '/api/devices/poll-now', true);
    xhr.setRequestHeader('Content-Type', 'application/json');
    xhr.onreadystatechange = function() {
      if (xhr.readyState !== 4) { return; }
      if (xhr.status !== 200) {
        var err = 'не удалось поставить опрос в очередь';
        try { err = (JSON.parse(xhr.responseText) || {}).error || err; } catch (e) {}
        appendManualPollLog('ERROR: ' + err);
        return;
      }
      appendManualPollLog('OK: ' + transportLabelForDevice(d) + ': канал прибора зарегистрирован работающей службой и готов к обмену.');
      appendManualPollLog('OK: ручной архивный опрос поставлен в приоритетную очередь.');
      manualPollState.timer = window.setInterval(watchManualDevicePoll, 500);
      watchManualDevicePoll();
    };
    xhr.send(JSON.stringify({device_id: deviceId}));
  });
}

function watchManualDevicePoll() {
  var state = manualPollState;
  if (!state) { return; }
  loadPollStateOnce(state.deviceId, function(st) {
    if (!manualPollState || manualPollState !== state) { return; }
    st = st || {};
    if (st.poll_in_progress && st.poll_kind === 'manual_archive') {
      if (!state.seenStarted) {
        state.seenStarted = true;
        appendManualPollLog('Опрос начат: ' + (st.poll_started_at || 'сейчас') + '.');
      }
      return;
    }
    if (st.poll_in_progress && st.poll_kind !== 'manual_archive') {
      if (!state.waitingLogged) {
        state.waitingLogged = true;
        appendManualPollLog('Прибор занят: выполняется ' + pollKindLabel(st.poll_kind) + '. Ручной запрос ждёт окончания этого обмена.');
      }
      return;
    }

    var finishedChanged = st.last_poll_finished_at && st.last_poll_finished_at !== state.baselineFinished;
    if (st.last_poll_kind === 'manual_archive' && (state.seenStarted || finishedChanged)) {
      if (state.timer) { window.clearInterval(state.timer); state.timer = null; }
      if (st.last_poll_ok) {
        appendManualPollLog('OK: ручной опрос завершён успешно' + (st.last_poll_finished_at ? ' — ' + st.last_poll_finished_at : '') + '.');
      } else {
        appendManualPollLog('ERROR: ручной опрос завершён с ошибкой' + (st.last_poll_finished_at ? ' — ' + st.last_poll_finished_at : '') + '.');
        if (st.last_poll_error) { appendManualPollLog('Причина: ' + st.last_poll_error); }
      }
      appendManualPollLog(transportLabelForDevice(findDevice(state.deviceId)) + ': канал остаётся открыт службой для следующих плановых опросов.');
      loadPollMonitor();
    }
  });
}

// ===================== СЛУЖБА =====================
function loadServiceStatus() {
  var panel = document.getElementById('panel-service');
  if (!panel || panel.className.indexOf('active') === -1) { return; }
  var xhr = new XMLHttpRequest();
  xhr.open('GET', '/api/service/status', true);
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4) { return; }
    if (xhr.status !== 200) {
      document.getElementById('serviceContent').innerHTML = '<p class="status-bad">Не удалось получить состояние службы.</p>';
      return;
    }
    var data;
    try { data = JSON.parse(xhr.responseText); } catch (e) { return; }
    renderServiceContent(data);
    loadServiceLog();
  };
  xhr.send();
}

function renderServiceContent(data) {
  var html = '';
  var build = data.build || {};
  var revision = build.revision || 'не определён';
  var shortRevision = revision.length > 12 ? revision.substring(0, 12) : revision;
  var sourceState = build.revision ? (build.modified ? 'есть незакоммиченные изменения' : 'чистая сборка') : 'состояние исходников не определено';
  html += '<div class="watchdog-box"><b>Версия программы</b>';
  html += '<p>Commit: <code title="' + escapeHtmlForLog(revision) + '">' + escapeHtmlForLog(shortRevision) + '</code> — ' + sourceState + '.</p>';
  html += '<p>Собрано: ' + escapeHtmlForLog(build.build_time || 'время сборки не указано') + '</p>';
  html += '<p>Go: ' + escapeHtmlForLog(build.go_version || 'нет данных') + '</p></div>';
  if (data.goos !== 'windows') {
    html += '<p class="small-note">Управление службой реализовано для Windows.</p>';
  } else {
    html += '<p>Режим запуска: <b>' + (data.running_as_service ? 'служба Windows' : 'ручной запуск') + '</b>.</p>';
    if (!data.service_installed) {
      html += '<p class="small-note">Служба mbgw_service не установлена. Установка из PowerShell администратора:</p>';
      html += '<pre style="background:#0c0c0c;color:#cccccc;padding:10px;border:1px solid #3e3e42;">mbgw.exe install-service --port 8080</pre>';
    } else {
      var stateClass = (data.service_state === 'работает') ? 'status-good' : 'status-bad';
      html += '<p>Состояние службы: <span class="' + stateClass + '">' + escapeHtmlForLog(data.service_state || 'неизвестно') + '</span></p>';
      html += '<p><button class="btn secondary" onclick="stopService()">Остановить</button></p>';
      html += '<p class="small-note">Запуск остановленной службы выполняется через «Службы Windows» или командой C:\\Windows\\System32\\sc.exe start mbgw_service. При аварийном завершении действует настроенная политика автоматического восстановления.</p>';
    }
  }

  var wd = data.watchdog || {};
  var wdClass = 'status-good';
  if (wd.state === 'предупреждение') { wdClass = 'log-warn'; }
  if (wd.state === 'зависание') { wdClass = 'status-bad'; }
  html += '<div class="watchdog-box"><b>Контроль зависания опроса</b>';
  html += '<p>Состояние: <span class="' + wdClass + '">' + escapeHtmlForLog(wd.state || 'нет данных') + '</span></p>';
  html += '<p>Таймаут: ' + (wd.timeout_minutes || '—') + ' мин.</p>';
  html += '<p>Последняя активность: ' + escapeHtmlForLog(wd.last_activity || 'нет данных') + '</p>';
  html += '<p class="small-note">' + escapeHtmlForLog(wd.message || '') + '</p>';
  if (wd.stuck_devices && wd.stuck_devices.length) {
    html += '<p class="log-warn">Возможно завис опрос приборов: ' + escapeHtmlForLog(wd.stuck_devices.join(', ')) + '</p>';
  }
  if (!data.running_as_service) {
    html += '<p class="small-note">При ручном запуске контроль зависания только показывает аварийное состояние и пишет журнал — процесс автоматически не завершается.</p>';
  }
  html += '</div>';
  document.getElementById('serviceContent').innerHTML = html;
}

function serviceLevelLabel(level) {
  if (level === 'успешно') { return 'Успешно'; }
  if (level === 'предупреждение') { return 'Предупреждение'; }
  if (level === 'ошибка') { return 'Ошибка'; }
  if (level === 'критично') { return 'Критично'; }
  return 'Информация';
}

function serviceLevelClass(level) {
  if (level === 'успешно') { return 'service-level-ok'; }
  if (level === 'предупреждение') { return 'service-level-warn'; }
  if (level === 'ошибка') { return 'service-level-error'; }
  if (level === 'критично') { return 'service-level-critical'; }
  return 'service-level-info';
}

function loadServiceLog() {
  var panel = document.getElementById('panel-service');
  if (!panel || panel.className.indexOf('active') === -1) { return; }
  var xhr = new XMLHttpRequest();
  xhr.open('GET', '/api/service/log?limit=300', true);
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4) { return; }
    var body = document.getElementById('serviceLogTable');
    if (!body) { return; }
    if (xhr.status !== 200) {
      body.innerHTML = '<tr><td colspan="4" class="status-bad">Журнал службы недоступен</td></tr>';
      return;
    }
    var rows;
    try { rows = JSON.parse(xhr.responseText) || []; } catch (e) { rows = []; }
    var filter = document.getElementById('serviceLogFilter').value;
    var html = '';
    for (var i = 0; i < rows.length; i++) {
      var r = rows[i];
      if (filter === 'lifecycle' && r.category !== 'запуск и остановка') { continue; }
      if (filter === 'watchdog' && r.category !== 'контроль зависания') { continue; }
      if (filter === 'problems' && r.level !== 'предупреждение' && r.level !== 'ошибка' && r.level !== 'критично') { continue; }
      var cls = serviceLevelClass(r.level);
      html += '<tr class="' + cls + '"><td>' + escapeHtmlForLog(r.time || '') + '</td><td>' + serviceLevelLabel(r.level) + '</td><td>' + escapeHtmlForLog(r.category || 'служба') + '</td><td>' + escapeHtmlForLog(r.message || '') + '</td></tr>';
    }
    if (!html) { html = '<tr><td colspan="4">Нет событий для выбранного фильтра</td></tr>'; }
    body.innerHTML = html;
  };
  xhr.send();
}

function stopService() {
  if (!confirm('Остановить МодбасШлюз? Опрос приборов прекратится до следующего запуска.')) { return; }
  document.getElementById('serviceMsg').className = 'msg';
  var xhr = new XMLHttpRequest();
  xhr.open('POST', '/api/service/stop', true);
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4) { return; }
    var data;
    try { data = JSON.parse(xhr.responseText); } catch (e) { showMsg('serviceMsg', false, 'Ошибка ответа сервера'); return; }
    if (data.ok) {
      showMsg('serviceMsg', true, 'Остановка запрошена. Служба дождётся завершения активных операций и только затем перейдёт в состояние «остановлена».');
    } else {
      showMsg('serviceMsg', false, 'Ошибка: ' + (data.error || 'неизвестная'));
    }
  };
  xhr.send();
}

function loadTimeCorrections() {
  var xhr = new XMLHttpRequest();
  xhr.open('GET', '/api/time-corrections?limit=100', true);
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4) { return; }
    var body = document.getElementById('timeCorrectionsTable');
    if (!body) { return; }
    if (xhr.status !== 200) {
      body.innerHTML = '<tr><td colspan="3" class="status-bad">Не удалось загрузить историю коррекций</td></tr>';
      return;
    }

    var data;
    try { data = JSON.parse(xhr.responseText) || []; } catch (e) {
      body.innerHTML = '<tr><td colspan="3" class="status-bad">Ошибка ответа сервера</td></tr>';
      return;
    }

    var html = '';
    for (var i = 0; i < data.length; i++) {
      var r = data[i];
      var deviceLabel = r.device_name ? (r.device_name + ' (' + r.device_id + ')') : r.device_id;
      var seconds = parseInt(r.correction_seconds, 10) || 0;
      var sign = seconds > 0 ? '+' : '';
      html += '<tr><td>' + deviceLabel + '</td><td>' + r.corrected_at +
        '</td><td class="status-good">' + sign + seconds + ' сек</td></tr>';
    }
    if (html === '') {
      html = '<tr><td colspan="3">Коррекций времени пока не выполнялось</td></tr>';
    }
    body.innerHTML = html;
  };
  xhr.send();
}

function loadDashboard() {
  var xhr = new XMLHttpRequest();
  xhr.open('GET', '/api/dashboard', true);
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4) { return; }
    if (xhr.status !== 200) { return; }
    try { dashboardData = JSON.parse(xhr.responseText) || []; } catch (e) { return; }
    renderDashboardTable();
  };
  xhr.send();
}

// sortDashboard — вызывается кликом по заголовку столбца. Повторный клик
// по тому же столбцу переключает направление (по возрастанию/убыванию),
// клик по другому столбцу сортирует по нему заново, по возрастанию.
function sortDashboard(key) {
  if (dashboardSortKey === key) {
    dashboardSortAsc = !dashboardSortAsc;
  } else {
    dashboardSortKey = key;
    dashboardSortAsc = true;
  }
  renderDashboardTable();
}

// parseRuDateTime парсит "ДД.ММ.ГГГГ ЧЧ:ММ:СС" (формат, которым сервер
// форматирует next_poll_at/last_period) в число миллисекунд — для
// сортировки по времени, раз сама строка лексикографически не
// сортируется как дата (день идёт первым).
function parseRuDateTime(s) {
  if (!s) { return 0; }
  var parts = s.split(' ');
  var d = parts[0].split('.');
  var t = (parts[1] || '00:00:00').split(':');
  return new Date(+d[2], +d[1] - 1, +d[0], +t[0], +t[1], +(t[2] || 0)).getTime();
}

function dashboardSortValue(row, key) {
  if (key === 'name') { return (row.name || row.id || '').toLowerCase(); }
  if (key === 'kind') { return row.kind || ''; }
  if (key === 'lag') { return row.lag_known ? row.lag_hours : -999999; } // "нет данных" — в самый низ при сортировке по возрастанию
  if (key === 'next') { return row.next_poll_at ? parseRuDateTime(row.next_poll_at) : 9999999999999; } // без расписания — в самый низ
  if (key === 'drift') {
    if (!row.time_drift_known || !row.time_drift_reliable) { return -999999; }
    return row.time_drift_seconds;
  }
  return '';
}

function deviceKindLabel(kind) {
  if (kind === 'vkm360') { return 'ВКМ-360'; }
  if (kind === 'akron') { return 'Акрон'; }
  if (kind === 'ivk-ter') { return 'ИВК-ТЭР'; }
  return kind || '—';
}

function renderDashboardTable() {
  var rows = dashboardData.slice(); // копия — не трогаем исходный порядок с сервера

  var systemHealth = document.getElementById('dashboardSystemHealth');
  if (systemHealth) {
    if (rows.length === 0) {
      systemHealth.innerHTML = 'Состояние шлюза: нет настроенных приборов';
    } else {
      var h = rows[0];
      var pollerOK = (h.poller_ok !== undefined) ? !!h.poller_ok : !!h.poller_last_cycle;
      var pollerText;
      if (pollerOK) {
        pollerText = '<span class="status-good">опрос приборов работает</span>' +
          (h.poller_last_cycle ? ' (последняя проверка ' + h.poller_last_cycle + ')' : '');
      } else {
        pollerText = '<span class="status-bad">опрос приборов НЕ работает</span>' +
          (h.poller_last_cycle ? ' (последняя проверка ' + h.poller_last_cycle + ')' : '');
      }
      var sqliteText;
      if (!h.sqlite_checked_at) {
        sqliteText = 'нет данных о локальной базе';
      } else if (h.sqlite_ok) {
        sqliteText = '<span class="status-good">локальная база работает</span> (проверено ' + h.sqlite_checked_at + ')';
      } else {
        sqliteText = '<span class="status-bad">ошибка локальной базы</span> (проверено ' + h.sqlite_checked_at + ')' +
          (h.sqlite_error ? ' — ' + h.sqlite_error : '');
      }
      systemHealth.innerHTML = 'Состояние шлюза — ' + pollerText + '; ' + sqliteText;
    }
  }
  if (dashboardSortKey) {
    rows.sort(function(a, b) {
      var va = dashboardSortValue(a, dashboardSortKey);
      var vb = dashboardSortValue(b, dashboardSortKey);
      var cmp = 0;
      if (va < vb) { cmp = -1; } else if (va > vb) { cmp = 1; }
      return dashboardSortAsc ? cmp : -cmp;
    });
  }

  var html = '';
  for (var i = 0; i < rows.length; i++) {
    var d = rows[i];
    var enabledText = d.enabled ? '<span class="status-good">да</span>' : '<span class="status-bad">нет</span>';

    var lagText, lagClass;
    if (!d.lag_known) {
      lagText = 'нет данных'; lagClass = 'status-bad';
    } else if (d.lag_hours === 0) {
      lagText = '0'; lagClass = 'status-good';
    } else {
      lagText = d.lag_hours.toFixed(1) + ' ч'; lagClass = 'status-bad';
    }

    var driftText, driftClass;
    if (!d.time_drift_known) {
      driftText = 'нет данных'; driftClass = '';
    } else if (!d.time_drift_reliable) {
      driftText = 'не определено'; driftClass = '';
    } else {
      var absSec = Math.abs(d.time_drift_seconds);
      var sign = d.time_drift_seconds >= 0 ? '+' : '-';
      driftText = sign + Math.round(absSec) + ' сек';
      driftClass = absSec > 300 ? 'status-bad' : (absSec > 60 ? '' : 'status-good');
    }
    var driftTitle = d.time_drift_checked_at ? ' title="проверено: ' + d.time_drift_checked_at + '"' : '';

    var workParts = [];
    workParts.push('текущие: ' + (d.last_current_success || '—'));
    workParts.push('архив: ' + (d.last_archive_success || '—'));
    if (d.sync_supported) {
      workParts.push('ЭС: ' + (d.last_es_write_success || '—'));
    }
    var workText = workParts.join('<br>');

    var actionCell = '';
    if (d.sync_supported) {
      actionCell = '<button class="btn secondary" onclick="openDashboardResyncBox(\'' + d.id + '\')">Принудительная пересинхронизация с ЭС</button>';
    }

    html += '<tr><td>' + (d.name || d.id) + ' (' + d.id + ')</td><td>' + deviceKindLabel(d.kind) + '</td><td>' +
      enabledText + '</td><td class="' + lagClass + '">' + lagText + '</td><td class="' + driftClass + '"' + driftTitle + '>' +
      driftText + '</td><td>' + workText + '</td><td>' + actionCell + '</td></tr>';
  }
  if (html === '') { html = '<tr><td colspan="7">Приборов пока нет</td></tr>'; }
  document.getElementById('dashboardTable').innerHTML = html;
}

function loadDevices() {
  var xhr = new XMLHttpRequest();
  xhr.open('GET', '/api/devices', true);
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4) { return; }
    if (xhr.status !== 200) { return; }
    allDevices = JSON.parse(xhr.responseText) || [];
    renderDevicesTable();
    renderDeviceActivityStrip();
  };
  xhr.send();
}

function renderDevicesTable() {
  var rows = '';
  for (var i = 0; i < allDevices.length; i++) {
    var d = allDevices[i];
    var transportDesc = d.transport_kind === 'modbus_tcp' ? (d.host + ':' + d.port) : d.com;
    var enabledText = d.enabled ? '<span class="status-good">да</span>' : '<span class="status-bad">нет</span>';
    rows += '<tr><td>' + d.id + '</td><td>' + d.name + '</td><td>' + deviceKindLabel(d.kind) + '</td><td>' +
      transportDesc + '</td><td>' + enabledText + '</td><td>' +
      '<button class="btn secondary" onclick="editDevice(\'' + d.id + '\')">Изменить</button> ' +
      '<button class="btn danger" onclick="deleteDevice(\'' + d.id + '\')">Удалить</button>' +
      '</td></tr>';
  }
  if (rows === '') { rows = '<tr><td colspan="6">Приборов пока нет</td></tr>'; }
  document.getElementById('devicesTable').innerHTML = rows;
}

function findDevice(id) {
  for (var i = 0; i < allDevices.length; i++) { if (allDevices[i].id === id) { return allDevices[i]; } }
  return null;
}

function showDeviceFormBody() {
  document.getElementById('deviceFormBody').style.display = 'block';
  document.getElementById('deviceFormOpenRow').style.display = 'none';
}

function openNewDeviceForm() {
  resetDeviceForm();
  showDeviceFormBody();
}

function editDevice(id) {
  var d = findDevice(id);
  if (!d) { return; }
  showDeviceFormBody();
  // Сброс сообщений от ПРЕЖНЕГО прибора — без этого «Сохранено. Для
  // применения запустите/перезапустите mbgw server.» (или результат
  // «Проверить прибор») продолжало висеть на экране после переключения
  // на другой прибор, как будто относится к нему (тот же класс бага,
  // что уже чинили для статуса переопроса — найдено оператором живьём,
  // 2026-08-29). Тот же приём, что уже применён в resetDeviceForm ниже.
  document.getElementById('deviceMsg').className = 'msg';
  document.getElementById('probeMsg').className = 'msg';
  editingOriginalKind = d.kind;
  document.getElementById('editWarning').style.display = 'block';
  document.getElementById('editWarningName').innerText = d.name;
  document.getElementById('editWarningID').innerText = d.id;
  document.getElementById('deviceFormTitle').innerText = 'Редактировать прибор: ' + id;
  document.getElementById('d_id').value = d.id;
  document.getElementById('d_id').disabled = true;
  document.getElementById('d_id_display').innerText = d.id;
  document.getElementById('d_name').value = d.name;
  document.getElementById('d_kind').value = d.kind;
  document.getElementById('d_profile').value = d.profile;
  document.getElementById('d_transport_kind').value = d.transport_kind;
  document.getElementById('d_host').value = d.host;
  document.getElementById('d_port').value = d.port || '';
  var comMatch = String(d.com || '').toUpperCase().match(/^COM([0-9]+)$/);
  document.getElementById('d_com').value = comMatch ? comMatch[1] : '';
  document.getElementById('d_baudrate').value = d.baudrate || '';
  document.getElementById('d_parity').value = d.parity || 'none';
  document.getElementById('d_stopbits').value = d.stopbits || '';
  document.getElementById('d_unit_id').value = d.unit_id || '';
  document.getElementById('d_timeout_ms').value = d.timeout_ms || '';
  document.getElementById('d_retries').value = d.retries || '3';
  document.getElementById('d_backfill_max_depth_hours').value = d.backfill_max_depth_hours || '0';
  document.getElementById('d_archive_every_periods').value = d.archive_every_periods || '1';
  setScheduleDays(d.archive_days_mask || 127);
  document.getElementById('d_archive_window_start').value = d.archive_window_start || '';
  document.getElementById('d_archive_window_end').value = d.archive_window_end || '';
  // archive_at_minute: -1 в БД означает "не задано явно" (сентинел, см.
  // internal/web/api_devices.go) — показываем оператору сразу
  // реальное действующее значение (5), а не сырое "-1", чтобы не
  // пришлось разбираться, что оно значит.
  document.getElementById('d_archive_at_minute').value = (d.archive_at_minute !== undefined && d.archive_at_minute >= 0) ? d.archive_at_minute : '5';
  document.getElementById('d_time_correction_deadband_seconds').value = (d.time_correction_deadband_seconds !== undefined ? d.time_correction_deadband_seconds : '0');
  document.getElementById('d_time_correction_max_step_seconds').value = (d.time_correction_max_step_seconds !== undefined ? d.time_correction_max_step_seconds : '0');
  document.getElementById('d_time_correction_daily_limit_seconds').value = (d.time_correction_daily_limit_seconds !== undefined ? d.time_correction_daily_limit_seconds : '0');
  document.getElementById('d_enabled').checked = !!d.enabled;
  onTransportKindChange();
  updateVKMTimeCorrectionVisibility();
  showTab('devices');
  window.scrollTo(0, document.body.scrollHeight);
}

function resetDeviceForm() {
  editingOriginalKind = null;
  document.getElementById('editWarning').style.display = 'none';
  document.getElementById('deviceFormTitle').innerText = 'Добавить прибор';
  document.getElementById('d_id').value = '';
  document.getElementById('d_id').disabled = false;
  document.getElementById('d_id_display').innerText = '—';
  document.getElementById('d_name').value = '';
  document.getElementById('d_kind').value = 'vkm360';
  document.getElementById('d_profile').value = 'profiles/vkm360.yaml';
  document.getElementById('d_transport_kind').value = 'modbus_tcp';
  document.getElementById('d_host').value = '';
  document.getElementById('d_port').value = '502';
  document.getElementById('d_com').value = '';
  document.getElementById('d_baudrate').value = '9600';
  document.getElementById('d_parity').value = 'none';
  document.getElementById('d_stopbits').value = '1';
  document.getElementById('d_unit_id').value = '1';
  document.getElementById('d_timeout_ms').value = '1000';
  document.getElementById('d_retries').value = '3';
  document.getElementById('d_backfill_max_depth_hours').value = '0';
  document.getElementById('d_archive_every_periods').value = '1';
  setScheduleDays(127);
  document.getElementById('d_archive_window_start').value = '';
  document.getElementById('d_archive_window_end').value = '';
  document.getElementById('d_archive_at_minute').value = '5';
  document.getElementById('d_time_correction_deadband_seconds').value = '0';
  document.getElementById('d_time_correction_max_step_seconds').value = '0';
  document.getElementById('d_time_correction_daily_limit_seconds').value = '0';
  document.getElementById('d_enabled').checked = true;
  onTransportKindChange();
  updateVKMTimeCorrectionVisibility();
  document.getElementById('probeMsg').className = 'msg';
  document.getElementById('deviceMsg').className = 'msg';
}

function currentDeviceFormAsJSON() {
  var isVKM = document.getElementById('d_kind').value === 'vkm360';
  return {
    id: document.getElementById('d_id').value,
    name: document.getElementById('d_name').value,
    kind: document.getElementById('d_kind').value,
    profile: document.getElementById('d_profile').value,
    transport_kind: document.getElementById('d_transport_kind').value,
    host: document.getElementById('d_host').value,
    port: intOrZero(document.getElementById('d_port').value),
    com: document.getElementById('d_transport_kind').value === 'modbus_tcp' ? '' : ('COM' + document.getElementById('d_com').value),
    baudrate: intOrZero(document.getElementById('d_baudrate').value),
    parity: document.getElementById('d_parity').value,
    stopbits: intOrZero(document.getElementById('d_stopbits').value),
    timeout_ms: intOrZero(document.getElementById('d_timeout_ms').value),
    unit_id: intOrZero(document.getElementById('d_unit_id').value),
    retries: intOrZero(document.getElementById('d_retries').value),
    current_poll_seconds: 0,
    backfill_max_depth_hours: intOrZero(document.getElementById('d_backfill_max_depth_hours').value),
    // gap_scan_window_hours раньше было жёстко захардкожено в 0 —
    // означало, что ПОСТОЯННОЕ самозалечивание пропусков (после каждого
    // обычного цикла опроса архива, см. device.go: d.GapScan) было
    // фактически всегда выключено для любого прибора через UI,
    // независимо от того, что оператор вводил в «Глубину дозабора при
    // старте» — то поле управляет ТОЛЬКО разовым дозабором при старте
    // сервера (BackfillArchives), не текущей работающей сессией. Найдено
    // оператором живьём (2026-08-29): принудительный переопрос столкнул
    // с линией плановый такт, период 18:30 потерялся, и без этого
    // изменения он бы не восстановился сам вплоть до следующего
    // перезапуска mbgw.exe. Теперь одно и то же число из формы задаёт
    // ОБА механизма разом — стартовый дозабор и постоянное
    // самозалечивание одинаковой глубиной, отдельного смысла держать их
    // разными в UI не было.
    gap_scan_window_hours: intOrZero(document.getElementById('d_backfill_max_depth_hours').value),
    // archive_at_minute раньше было жёстко захардкожено в -1 (что на
    // сервере трактуется как "используй умолчание 5") — поля для его
    // настройки в форме вообще не было, оператор не мог посмотреть или
    // изменить, когда именно опрашивается архив (добавлено 2026-08-30,
    // прямой запрос оператора — "непонятно, когда следующий опрос
    // запланирован", и заодно "нужна настройка", не только отображение
    // на вкладке «Главная»). Поле в форме уже показывает 5 по умолчанию
    // (см. HTML value="5"), так что для типового случая оператору
    // ничего менять не нужно — то же самое поведение, что и раньше.
    archive_at_minute: intOrZero(document.getElementById('d_archive_at_minute').value),
    archive_every_periods: intOrZero(document.getElementById('d_archive_every_periods').value) || 1,
    archive_days_mask: scheduleDaysMask(),
    archive_window_start: document.getElementById('d_archive_window_start').value,
    archive_window_end: document.getElementById('d_archive_window_end').value,
    time_correction_deadband_seconds: isVKM ? intOrZero(document.getElementById('d_time_correction_deadband_seconds').value) : 0,
    time_correction_max_step_seconds: isVKM ? intOrZero(document.getElementById('d_time_correction_max_step_seconds').value) : 0,
    time_correction_daily_limit_seconds: isVKM ? intOrZero(document.getElementById('d_time_correction_daily_limit_seconds').value) : 0,
    enabled: document.getElementById('d_enabled').checked,
    overwrite: document.getElementById('d_id').disabled // true only when editing an existing device
  };
}

function saveDevice() {
  var body = currentDeviceFormAsJSON();
  if (!body.id) { showMsg('deviceMsg', false, 'Заполните поле "Название"'); return; }
  // ЗАЩИТА ОТ КОЛЛИЗИИ ID (фикс случая 2026-08-23: сохранение прибора
  // ВКМ тихо перезаписало уже сохранённый прибор Akron, потому что оба
  // случайно сгенерировали одинаковый ID из похожих названий). Действует
  // только при добавлении НОВОГО прибора (d_id ещё не отключено) —
  // editDevice() отключает поле именно потому, что для СУЩЕСТВУЮЩЕГО
  // прибора его собственный ID и должен совпадать сам с собой при
  // сохранении, это не коллизия.
  var isNew = !document.getElementById('d_id').disabled;
  if (isNew && findDevice(body.id)) {
    showMsg('deviceMsg', false, 'Прибор с таким же ID ("' + body.id +
      '") уже есть — измените "Название" так, чтобы оно отличалось (например, добавьте номер).');
    return;
  }
  var validationErr = validateTransportFields(body);
  if (validationErr) { showMsg('deviceMsg', false, validationErr); return; }
  var scheduleErr = validateScheduleFields(body);
  if (scheduleErr) { showMsg('deviceMsg', false, scheduleErr); return; }
  var timeCorrectionErr = validateTimeCorrectionFields(body);
  if (timeCorrectionErr) { showMsg('deviceMsg', false, timeCorrectionErr); return; }
  var xhr = new XMLHttpRequest();
  xhr.open('POST', '/api/devices', true);
  xhr.setRequestHeader('Content-Type', 'application/json');
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4) { return; }
    if (xhr.status === 200) {
      showMsg('deviceMsg', true, 'Сохранено. Для применения запустите/перезапустите сервер mbgw.');
      loadDevices();
    } else {
      var err = 'HTTP ' + xhr.status;
      try { err = JSON.parse(xhr.responseText).error; } catch (e) {}
      showMsg('deviceMsg', false, 'Ошибка: ' + err);
    }
  };
  xhr.send(JSON.stringify(body));
}

function deleteDevice(id) {
  if (!confirm('Удалить прибор ' + id + '?')) { return; }
  var xhr = new XMLHttpRequest();
  xhr.open('POST', '/api/devices/delete', true);
  xhr.setRequestHeader('Content-Type', 'application/json');
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4) { return; }
    if (xhr.status === 200) { loadDevices(); } else { alert('Ошибка удаления: HTTP ' + xhr.status); }
  };
  xhr.send(JSON.stringify({ id: id }));
}

function probeDevice() {
  var body = currentDeviceFormAsJSON();
  document.getElementById('probeMsg').className = 'msg';
  var validationErr = validateTransportFields(body);
  if (validationErr) { showMsg('probeMsg', false, validationErr); return; }
  showMsg('probeMsg', true, 'Открываю порт/соединение и начинаю опрос прибора...');
  var xhr = new XMLHttpRequest();
  xhr.open('POST', '/api/devices/probe', true);
  xhr.setRequestHeader('Content-Type', 'application/json');
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4) { return; }
    var data;
    try { data = JSON.parse(xhr.responseText); } catch (e) { showMsg('probeMsg', false, 'Ошибка ответа сервера'); return; }
    if (data.ok) {
      var text = '';
      if (data.steps && data.steps.length) { text = data.steps.join('\n'); }
      if (text) { text += '\n\n'; }
      text += 'Прибор отвечает.';
      if (data.serial_number) { text += ' Заводской №: ' + data.serial_number + '.'; }
      if (data.firmware_info) { text += ' ' + data.firmware_info + '.'; }
      if (data.device_time) { text += ' Время прибора: ' + data.device_time + '.'; }
      // Поля ниже заполняет только probeVKM (internal/web/api_probe.go) —
      // мгновенные показания трубопровода №1, добавлено 2026-08-29 по
      // прямому запросу оператора вместе с самой реализацией проверки
      // ВКМ (раньше кнопка «Проверить прибор» для ВКМ ничего не делала).
      if (data.pressure) { text += ' Давление: ' + data.pressure + '.'; }
      if (data.temperature) { text += ' Температура: ' + data.temperature + '.'; }
      if (data.mass_flow) { text += ' Массовый расход: ' + data.mass_flow + '.'; }
      if (data.current_flow) { text += ' Текущий расход: ' + data.current_flow + '.'; }
      if (data.archive_info) { text += ' Архив: ' + data.archive_info + '.'; }
      if (data.error) { text += '\n\nДиагностические замечания: ' + data.error; }
      showMsg('probeMsg', true, text);
    } else {
      var errText = '';
      if (data.steps && data.steps.length) { errText = data.steps.join('\n'); }
      if (errText) { errText += '\n\n'; }
      errText += (data.error || 'Не удалось опросить прибор');
      showMsg('probeMsg', false, errText);
    }
  };
  xhr.send(JSON.stringify(body));
}

// POINT_TAG_DEFS — какие величины и с какими подсказками показывать на
// вкладке «Точки ЭС», по типу прибора. ОБЪЕДИНЕНО (2026-08-31, прямой
// запрос оператора: "переделать опрос акрона... сделать также как вкм"):
// раньше у ВКМ и Akron были СОВСЕМ разные вкладки/механизмы доставки
// данных в ЭС — теперь один и тот же механизм для обоих, различие
// только в том, сколько у прибора величин и как они называются.
var POINT_TAG_DEFS = {
  vkm360: [
    { tag: 'ST', label: 'Тепловая энергия (ST)', hint: 'Сырое значение с прибора — Джоули. Множитель 1 = в ЭС уйдут джоули «как есть» (большие числа). Чтобы в ЭС сразу приходили готовые Гкал — поставьте <b>0.000000000238846</b> (это 1&nbsp;/&nbsp;4.1868e9 — точное определение Гкал в Дж).' },
    { tag: 'S', label: 'Масса (S)', hint: 'Сырое значение с прибора — килограммы. Множитель 1 = в ЭС уйдут кг. Чтобы в ЭС приходили тонны (как в архиве МШ и в родном ПО прибора) — поставьте <b>0.001</b>.' },
    { tag: 'T', label: 'Температура (T)', hint: 'Сырое значение уже в °C — множитель 1 (без пересчёта), как правило, верен, менять обычно не нужно.' },
    { tag: 'Pi', label: 'Давление (Pi)', hint: 'Сырое значение с прибора — Паскали (Па). Множитель 1 = в ЭС уйдут Па. Для МПа поставьте <b>0.000001</b>; для кгс/см² — <b>0.0000101972</b>.' }
  ],
  akron: [
    { tag: 'V', label: 'Объём (V)', hint: 'Часовой расход вычисляется по разнице накопительного V и автоматически делится поровну на две получасовки ЭС. Множитель 1 = м³ без дополнительного пересчёта.' }
  ],
  'ivk-ter': [
    { tag: 'v_plus', label: 'Объём прямой', hint: 'Часовое значение из архива ИВК-ТЭР, м³. Множитель 1 передаёт значение без пересчёта.' },
    { tag: 'v_minus', label: 'Объём обратный', hint: 'Часовое значение из архива ИВК-ТЭР, м³. Множитель 1 передаёт значение без пересчёта.' },
    { tag: 'q_avg', label: 'Средний расход', hint: 'Средний расход за архивный час, л/мин. Множитель 1 передаёт значение без пересчёта.' },
    { tag: 'resistance', label: 'Сопротивление', hint: 'Сопротивление из часового архива ИВК-ТЭР, Ом.' },
    { tag: 'errors', label: 'Код ошибок', hint: 'Код состояния/ошибок из часовой записи. Обычно множитель оставляют 1.' },
    { tag: 'comm_fail_time', label: 'Нет связи, мин', hint: 'Продолжительность отсутствия связи, записанная прибором за период.' },
    { tag: 'flowmeter_type', label: 'Тип расходомера (код)', hint: 'Код типа расходомера из архивной записи ИВК-ТЭР.' },
    { tag: 'downtime', label: 'Простой, мин', hint: 'Продолжительность простоя за архивный период.' },
    { tag: 'power_loss_time', label: 'Нет питания, мин', hint: 'Продолжительность отсутствия питания за архивный период.' }
  ]
};

// currentChannelsKind находит тип выбранного на вкладке «Точки ЭС»
// прибора — по нему решается, сколько строк и с какими подсказками
// рисовать (см. POINT_TAG_DEFS).
function currentChannelsKind() {
  var deviceId = document.getElementById('ch_device').value;
  for (var i = 0; i < allDevices.length; i++) {
    if (allDevices[i].id === deviceId) { return allDevices[i].kind; }
  }
  return null;
}

// renderChannelsTable keeps the compact historical layout for Akron/IVK.
// VKM-360 uses a separate pipe/slot editor below because its source tag is no
// longer hard-coded: each of up to ten pipes has four independently mapped
// ID_PP slots.
function renderChannelsTable(kind) {
  var legacy = document.getElementById('channelsLegacyWrap');
  var vkm = document.getElementById('channelsVKMWrap');
  if (kind === 'vkm360') {
    legacy.style.display = 'none';
    vkm.style.display = 'block';
    renderVKMPipeGroups();
    return;
  }

  legacy.style.display = 'block';
  vkm.style.display = 'none';
  var defs = POINT_TAG_DEFS[kind] || [];
  var html = '';
  for (var i = 0; i < defs.length; i++) {
    var d = defs[i];
    html += '<tr><td>' + d.label + '</td><td><input id="ch_' + d.tag + '_id" type="text"></td>' +
      '<td><input id="ch_' + d.tag + '_factor" type="text" value="1"><div class="small-note">' + d.hint + '</div></td></tr>';
  }
  if (html === '') { html = '<tr><td colspan="3">Выберите прибор</td></tr>'; }
  document.getElementById('channelsTableBody').innerHTML = html;
}

function currentVKMActivePipeMap() {
  var deviceId = document.getElementById('ch_device').value;
  var pipes = [1];
  for (var i = 0; i < allDevices.length; i++) {
    if (allDevices[i].id === deviceId) {
      if (allDevices[i].vkm_active_pipes && allDevices[i].vkm_active_pipes.length) {
        pipes = allDevices[i].vkm_active_pipes;
      }
      break;
    }
  }
  var out = {};
  for (var j = 0; j < pipes.length; j++) { out[pipes[j]] = true; }
  return out;
}

function vkmFallbackTags() {
  var defs = POINT_TAG_DEFS.vkm360 || [];
  var out = [];
  for (var i = 0; i < defs.length; i++) { out.push(defs[i].tag); }
  return out;
}

function vkmTagLabel(tag) {
  var defs = POINT_TAG_DEFS.vkm360 || [];
  for (var i = 0; i < defs.length; i++) {
    if (defs[i].tag === tag) { return defs[i].label; }
  }
  return tag;
}

function vkmTagOptions(pipe, selected) {
  var meta = vkmSourceTagsByPipe[pipe];
  var tags = (meta && meta.tags && meta.tags.length) ? meta.tags.slice(0) : vkmFallbackTags();
  var seen = {};
  var html = '<option value="">— параметр —</option>';
  if (selected) {
    var already = false;
    for (var z = 0; z < tags.length; z++) { if (tags[z] === selected) { already = true; break; } }
    if (!already) { tags.unshift(selected); }
  }
  for (var i = 0; i < tags.length; i++) {
    var t = tags[i];
    if (seen[t]) { continue; }
    seen[t] = true;
    html += '<option value="' + escapeHtmlForLog(t) + '"' + (t === selected ? ' selected' : '') + '>' +
      escapeHtmlForLog(vkmTagLabel(t)) + '</option>';
  }
  return html;
}

function vkmPipeHasMapping(pipe) {
  for (var slot = 1; slot <= 4; slot++) {
    if (vkmChannelBySlot[pipe + ':' + slot]) { return true; }
  }
  return false;
}

function renderVKMPipeGroups() {
  var host = document.getElementById('vkmPipeGroups');
  if (!host) { return; }
  var active = currentVKMActivePipeMap();
  var html = '';
  for (var pipe = 1; pipe <= 10; pipe++) {
    var expanded = pipe === 1 || active[pipe] || vkmPipeHasMapping(pipe);
    var meta = vkmSourceTagsByPipe[pipe];
    var note = vkmPipeSourceNote(meta);
    html += '<div style="border:1px solid #3e3e42;margin:10px 0;background:#1e1e1e;">' +
      '<div style="padding:8px 10px;background:#333337;">' +
      '<button type="button" class="btn secondary" style="width:180px;text-align:left;" onclick="toggleVKMPipe(' + pipe + ')">' +
      '<span id="ch_pipe_' + pipe + '_arrow">' + (expanded ? '▼' : '▶') + '</span> Трубопровод ' + pipe + '</button>' +
      '<label style="margin-left:12px;color:#cccccc;"><input id="ch_pipe_' + pipe + '_active" type="checkbox"' + (active[pipe] ? ' checked' : '') + '> Опрос</label>' +
      '<span id="ch_pipe_' + pipe + '_scan" style="margin-left:12px;color:#bdbdbd;">' + vkmPipeDiscoveryLabel(vkmPipeDiscoveryByPipe[pipe]) + '</span>' +
      '</div>' +
      '<div id="ch_pipe_' + pipe + '_body" style="display:' + (expanded ? 'block' : 'none') + ';padding:8px 10px;">' +
      '<div id="ch_pipe_' + pipe + '_source_note" class="small-note">' + note + '</div>' +
      '<table><thead><tr><th>Слот</th><th>Параметр прибора</th><th>ID_PP</th><th>Множитель</th><th>Мин.</th><th>Макс.</th></tr></thead><tbody>';
    for (var slot = 1; slot <= 4; slot++) {
      var key = pipe + ':' + slot;
      var row = vkmChannelBySlot[key] || {};
      var tag = row.tag || '';
      var id = row.es_channel_id || '';
      var factor = (row.factor === undefined || row.factor === null) ? '1' : row.factor;
      var minValue = (row.min_value === undefined || row.min_value === null) ? '' : row.min_value;
      var maxValue = (row.max_value === undefined || row.max_value === null) ? '' : row.max_value;
      html += '<tr><td>' + slot + '</td>' +
        '<td><select id="ch_p' + pipe + '_s' + slot + '_tag" style="width:190px;">' + vkmTagOptions(pipe, tag) + '</select></td>' +
        '<td><input id="ch_p' + pipe + '_s' + slot + '_id" type="text" value="' + id + '" style="width:90px;"></td>' +
        '<td><input id="ch_p' + pipe + '_s' + slot + '_factor" type="text" value="' + factor + '" style="width:100px;"></td>' +
        '<td><input id="ch_p' + pipe + '_s' + slot + '_min" type="text" value="' + minValue + '" style="width:90px;"></td>' +
        '<td><input id="ch_p' + pipe + '_s' + slot + '_max" type="text" value="' + maxValue + '" style="width:90px;"></td></tr>';
    }
    html += '</tbody></table></div></div>';
  }
  host.innerHTML = html;
}

function toggleVKMPipe(pipe) {
  var body = document.getElementById('ch_pipe_' + pipe + '_body');
  var arrow = document.getElementById('ch_pipe_' + pipe + '_arrow');
  if (!body) { return; }
  var open = body.style.display !== 'none';
  body.style.display = open ? 'none' : 'block';
  if (arrow) { arrow.innerText = open ? '▶' : '▼'; }
}

function vkmPipeSourceNote(meta) {
  if (!meta || !meta.found) { return 'Архивная строка ещё не сохранена — показан базовый набор параметров.'; }
  if (meta.source === 'scan') {
    return 'Параметры из последнего сканирования' + (meta.ts ? ' (' + meta.ts.replace('T', ' ') + ')' : '') + '.';
  }
  return 'Параметры из последней архивной строки' + (meta.ts ? ' (' + meta.ts.replace('T', ' ') + ')' : '') + '.';
}

function vkmPipeDiscoveryLabel(meta) {
  if (!meta || !meta.status) { return 'не сканировался'; }
  if (meta.status === 'available') { return 'найден'; }
  if (meta.status === 'absent') { return 'не поддерживается'; }
  if (meta.status === 'uncertain') { return 'нет записей — не определено'; }
  return 'ошибка сканирования';
}

function applyVKMPipeDiscovery(rows) {
  vkmPipeDiscoveryByPipe = {};
  for (var i = 0; i < rows.length; i++) { vkmPipeDiscoveryByPipe[rows[i].pipe_no] = rows[i]; }
  for (var pipe = 1; pipe <= 10; pipe++) {
    var el = document.getElementById('ch_pipe_' + pipe + '_scan');
    if (el) { el.innerText = vkmPipeDiscoveryLabel(vkmPipeDiscoveryByPipe[pipe]); }
  }
}

function loadVKMPipeDiscovery() {
  var deviceId = document.getElementById('ch_device').value;
  if (!deviceId || currentChannelsKind() !== 'vkm360') { return; }
  var xhr = new XMLHttpRequest();
  xhr.open('GET', '/api/vkm-pipe-discovery?device_id=' + encodeURIComponent(deviceId), true);
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4 || xhr.status !== 200) { return; }
    var rows = [];
    try { rows = JSON.parse(xhr.responseText) || []; } catch (e) {}
    applyVKMPipeDiscovery(rows);
  };
  xhr.send();
}

function rescanVKMPipes() {
  var deviceId = document.getElementById('ch_device').value;
  if (!deviceId || currentChannelsKind() !== 'vkm360') { return; }
  showMsg('channelsMsg', true, 'Сканирую трубопроводы 1–10. Флажки «Опрос» останутся без изменений...');
  var xhr = new XMLHttpRequest();
  xhr.open('POST', '/api/vkm-pipe-discovery', true);
  xhr.setRequestHeader('Content-Type', 'application/json');
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4) { return; }
    if (xhr.status !== 200) {
      var errData = {};
      try { errData = JSON.parse(xhr.responseText); } catch (e) {}
      showMsg('channelsMsg', false, 'Сканирование не выполнено: ' + (errData.error || ('HTTP ' + xhr.status)));
      return;
    }
    var rows = [];
    try { rows = JSON.parse(xhr.responseText) || []; } catch (e2) {}
    applyVKMPipeDiscovery(rows);
    var available = 0, absent = 0, uncertain = 0, errors = 0;
    for (var i = 0; i < rows.length; i++) {
      if (rows[i].status === 'available') { available++; }
      else if (rows[i].status === 'absent') { absent++; }
      else if (rows[i].status === 'uncertain') { uncertain++; }
      else { errors++; }
    }
    showMsg('channelsMsg', true, 'Сканирование завершено: найдено ' + available + ', не поддерживается ' + absent + ', не определено ' + uncertain + ', ошибок ' + errors + '. Флажки «Опрос» не изменены.');
    refreshVKMSourceTags();
  };
  xhr.send(JSON.stringify({ device_id: deviceId }));
}

function applyVKMSourceTags(rows) {
  vkmSourceTagsByPipe = {};
  for (var i = 0; i < rows.length; i++) { vkmSourceTagsByPipe[rows[i].pipe_no] = rows[i]; }
  for (var pipe = 1; pipe <= 10; pipe++) {
    var meta = vkmSourceTagsByPipe[pipe];
    var noteEl = document.getElementById('ch_pipe_' + pipe + '_source_note');
    if (noteEl) {
      noteEl.innerText = vkmPipeSourceNote(meta);
    }
    for (var slot = 1; slot <= 4; slot++) {
      var sel = document.getElementById('ch_p' + pipe + '_s' + slot + '_tag');
      if (!sel) { continue; }
      var current = sel.value;
      sel.innerHTML = vkmTagOptions(pipe, current);
      sel.value = current;
    }
  }
}

function refreshVKMSourceTags() {
  var deviceId = document.getElementById('ch_device').value;
  if (!deviceId || currentChannelsKind() !== 'vkm360') { return; }
  var xhr = new XMLHttpRequest();
  xhr.open('GET', '/api/vkm-source-tags?device_id=' + encodeURIComponent(deviceId), true);
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4) { return; }
    if (xhr.status !== 200) {
      showMsg('channelsMsg', false, 'Не удалось обновить параметры ВКМ: HTTP ' + xhr.status);
      return;
    }
    var rows = [];
    try { rows = JSON.parse(xhr.responseText) || []; } catch (e) {}
    applyVKMSourceTags(rows);
  };
  xhr.send();
}

function populateDeviceSelect(selectId, kindFilter) {
  var sel = document.getElementById(selectId);
  var html = '';
  if (selectId === 'cur_device') { html += '<option value="">— все приборы —</option>'; }
  for (var i = 0; i < allDevices.length; i++) {
    if (kindFilter && allDevices[i].kind !== kindFilter) { continue; }
    html += '<option value="' + allDevices[i].id + '">' + allDevices[i].name + ' (' + allDevices[i].id + ')</option>';
  }
  if (html === '') { html = '<option value="">— нет подходящих приборов —</option>'; }
  sel.innerHTML = html;
  if (selectId === 'ch_device') { loadChannels(); }
  if (selectId === 'ar_device') { onArchiveDeviceChange(); }
}

function loadChannels() {
  var deviceId = document.getElementById('ch_device').value;
  var kind = currentChannelsKind();
  channelSafetyByTag = {};
  vkmChannelBySlot = {};
  vkmSourceTagsByPipe = {};
  vkmPipeDiscoveryByPipe = {};
  renderChannelsTable(kind);
  if (!deviceId) { return; }

  var xhr = new XMLHttpRequest();
  xhr.open('GET', '/api/vkm-channels?device_id=' + encodeURIComponent(deviceId), true);
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4 || xhr.status !== 200) { return; }
    var rows = JSON.parse(xhr.responseText) || [];
    if (kind === 'vkm360') {
      for (var i = 0; i < rows.length; i++) {
        vkmChannelBySlot[rows[i].pipe_no + ':' + rows[i].slot_no] = rows[i];
      }
      renderVKMPipeGroups();
      refreshVKMSourceTags();
      loadVKMPipeDiscovery();
      return;
    }

    var tags = (POINT_TAG_DEFS[kind] || []).map(function(d) { return d.tag; });
    var byTag = {};
    for (var j = 0; j < rows.length; j++) { byTag[rows[j].tag] = rows[j]; }
    channelSafetyByTag = byTag;
    for (var k = 0; k < tags.length; k++) {
      var t = tags[k];
      var idEl = document.getElementById('ch_' + t + '_id');
      var factorEl = document.getElementById('ch_' + t + '_factor');
      if (!idEl) { continue; }
      idEl.value = byTag[t] ? byTag[t].es_channel_id : '';
      factorEl.value = byTag[t] ? byTag[t].factor : '1';
    }
  };
  xhr.send();
}

function nullableFloat(v) {
  if (v === null || v === undefined || String(v).replace(/^\s+|\s+$/g, '') === '') { return null; }
  var n = parseFloat(v);
  return isNaN(n) ? null : n;
}

function saveChannels(force) {
  var deviceId = document.getElementById('ch_device').value;
  if (!deviceId) { showMsg('channelsMsg', false, 'Выберите прибор'); return; }
  var kind = currentChannelsKind();
  var channels = [];
  var channelIds = [];
  var activePipes = null;
  var seenIds = {};

  if (kind === 'vkm360') {
    activePipes = [];
    for (var pipe = 1; pipe <= 10; pipe++) {
      var activeEl = document.getElementById('ch_pipe_' + pipe + '_active');
      if (activeEl && activeEl.checked) { activePipes.push(pipe); }
      for (var slot = 1; slot <= 4; slot++) {
        var idEl = document.getElementById('ch_p' + pipe + '_s' + slot + '_id');
        if (!idEl) { continue; }
        var idVal = String(idEl.value).replace(/^\s+|\s+$/g, '');
        if (idVal === '') { continue; }
        var chId = parseInt(idVal, 10);
        if (isNaN(chId) || chId <= 0) {
          showMsg('channelsMsg', false, 'Трубопровод ' + pipe + ', слот ' + slot + ': ID_PP должен быть положительным целым числом.');
          return;
        }
        if (seenIds[chId]) {
          showMsg('channelsMsg', false, 'ID_PP ' + chId + ' указан более одного раза.');
          return;
        }
        seenIds[chId] = true;
        var tag = document.getElementById('ch_p' + pipe + '_s' + slot + '_tag').value;
        if (!tag) {
          showMsg('channelsMsg', false, 'Трубопровод ' + pipe + ', слот ' + slot + ': выберите параметр прибора.');
          return;
        }
        var minValue = nullableFloat(document.getElementById('ch_p' + pipe + '_s' + slot + '_min').value);
        var maxValue = nullableFloat(document.getElementById('ch_p' + pipe + '_s' + slot + '_max').value);
        if (minValue !== null && maxValue !== null && minValue > maxValue) {
          showMsg('channelsMsg', false, 'Трубопровод ' + pipe + ', слот ' + slot + ': минимум больше максимума.');
          return;
        }
        channels.push({
          pipe_no: pipe,
          slot_no: slot,
          tag: tag,
          es_channel_id: chId,
          factor: floatOrOne(document.getElementById('ch_p' + pipe + '_s' + slot + '_factor').value),
          min_value: minValue,
          max_value: maxValue
        });
        channelIds.push(chId);
      }
    }
    if (activePipes.length === 0) {
      showMsg('channelsMsg', false, 'Для ВКМ-360 должен быть включён хотя бы один трубопровод.');
      return;
    }
  } else {
    var tags = (POINT_TAG_DEFS[kind] || []).map(function(d) { return d.tag; });
    for (var i = 0; i < tags.length; i++) {
      var t = tags[i];
      var legacyIdVal = document.getElementById('ch_' + t + '_id').value;
      if (legacyIdVal === '') { continue; }
      var legacyChId = intOrZero(legacyIdVal);
      var safety = channelSafetyByTag[t] || {};
      channels.push({
        tag: t,
        es_channel_id: legacyChId,
        factor: floatOrOne(document.getElementById('ch_' + t + '_factor').value),
        min_value: (safety.min_value !== undefined ? safety.min_value : null),
        max_value: (safety.max_value !== undefined ? safety.max_value : null)
      });
      channelIds.push(legacyChId);
    }
  }

  if (force) {
    doSaveChannels(deviceId, channels, activePipes, true);
    return;
  }

  document.getElementById('channelsMsg').className = 'msg';
  var checkXhr = new XMLHttpRequest();
  checkXhr.open('POST', '/api/vkm-channels/check-history', true);
  checkXhr.setRequestHeader('Content-Type', 'application/json');
  checkXhr.onreadystatechange = function() {
    if (checkXhr.readyState !== 4) { return; }
    var data = {};
    try { data = JSON.parse(checkXhr.responseText); } catch (e) {}

    if (checkXhr.status === 200 && data.checked === false) {
      if (!confirm('Не удалось проверить точки напрямую в ЭС (' + (data.reason || 'причина неизвестна') +
        '). Продолжить сохранение без этой проверки?')) {
        showMsg('channelsMsg', false, 'Сохранение отменено.');
        return;
      }
      doSaveChannels(deviceId, channels, activePipes, false);
      return;
    }

    if (checkXhr.status === 200 && data.occupied && data.occupied.length > 0) {
      var msg = 'ВНИМАНИЕ: в ЭС уже есть данные в этих точках — похоже, они заняты другим прибором:\n';
      for (var j = 0; j < data.occupied.length; j++) {
        var o = data.occupied[j];
        msg += '  точка ' + o.channel_id + ': ' + o.row_count + ' записей, с ' + o.oldest + ' по ' + o.newest + '\n';
      }
      msg += '\nСохранить всё равно? Это может испортить данные другого прибора в ЭС!';
      if (!confirm(msg)) {
        showMsg('channelsMsg', false, 'Сохранение отменено — номер точки совпадает с уже используемым в ЭС.');
        return;
      }
    }

    doSaveChannels(deviceId, channels, activePipes, false);
  };
  checkXhr.send(JSON.stringify({ channel_ids: channelIds }));
}

function doSaveChannels(deviceId, channels, activePipes, force) {
  var xhr = new XMLHttpRequest();
  xhr.open('POST', '/api/vkm-channels', true);
  xhr.setRequestHeader('Content-Type', 'application/json');
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4) { return; }
    if (xhr.status === 200) {
      if (activePipes && activePipes.length) {
        for (var i = 0; i < allDevices.length; i++) {
          if (allDevices[i].id === deviceId) { allDevices[i].vkm_active_pipes = activePipes.slice(0); break; }
        }
      }
      showMsg('channelsMsg', true, 'Точки и трубопроводы сохранены.');
      loadChannels();
    } else if (xhr.status === 409) {
      var data = {};
      try { data = JSON.parse(xhr.responseText); } catch (e) {}
      var msg = (data.error || 'Обнаружен конфликт точек.') + ' Сохранить всё равно?';
      if (confirm(msg)) {
        doSaveChannels(deviceId, channels, activePipes, true);
      } else {
        showMsg('channelsMsg', false, 'Сохранение отменено — исправьте номер точки.');
      }
    } else {
      var errorData = {};
      try { errorData = JSON.parse(xhr.responseText); } catch (e2) {}
      showMsg('channelsMsg', false, 'Ошибка: ' + (errorData.error || ('HTTP ' + xhr.status)));
    }
  };
  var body = { device_id: deviceId, channels: channels, force: !!force };
  if (activePipes && activePipes.length) { body.active_pipes = activePipes; }
  xhr.send(JSON.stringify(body));
}

function loadESConnection() {
  var xhr = new XMLHttpRequest();
  xhr.open('GET', '/api/es-connection', true);
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4 || xhr.status !== 200) { return; }
    var c = JSON.parse(xhr.responseText);
    var configured = !!c.password_set;
    document.getElementById('es_status_configured').style.display = configured ? 'block' : 'none';
    document.getElementById('es_status_not_configured').style.display = configured ? 'none' : 'block';
    if (configured) {
      document.getElementById('es_status_server').innerText = c.sql_server;
      document.getElementById('es_status_db').innerText = c.sql_database;
    }
    document.getElementById('es_server').value = c.sql_server || '';
    document.getElementById('es_database').value = c.sql_database || '';
    document.getElementById('es_user').value = c.sql_user || '';
    document.getElementById('es_port').value = c.sql_port || '1433';
    document.getElementById('es_time_shift').value = (c.time_shift_minutes === undefined ? 0 : c.time_shift_minutes);
    document.getElementById('es_password_note').innerText = configured ?
      'Пароль уже сохранён (не показывается). Введите новый, только если хотите его изменить.' :
      'Пароль ещё не задан — введите его для сохранения.';
  };
  xhr.send();
}

function esConnectionFormAsJSON() {
  return {
    sql_server: document.getElementById('es_server').value,
    sql_database: document.getElementById('es_database').value,
    sql_user: document.getElementById('es_user').value,
    sql_password: document.getElementById('es_password').value,
    sql_port: intOrZero(document.getElementById('es_port').value),
    time_shift_minutes: intOrZero(document.getElementById('es_time_shift').value)
  };
}

function testESConnection() {
  var body = esConnectionFormAsJSON();
  document.getElementById('esTestMsg').className = 'msg';
  var xhr = new XMLHttpRequest();
  xhr.open('POST', '/api/es-connection/test', true);
  xhr.setRequestHeader('Content-Type', 'application/json');
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4) { return; }
    var data;
    try { data = JSON.parse(xhr.responseText); } catch (e) { showMsg('esTestMsg', false, 'Ошибка ответа сервера'); return; }
    if (data.ok) {
      showMsg('esTestMsg', true, 'Подключение успешно.');
      if (data.databases && data.databases.length > 0) {
        var sel = document.getElementById('es_database_select');
        var html = '';
        for (var i = 0; i < data.databases.length; i++) {
          html += '<option value="' + data.databases[i] + '">' + data.databases[i] + '</option>';
        }
        sel.innerHTML = html;
        sel.style.display = 'inline-block';
        sel.value = document.getElementById('es_database').value || data.databases[0];
        document.getElementById('es_database').style.display = 'none';
        sel.onchange = function() { document.getElementById('es_database').value = sel.value; };
        document.getElementById('es_database').value = sel.value;
      }
    } else {
      showMsg('esTestMsg', false, 'Ошибка: ' + data.error);
    }
  };
  xhr.send(JSON.stringify(body));
}

function saveESConnection() {
  var body = esConnectionFormAsJSON();
  if (!body.sql_password) { showMsg('esSaveMsg', false, 'Введите пароль для сохранения'); return; }
  var xhr = new XMLHttpRequest();
  xhr.open('POST', '/api/es-connection', true);
  xhr.setRequestHeader('Content-Type', 'application/json');
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4) { return; }
    if (xhr.status === 200) { showMsg('esSaveMsg', true, 'Сохранено.'); loadESConnection(); }
    else { showMsg('esSaveMsg', false, 'Ошибка: HTTP ' + xhr.status); }
  };
  xhr.send(JSON.stringify(body));
}

var pointLabels = {
  'V': 'Скорость потока',
  'Q': 'Расход',
  'am': 'Амплитуда сигнала',
  'acc_time': 'Время наработки',
  'second': 'Секунда (часы прибора)',
  'minute': 'Минута (часы прибора)',
  'hour': 'Час (часы прибора)',
  'date': 'День (часы прибора)',
  'month': 'Месяц (часы прибора)',
  'year': 'Год (часы прибора)',
  'serial_number': 'Заводской номер',
  'Current flow rate': 'Текущий расход',
  'Temperature': 'Температура',
  'current_time': 'Время прибора',
  'v_plus': 'Объём в прямом направлении',
  'v_minus': 'Объём в обратном направлении',
  'q_avg': 'Средний расход',
  'resistance': 'Сопротивление',
  'errors': 'Ошибки прибора',
  'comm_fail_time': 'Время отсутствия связи',
  'flowmeter_type': 'Тип расходомера',
  'downtime': 'Время простоя',
  'power_loss_time': 'Время отсутствия питания'
};

function loadCurrentData() {
  var deviceId = document.getElementById('cur_device').value;
  var url = '/api/current';
  if (deviceId) { url += '?device_id=' + encodeURIComponent(deviceId); }
  var xhr = new XMLHttpRequest();
  xhr.open('GET', url, true);
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4 || xhr.status !== 200) { return; }
    var data = JSON.parse(xhr.responseText) || [];
    var rows = '';
    for (var i = 0; i < data.length; i++) {
      var r = data[i];
      var t = r.Timestamp ? new Date(r.Timestamp).toLocaleTimeString('ru-RU') : '-';
      var qClass = r.Quality === 'VALID' ? 'status-good' : 'status-bad';
      var qText = r.Quality === 'VALID' ? 'Достоверно' : 'Недостоверно';
      var label = pointLabels[r.PointID] || r.PointID;
      rows += '<tr><td>' + r.DeviceID + '</td><td>' + label + '</td><td>' + r.Instance +
        '</td><td>' + r.Value + '</td><td>' + r.Unit + '</td><td class="' + qClass + '">' +
        qText + '</td><td>' + t + '</td></tr>';
    }
    if (rows === '') { rows = '<tr><td colspan="7">Нет данных</td></tr>'; }
    document.getElementById('currentData').innerHTML = rows;
  };
  xhr.send();
}

function loadSettings() {
  var xhr = new XMLHttpRequest();
  xhr.open('GET', '/api/settings', true);
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4 || xhr.status !== 200) { return; }
    var s = JSON.parse(xhr.responseText);
    document.getElementById('s_port').value = s.configured_port || '';
    document.getElementById('s_debug').checked = !!s.debug_log_enabled;
    document.getElementById('s_watchdog_timeout').value = s.watchdog_timeout_minutes || 10;

    var conflictBox = document.getElementById('s_port_conflict');
    if (s.actual_port && s.actual_port !== s.configured_port) {
      document.getElementById('s_port_actual').innerText = s.actual_port;
      document.getElementById('s_port_actual2').innerText = s.actual_port;
      document.getElementById('s_port_configured_repeat').innerText = s.configured_port;
      conflictBox.style.display = 'block';
    } else {
      conflictBox.style.display = 'none';
    }

    var note = 'Смена порта, отладочный лог и таймаут контроля зависания применяются сразу, без перезапуска.';
    document.getElementById('s_port_note').innerText = note;
  };
  xhr.send();
}

function adoptActualPort() {
  var actual = document.getElementById('s_port_actual').innerText;
  document.getElementById('s_port').value = actual;
  saveSettings(); // сохраняет сразу, без отдельного клика — раньше требовало двух действий и путало
}

function saveSettings() {
  var port = intOrZero(document.getElementById('s_port').value);
  if (port <= 0 || port > 65535) { showMsg('settingsMsg', false, 'Укажите порт в диапазоне 1-65535'); return; }
  var watchdogTimeout = intOrZero(document.getElementById('s_watchdog_timeout').value);
  if (watchdogTimeout < 2 || watchdogTimeout > 120) { showMsg('settingsMsg', false, 'Таймаут контроля зависания должен быть от 2 до 120 минут'); return; }
  var body = { configured_port: port, debug_log_enabled: document.getElementById('s_debug').checked, watchdog_timeout_minutes: watchdogTimeout };
  var xhr = new XMLHttpRequest();
  xhr.open('POST', '/api/settings', true);
  xhr.setRequestHeader('Content-Type', 'application/json');
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4) { return; }
    if (xhr.status === 200) {
      var resp = {};
      try { resp = JSON.parse(xhr.responseText); } catch (e) {}
      showMsg('settingsMsg', true, resp.note || 'Сохранено.');
      // если порт реально сменился, страница теперь обращается к
      // старому (уже закрытому) серверу — переходим на новый адрес
      // автоматически, вместо того чтобы оператор гадал, куда идти
      if (resp.note && resp.note.indexOf('уже работает на новом порту') >= 0) {
        setTimeout(function() {
          window.location = 'http://127.0.0.1:' + port + '/admin';
        }, 800);
      } else {
        loadSettings();
      }
    } else {
      showMsg('settingsMsg', false, 'Ошибка: HTTP ' + xhr.status);
    }
  };
  xhr.send(JSON.stringify(body));
}

// ===================== ПРОСТОЙ ВСПЛЫВАЮЩИЙ КАЛЕНДАРЬ =====================
// Internet Explorer (единственный браузер на прод-сервере) НЕ реализовал
// ни один из HTML5-типов полей даты/времени (<input type="date">,
// "datetime-local", "time") ни в одной своей версии — они просто
// становятся обычными текстовыми полями без всякого календаря и без
// проверки формата. Раньше здесь предполагалось, что "date" хотя бы
// откатится на что-то безопасное — не откатывается, календаря нет вообще
// (подтверждено живьём, 2026-08-26). Поэтому здесь свой собственный,
// маленький календарь на чистом ES5 — работает одинаково в любом браузере,
// включая IE. Календарь остаётся удобным способом выбора, но поле также
// можно редактировать вручную: принимаются ГГГГ-ММ-ДД и ДД.ММ.ГГГГ.

var calendarPopupEl = null;
var calendarActiveInputId = null;

var monthNamesRu = ['Январь', 'Февраль', 'Март', 'Апрель', 'Май', 'Июнь', 'Июль', 'Август', 'Сентябрь', 'Октябрь', 'Ноябрь', 'Декабрь'];
var weekDaysRu = ['Пн', 'Вт', 'Ср', 'Чт', 'Пт', 'Сб', 'Вс'];

function normalizeDateText(s) {
  s = (s || '').replace(/^\s+|\s+$/g, '');
  if (!s) { return ''; }

  var y, m, d, match;
  match = /^(\d{4})-(\d{1,2})-(\d{1,2})$/.exec(s);
  if (match) {
    y = parseInt(match[1], 10); m = parseInt(match[2], 10); d = parseInt(match[3], 10);
  } else {
    match = /^(\d{1,2})\.(\d{1,2})\.(\d{4})$/.exec(s);
    if (!match) { return null; }
    d = parseInt(match[1], 10); m = parseInt(match[2], 10); y = parseInt(match[3], 10);
  }

  var dt = new Date(y, m - 1, d);
  if (dt.getFullYear() !== y || dt.getMonth() !== m - 1 || dt.getDate() !== d) { return null; }
  return y + '-' + pad2(m) + '-' + pad2(d);
}

function normalizeCalendarInput(input) {
  if (!input) { return false; }
  var normalized = normalizeDateText(input.value);
  if (normalized === null) {
    input.style.borderColor = '#a1260d';
    input.title = 'Введите дату в формате ГГГГ-ММ-ДД или ДД.ММ.ГГГГ';
    return false;
  }
  if (normalized) { input.value = normalized; }
  input.style.borderColor = '#3e3e42';
  input.title = '';
  return true;
}

function parseISODate(s) {
  var normalized = normalizeDateText(s);
  if (!normalized) { return null; }
  var parts = normalized.split('-');
  return new Date(parseInt(parts[0], 10), parseInt(parts[1], 10) - 1, parseInt(parts[2], 10));
}

// attachCalendar делает поле нажимаемым — клик открывает всплывающий
// календарь под этим полем. Вызывается один раз при загрузке страницы
// для каждого из полей дат.
function attachCalendar(inputId) {
  var input = document.getElementById(inputId);
  if (!input) { return; }
  input.style.cursor = 'text';
  input.style.background = '#3c3c3c';
  input.onclick = function() { toggleCalendarPopup(inputId); };
  input.onblur = function() { normalizeCalendarInput(input); };
}

function toggleCalendarPopup(inputId) {
  if (calendarPopupEl && calendarActiveInputId === inputId) {
    closeCalendarPopup();
    return;
  }
  closeCalendarPopup();

  var input = document.getElementById(inputId);
  var rect = input.getBoundingClientRect();
  var scrollTop = document.body.scrollTop || document.documentElement.scrollTop;
  var scrollLeft = document.body.scrollLeft || document.documentElement.scrollLeft;

  var popup = document.createElement('div');
  popup.style.position = 'absolute';
  popup.style.left = (rect.left + scrollLeft) + 'px';
  popup.style.top = (rect.bottom + scrollTop + 4) + 'px';
  popup.style.background = '#2d2d30';
  popup.style.border = '1px solid #3e3e42';
  popup.style.borderRadius = '4px';
  popup.style.padding = '10px';
  popup.style.zIndex = '9999';
  popup.style.width = '230px';
  popup.style.boxShadow = '0 4px 12px rgba(0,0,0,0.5)';

  var current = input.value ? parseISODate(input.value) : new Date();
  if (!current) { current = new Date(); }

  document.body.appendChild(popup);
  calendarPopupEl = popup;
  calendarActiveInputId = inputId;
  renderCalendarMonth(current.getFullYear(), current.getMonth());

  setTimeout(function() {
    if (document.addEventListener) {
      document.addEventListener('click', outsideCalendarClick, false);
    } else if (document.attachEvent) {
      document.attachEvent('onclick', outsideCalendarClick);
    }
  }, 0);
}

function outsideCalendarClick(e) {
  var evt = e || window.event;
  var target = evt.target || evt.srcElement;
  if (!calendarPopupEl) { return; }
  var node = target;
  while (node) {
    if (node === calendarPopupEl) { return; } // клик внутри календаря — не закрываем
    node = node.parentNode;
  }
  if (target.id === calendarActiveInputId) { return; } // клик по самому полю — им управляет toggle
  closeCalendarPopup();
}

function closeCalendarPopup() {
  if (calendarPopupEl && calendarPopupEl.parentNode) {
    calendarPopupEl.parentNode.removeChild(calendarPopupEl);
  }
  calendarPopupEl = null;
  calendarActiveInputId = null;
  if (document.removeEventListener) {
    document.removeEventListener('click', outsideCalendarClick, false);
  } else if (document.detachEvent) {
    document.detachEvent('onclick', outsideCalendarClick);
  }
}

function renderCalendarMonth(year, month) {
  if (!calendarPopupEl) { return; }
  var html = '';
  html += '<div style="display:table;width:100%;margin-bottom:8px;">';
  html += '<div style="display:table-cell;text-align:left;"><button type="button" style="background:#3c3c3c;color:#fff;border:none;padding:4px 10px;cursor:pointer;" onclick="return calendarChangeMonth(' + year + ',' + month + ',-1,event)">&lt;</button></div>';
  html += '<div style="display:table-cell;text-align:center;color:#fff;font-size:13px;">' + monthNamesRu[month] + ' ' + year + '</div>';
  html += '<div style="display:table-cell;text-align:right;"><button type="button" style="background:#3c3c3c;color:#fff;border:none;padding:4px 10px;cursor:pointer;" onclick="return calendarChangeMonth(' + year + ',' + month + ',1,event)">&gt;</button></div>';
  html += '</div>';
  html += '<table style="width:100%;border-collapse:collapse;font-size:12px;">';
  html += '<tr>';
  for (var d = 0; d < 7; d++) { html += '<th style="color:#969696;padding:4px;font-weight:normal;">' + weekDaysRu[d] + '</th>'; }
  html += '</tr>';

  var firstDay = new Date(year, month, 1);
  var startOffset = (firstDay.getDay() + 6) % 7; // понедельник = первый столбец
  var daysInMonth = new Date(year, month + 1, 0).getDate();
  var todayStr = dateToInputValue(new Date());

  var day = 1;
  var rows = Math.ceil((startOffset + daysInMonth) / 7);
  for (var row = 0; row < rows; row++) {
    html += '<tr>';
    for (var col = 0; col < 7; col++) {
      if (row === 0 && col < startOffset) {
        html += '<td></td>';
      } else if (day > daysInMonth) {
        html += '<td></td>';
      } else {
        var dStr = year + '-' + pad2(month + 1) + '-' + pad2(day);
        var bg = (dStr === todayStr) ? '#0e639c' : 'transparent';
        html += '<td style="text-align:center;padding:6px 0;cursor:pointer;color:#ccc;background:' + bg + ';" ' +
          'onmouseover="this.style.background=\'#3c3c3c\'" ' +
          'onmouseout="this.style.background=\'' + bg + '\'" ' +
          'onclick="calendarPickDate(\'' + dStr + '\')">' + day + '</td>';
        day++;
      }
    }
    html += '</tr>';
  }
  html += '</table>';
  calendarPopupEl.innerHTML = html;
}

function calendarChangeMonth(year, month, delta, e) {
  var evt = e || window.event;
  if (evt) {
    if (evt.stopPropagation) { evt.stopPropagation(); }
    evt.cancelBubble = true;
  }
  month += delta;
  if (month < 0) { month = 11; year -= 1; }
  if (month > 11) { month = 0; year += 1; }
  renderCalendarMonth(year, month);
  return false;
}

function calendarPickDate(dateStr) {
  var inputId = calendarActiveInputId;
  document.getElementById(inputId).value = dateStr;
  closeCalendarPopup();
}

attachCalendar('ar_from');
attachCalendar('ar_to');
// populateHourSelect заполняет выпадающий список часов 00-23 — по умолчанию
// выбран первый вариант (00), что и даёт "по умолчанию 00:00" без
// дополнительного кода. Отдельная функция, а не разметка вручную на 24
// строки — компактнее и меньше шансов ошибиться при правке.
function populateHourSelect(selectId) {
  var sel = document.getElementById(selectId);
  var html = '';
  for (var h = 0; h < 24; h++) {
    var hh = pad2(h);
    html += '<option value="' + hh + '">' + hh + '</option>';
  }
  sel.innerHTML = html;
}
function populateMinuteSelect(selectId) {
  var sel = document.getElementById(selectId);
  var html = '';
  for (var m = 0; m < 60; m++) {
    var mm = pad2(m);
    html += '<option value="' + mm + '">' + mm + '</option>';
  }
  sel.innerHTML = html;
}
populateHourSelect('ar_from_h');
populateHourSelect('ar_to_h');
populateMinuteSelect('ar_from_m');
populateMinuteSelect('ar_to_m');
document.getElementById('ar_from_h').value = '00';
document.getElementById('ar_from_m').value = '00';
document.getElementById('ar_to_h').value = '23';
document.getElementById('ar_to_m').value = '59';

populateHourSelect('rl_from_h');
populateHourSelect('rl_to_h');

attachCalendar('rl_from');
attachCalendar('rl_to');

populateHourSelect('dr_from_h');
populateHourSelect('dr_to_h');
attachCalendar('dr_from');
attachCalendar('dr_to');

loadDevices();
loadProfiles();
resetDeviceForm();
document.getElementById('deviceFormBody').style.display = 'none';
document.getElementById('deviceFormOpenRow').style.display = 'block';
loadDashboard();
loadPollMonitor();
// Автообновление вкладки «Главная» — раз в 30с, независимо от того,
// какая вкладка сейчас открыта (дёшево: один маленький GET-запрос), так
// что оператор видит актуальную картину сразу при переключении на неё,
// без ожидания. Тот же интервал, что у старого диагностического
// дашборда на / (см. handleDashboard в server.go, setInterval(loadData,
// 30000)) — уже проверенное на практике значение для этого проекта.
setInterval(loadDashboard, 30000);
// Монитор обновляется раз в секунду, но запрос выполняется только когда
// вкладка открыта. Endpoint читает только память процесса и не трогает SQLite.
setInterval(loadPollMonitor, 1000);
// Состояние службы и её отдельный журнал обновляются только при открытой вкладке.
setInterval(loadServiceStatus, 5000);

loadLog();
// Опрос новых строк лога каждые 2с — независимо от того, какая вкладка
// открыта сейчас, тот же принцип, что и у loadDashboard выше (дёшево:
// сервер отдаёт только НОВЫЕ строки, не весь буфер целиком).
setInterval(loadLog, 2000);
</script>
</body>
</html>`

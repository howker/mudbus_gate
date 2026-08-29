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
//     заголовок столбца; кнопка «Синхронизировать сейчас» для ВКМ прямо
//     в строке прибора (добавлено 2026-08-29). Активна по умолчанию.
//  1. Приборы — список + форма добавления/редактирования (включая
//     «Проверить прибор» -> POST /api/devices/probe) + удаление.
//  2. Каналы ЭС — таблица тег->канал+множитель для одного прибора,
//     имеет смысл только для приборов типа vkm360.
//  3. Подключение к ЭС — форма подключения к SQL Server + «Проверить
//     подключение» (POST /api/es-connection/test, ничего не сохраняет).
//  4. Приём Акрона (ЭС) — адрес прослушивания для конкретного прибора,
//     имеет смысл только для приборов типа akron.
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
</style>
</head>
<body>

<div class="topbar"><h2>mbgw — Управление приборами</h2></div>
<div class="tabs">
  <button class="tab-btn active" onclick="showTab('dashboard')">Главная</button>
  <button class="tab-btn" onclick="showTab('devices')">Приборы</button>
  <button class="tab-btn" onclick="showTab('channels')">Каналы ЭС</button>
  <button class="tab-btn" onclick="showTab('esconn')">Подключение к ЭС</button>
  <button class="tab-btn" onclick="showTab('akron')">Приём Акрона (ЭС)</button>
  <button class="tab-btn" onclick="showTab('settings')">Настройки</button>
  <button class="tab-btn" onclick="showTab('archive')">Архив</button>
  <button class="tab-btn" onclick="showTab('current')">Последний опрос</button>
</div>

<div class="content">

  <!-- ===================== РџР РР‘РћР Р« ===================== -->
  <!-- ===================== ГЛАВНАЯ (ДАШБОРД) ===================== -->
  <div id="panel-dashboard" class="panel active">
    <div class="section">
      <h3>Статус приборов</h3>
      <p class="small-note">Отставание архива — сколько последних периодов ещё не собрано, в часах (получасовки ВКМ и часовки Акрона — на одной шкале). 0 = данные свежие. Проверка учитывает плановую задержку опроса (обычно 5 минут после границы периода + небольшой запас), чтобы не показывать ложное отставание сразу после границы часа/получаса.</p>
      <p class="small-note">Расхождение времени — на сколько часы ПРИБОРА (не сервера) отличаются от ожидаемого, по данным последнего собранного архива ВКМ. Положительное = часы прибора спешат, отрицательное = отстают. Коррекция времени прибора через mbgw не реализована — это только наблюдение.</p>
      <p class="small-note">«Синхронизировать сейчас» (только для ВКМ) — просит уже работающий цикл отправки в ЭС сделать внеплановый проход немедленно, не дожидаясь обычного часового цикла. Полезно, если вы только что запустили принудительный переопрос или вручную дозагрузили данные и хотите увидеть их в ЭС сразу, не ожидая часа. Саму архивную запись у прибора эта кнопка НЕ переопрашивает — она лишь отправляет то, что уже собрано в нашей базе.</p>
      <table>
        <thead><tr>
          <th style="cursor:pointer;" onclick="sortDashboard('name')">Прибор ⇅</th>
          <th style="cursor:pointer;" onclick="sortDashboard('kind')">Тип ⇅</th>
          <th>Включён</th>
          <th style="cursor:pointer;" onclick="sortDashboard('lag')">Отставание архива ⇅</th>
          <th>Последний период</th>
          <th style="cursor:pointer;" onclick="sortDashboard('next')">Следующий опрос ⇅</th>
          <th style="cursor:pointer;" onclick="sortDashboard('drift')">Расхождение времени ⇅</th>
          <th>Действие</th>
        </tr></thead>
        <tbody id="dashboardTable"><tr><td colspan="8">Загрузка...</td></tr></tbody>
      </table>
      <div id="dashboardSyncMsg" class="msg"></div>
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
        </select>
      </div>
      <div class="form-row"><label>Путь к профилю</label>
        <select id="d_profile"></select>
      </div>
      <div class="form-row"><label>Тип связи</label>
        <select id="d_transport_kind" onchange="onTransportKindChange()">
          <option value="modbus_tcp">TCP (modbus_tcp)</option>
          <option value="rtu_serial">Последовательный порт (rtu_serial)</option>
          <option value="tcp_serial">TCP-serial (конвертер)</option>
        </select>
      </div>
      <div id="tcpFields">
        <div class="form-row"><label>IP-адрес</label><input id="d_host" type="text" placeholder="10.48.228.126"></div>
        <div class="form-row"><label>Порт</label><input id="d_port" type="text" value="502"></div>
      </div>
      <div id="serialFields">
        <div class="form-row"><label>COM-порт</label><input id="d_com" type="text" placeholder="COM105"></div>
        <div class="form-row"><label>Скорость (бод)</label><input id="d_baudrate" type="text" value="9600"></div>
        <div class="form-row"><label>Чётность</label>
          <select id="d_parity"><option value="none">none</option><option value="even">even</option><option value="odd">odd</option></select>
        </div>
        <div class="form-row"><label>Стоп-биты</label><input id="d_stopbits" type="text" value="1"></div>
      </div>
      <div class="form-row"><label>Адрес на линии (unit id)</label><input id="d_unit_id" type="text" value="1"></div>
      <div class="form-row"><label>Таймаут (мс)</label><input id="d_timeout_ms" type="text" value="1000"></div>
      <div class="form-row"><label>Количество повторов при ошибке</label><input id="d_retries" type="text" value="3"></div>
      <p class="small-note" style="margin-left:220px;margin-top:-8px;">При сбое запрос повторяется с растущей паузой (0.2с, 0.4с, 0.8с) — полезно на нестабильной линии (RS-485 с помехами, обрывы).</p>
      <div class="form-row"><label>Включён</label><input id="d_enabled" type="checkbox" checked></div>

      <p><a href="#" onclick="toggleAdvanced(); return false;" style="color:#0e639c;font-size:13px;" id="advancedToggle">▸ Дополнительные настройки</a></p>
      <div id="advancedFields" style="display:none;">
        <div class="form-row"><label>Опрос текущих (сек)</label><input id="d_current_poll_seconds" type="text" value="3600"></div>
        <div class="form-row"><label>Глубина дозабора при старте (часов)</label><input id="d_backfill_max_depth_hours" type="text" value="0"></div>
        <p class="small-note">Сколько часов назад искать и добирать пропуски при каждом запуске сервера. 0 — использовать значение по умолчанию (24ч для ВКМ). Если сервер может простаивать дольше суток (плановое обслуживание и т.п.) — увеличьте, например до 72-96, чтобы пропуски добирались автоматически при следующем старте, без ручного «Принудительного переопроса».</p>
        <p class="small-note">Как часто опрашивать мгновенные показания (не архив). Раз в час обычно достаточно — этот шлюз собирает архив, не ведёт непрерывную телеметрию.</p>
        <div class="form-row"><label>Опрос архива, минута после границы</label><input id="d_archive_at_minute" type="text" value="5"></div>
        <p class="small-note">Через сколько минут ПОСЛЕ границы периода запрашивать архив (получасовки у ВКМ — в HH:05 и HH:35, часовки у Akron — в HH:05, при значении по умолчанию 5). Прибору нужно время, чтобы закрыть период и подготовить данные — опрос точно на самой границе (0) обычно даёт ещё не готовый или неполный результат. Значение видно и настраивается здесь же, что и на вкладке «Главная» в столбце «Следующий опрос» — добавлено 2026-08-30, раньше это было изменить нельзя вообще (жёстко 5 минут для всех приборов).</p>
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

  <!-- ===================== КАНАЛЫ ЭС ===================== -->
  <div id="panel-channels" class="panel">
    <div class="section">
      <h3>Каналы ЭС для ВКМ-прибора</h3>
      <div class="form-row"><label>Прибор</label>
        <select id="ch_device" onchange="loadChannels()"></select>
      </div>
      <table>
        <thead><tr><th>Величина</th><th>Номер канала ЭС (ID_Channel)</th><th>Множитель</th></tr></thead>
        <tbody>
          <tr><td>Тепловая энергия (ST)</td><td><input id="ch_ST_id" type="text"></td><td><input id="ch_ST_factor" type="text" value="1"><div class="small-note">Сырое значение с прибора — Джоули. Множитель 1 = в ЭС уйдут джоули «как есть» (большие числа). Чтобы в ЭС сразу приходили готовые Гкал — поставьте <b>0.000000000238846</b> (это 1&nbsp;/&nbsp;4.1868e9 — точное определение Гкал в Дж).</div></td></tr>
          <tr><td>Масса (S)</td><td><input id="ch_S_id" type="text"></td><td><input id="ch_S_factor" type="text" value="1"><div class="small-note">Сырое значение с прибора — килограммы. Множитель 1 = в ЭС уйдут кг. Чтобы в ЭС приходили тонны (как в архиве МШ и в родном ПО прибора) — поставьте <b>0.001</b>.</div></td></tr>
          <tr><td>Температура (T)</td><td><input id="ch_T_id" type="text"></td><td><input id="ch_T_factor" type="text" value="1"><div class="small-note">Сырое значение уже в °C — множитель 1 (без пересчёта), как правило, верен, менять обычно не нужно.</div></td></tr>
          <tr><td>Давление (Pi)</td><td><input id="ch_Pi_id" type="text"></td><td><input id="ch_Pi_factor" type="text" value="1"><div class="small-note">Сырое значение с прибора — Паскали (Па). Множитель 1 = в ЭС уйдут Па. Для МПа поставьте <b>0.000001</b>; для кгс/см² — <b>0.0000101972</b>.</div></td></tr>
        </tbody>
      </table>
      <p><button class="btn" onclick="saveChannels()">Сохранить каналы</button></p>
      <div id="channelsMsg" class="msg"></div>
    </div>
  </div>

  <!-- ===================== ПОДКЛЮЧЕНИЕ К ЭС ===================== -->
  <div id="panel-esconn" class="panel">
    <div class="section">
      <h3>Подключение к БД Энергосферы (SQL Server) — для приборов с прямой записью в базу</h3>
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
      <div class="form-row"><label>Сдвиг времени для ВКМ (минут)</label><input id="es_time_shift" type="text" value="0"></div>
      <p class="small-note" style="margin-left:220px;margin-top:-8px;color:#ffd479;">⚠ Касается ТОЛЬКО приборов, данные которых мы пишем НАПРЯМУЮ в эту базу (сейчас это ВКМ). Приборов, чьи данные ЭС забирает сама через эмуляцию (сейчас это Akron), это не касается вообще — у них нет нашей прямой записи, значит и сдвигать нечего. Если в будущем появится новый тип прибора — смотрите, каким способом он подключён: прямая запись в базу — сдвиг актуален; эмуляция прибора для ЭС — не актуален.</p>
      <p class="small-note" style="margin-left:220px;margin-top:4px;">Сдвигает метку времени при записи данных ВКМ в базу ЭС. Нужен, потому что ЭС раскладывает такие прямые записи по своим строкам со смещением (наблюдалось смещение на 1,5 часа = -90). 0 — без сдвига. Подбирается опытным путём: сравните час в ЭС с часом в родной программе прибора.</p>
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
           вдалеке от общего обзора приборов. -->
    </div>
  </div>

  <!-- ===================== AKRON NORTHBOUND ===================== -->
  <div id="panel-akron" class="panel">
    <div class="section">
      <h3>Адрес приёма данных Акрона для Энергосферы</h3>
      <p class="small-note">ЭС сама подключается по этому адресу через свой драйвер АКРОН-01-1 (тип связи Raw TCP).</p>
      <div class="form-row"><label>Прибор</label>
        <select id="ak_device" onchange="loadAkronAddr()"></select>
      </div>
      <div id="ak_status_configured" style="display:none;background:#1e3d1e;border:1px solid #2d5a2d;color:#4caf50;padding:10px;border-radius:4px;margin-bottom:15px;">
        Адрес настроен: <span id="ak_status_addr"></span>
      </div>
      <div id="ak_status_not_configured" style="display:none;background:#3d1e1e;border:1px solid #5a2d2d;color:#f44336;padding:10px;border-radius:4px;margin-bottom:15px;">
        Адрес ещё НЕ настроен — заполните поле ниже и нажмите «Сохранить».
      </div>
      <div class="form-row"><label>Адрес (IP:порт)</label><input id="ak_addr" type="text"></div>
      <p class="small-note" style="margin-left:220px;margin-top:-8px;">например: 127.0.0.1:15021</p>
      <p><button class="btn" onclick="saveAkronAddr()">Сохранить</button></p>
      <div id="akronMsg" class="msg"></div>
    </div>
  </div>

  <!-- ===================== РўР•РљРЈР©РР• Р”РђРќРќР«Р• ===================== -->
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

  <!-- ===================== РђР РҐРР’ ===================== -->
  <div id="panel-archive" class="panel">
    <div class="section">
      <h3>Архив по прибору</h3>
      <div class="form-row"><label>Прибор</label>
        <select id="ar_device" onchange="onArchiveDeviceChange()"></select>
      </div>
      <div class="form-row"><label>Период</label>
        <input id="ar_from" type="text" readonly="readonly" style="width:150px;" placeholder="ГГГГ-ММ-ДД">
        &nbsp;—&nbsp;
        <input id="ar_to" type="text" readonly="readonly" style="width:150px;" placeholder="ГГГГ-ММ-ДД">
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
      <p class="small-note">Заново спрашивает прибор за указанный период и ПЕРЕЗАПИСЫВАЕТ уже сохранённые данные — используйте, если в архиве обнаружено заведомо неверное значение (например, из-за помехи на линии связи). Обычный дозабор такое не исправляет, поскольку строка для этого периода уже существует.</p>
      <div class="form-row"><label>Переопросить с</label>
        <input id="rl_from" type="text" readonly="readonly" style="width:120px;" placeholder="ГГГГ-ММ-ДД">
        <select id="rl_from_h" style="width:55px;"></select>:<select id="rl_from_m" style="width:55px;"><option value="00">00</option><option value="30">30</option></select>
      </div>
      <div class="form-row" id="rl_to_row"><label>По какую дату (только ВКМ)</label>
        <input id="rl_to" type="text" readonly="readonly" style="width:120px;" placeholder="ГГГГ-ММ-ДД">
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
      <p><button class="btn" onclick="saveSettings()">Сохранить</button></p>
      <div id="settingsMsg" class="msg"></div>
    </div>
  </div>

</div>

<script>
var allDevices = [];
var allProfiles = [];
var editingOriginalKind = null; // set by editDevice(), cleared by resetDeviceForm() — used to warn if the operator changes "Тип прибора" while editing an EXISTING device (root cause of the 2026-08-23 incident: switching kind mid-edit silently repurposed one device's saved row into a different device).

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
    if (current) { sel.value = current; } // preserve selection across a reload
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

  if (name === 'current') { populateDeviceSelect('cur_device', null); loadCurrentData(); }
  if (name === 'channels') { populateDeviceSelect('ch_device', 'vkm360'); }
  if (name === 'akron') { populateDeviceSelect('ak_device', 'akron'); }
  if (name === 'esconn') { loadESConnection(); }
  if (name === 'settings') { loadSettings(); }
  if (name === 'archive') { populateDeviceSelect('ar_device', null); setArchivePreset('week'); }
  if (name === 'dashboard') { loadDashboard(); }
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
    var ok = confirm('Вы редактируете существующий прибор и меняете его тип с "' + editingOriginalKind +
      '" на "' + kind + '". Это изменит СУЩЕСТВУЮЩИЙ прибор, а не создаст новый. Продолжить?');
    if (!ok) {
      document.getElementById('d_kind').value = editingOriginalKind;
      return;
    }
  }
  var tk = document.getElementById('d_transport_kind');
  var profileEl = document.getElementById('d_profile');
  if (kind === 'vkm360') {
    tk.value = 'modbus_tcp';
    if (!profileEl.value) { profileEl.value = 'profiles/vkm360.yaml'; }
  }
  if (kind === 'akron') {
    tk.value = 'rtu_serial';
    if (!profileEl.value) { profileEl.value = 'profiles/acron-01.yaml'; }
  }
  onTransportKindChange();
}
function onTransportKindChange() {
  var tk = document.getElementById('d_transport_kind').value;
  var showTCP = (tk === 'modbus_tcp' || tk === 'tcp_serial');
  var showSerial = (tk === 'rtu_serial' || tk === 'tcp_serial');
  document.getElementById('tcpFields').style.display = showTCP ? 'block' : 'none';
  document.getElementById('serialFields').style.display = showSerial ? 'block' : 'none';
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
function validateTransportFields(body) {
  if (body.transport_kind === 'modbus_tcp') {
    if (!body.host) { return 'Заполните поле "IP-адрес"'; }
  } else {
    if (!body.com) { return 'Заполните поле "COM-порт"'; }
  }
  return '';
}

function onArchiveDeviceChange() {
  var deviceId = document.getElementById('ar_device').value;
  var d = findDevice(deviceId);
  var showReload = !!(d && (d.kind === 'akron' || d.kind === 'vkm360'));
  document.getElementById('reloadSection').style.display = showReload ? 'block' : 'none';
  // поле "по" нужно только ВКМ (архив адресуется по времени напрямую) —
  // у Akron переопрос всегда идёт "с указанной даты и до сейчас"
  // (архив адресуется по индексу вглубь от текущей вершины, конкретную
  // верхнюю границу задать нельзя)
  var isVKM = !!(d && d.kind === 'vkm360');
  document.getElementById('rl_to_row').style.display = isVKM ? 'block' : 'none';

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
  var fromDate = document.getElementById('rl_from').value; // ГГГГ-ММ-ДД, из календаря
  var toDate = document.getElementById('rl_to').value;
  if (!deviceId) { showMsg('reloadMsg', false, 'Выберите прибор'); return; }
  if (!fromDate) { showMsg('reloadMsg', false, 'Выберите дату начала в календаре'); return; }
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
// syncNow — кнопка «Синхронизировать сейчас» напротив прибора на вкладке
// «Главная» (переехала сюда со вкладки «Подключение к ЭС», 2026-08-29 по
// прямому запросу оператора — раньше стояла отдельно от общего обзора
// приборов, с собственным выпадающим списком; теперь просто передаётся
// id конкретной строки таблицы) — просит уже работающий цикл es-sync
// конкретного прибора сделать внеплановый проход немедленно (сама
// логика на сервере не менялась, добавлена 2026-08-27).
function syncNow(deviceId) {
  document.getElementById('dashboardSyncMsg').className = 'msg';
  var xhr = new XMLHttpRequest();
  xhr.open('POST', '/api/es-sync/trigger', true);
  xhr.setRequestHeader('Content-Type', 'application/json');
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4) { return; }
    var data;
    try { data = JSON.parse(xhr.responseText); } catch (e) { showMsg('dashboardSyncMsg', false, 'Ошибка ответа сервера'); return; }
    if (data.ok) {
      showMsg('dashboardSyncMsg', true, deviceId + ': синхронизация запрошена — проверьте ЭС через несколько секунд.');
    } else {
      showMsg('dashboardSyncMsg', false, deviceId + ': ошибка — ' + (data.error || 'неизвестная'));
    }
  };
  xhr.send(JSON.stringify({ device_id: deviceId }));
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
      showMsg('reloadMsg', true, 'Идёт переопрос: обработано ' + data.done + ' из ' + data.total + ' периодов (' + pct + '%). Данные постепенно появляются в ЭС по ходу сбора...');
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
      showMsg('reloadMsg', true, 'Готово, перезаписано записей: ' + data.saved + '. Нажмите «Показать», чтобы увидеть обновлённые данные.');
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
}

function archiveQueryString() {
  var deviceId = document.getElementById('ar_device').value;
  var from = document.getElementById('ar_from').value;
  var to = document.getElementById('ar_to').value;
  var granularity = document.getElementById('ar_granularity').value;
  return 'device_id=' + encodeURIComponent(deviceId) +
    '&from=' + encodeURIComponent(from) +
    '&to=' + encodeURIComponent(to) +
    '&granularity=' + encodeURIComponent(granularity);
}

function loadArchiveTable() {
  var deviceId = document.getElementById('ar_device').value;
  if (!deviceId) { showMsg('archiveMsg', false, 'Выберите прибор'); return; }
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

function renderDashboardTable() {
  var rows = dashboardData.slice(); // копия — не трогаем исходный порядок с сервера
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

    var lastPeriodText = d.last_period || '—';
    var nextPollText = d.next_poll_at || '—';

    var actionCell = '';
    if (d.sync_supported) {
      actionCell = '<button class="btn secondary" onclick="syncNow(\'' + d.id + '\')">Синхронизировать сейчас</button>';
    }

    html += '<tr><td>' + (d.name || d.id) + ' (' + d.id + ')</td><td>' + d.kind + '</td><td>' +
      enabledText + '</td><td class="' + lagClass + '">' + lagText + '</td><td>' + lastPeriodText +
      '</td><td>' + nextPollText + '</td><td class="' + driftClass + '"' + driftTitle + '>' +
      driftText + '</td><td>' + actionCell + '</td></tr>';
  }
  if (html === '') { html = '<tr><td colspan="8">Приборов пока нет</td></tr>'; }
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
  };
  xhr.send();
}

function renderDevicesTable() {
  var rows = '';
  for (var i = 0; i < allDevices.length; i++) {
    var d = allDevices[i];
    var transportDesc = d.transport_kind === 'modbus_tcp' ? (d.host + ':' + d.port) : d.com;
    var enabledText = d.enabled ? '<span class="status-good">да</span>' : '<span class="status-bad">нет</span>';
    rows += '<tr><td>' + d.id + '</td><td>' + d.name + '</td><td>' + d.kind + '</td><td>' +
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

function editDevice(id) {
  var d = findDevice(id);
  if (!d) { return; }
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
  document.getElementById('d_com').value = d.com;
  document.getElementById('d_baudrate').value = d.baudrate || '';
  document.getElementById('d_parity').value = d.parity || 'none';
  document.getElementById('d_stopbits').value = d.stopbits || '';
  document.getElementById('d_unit_id').value = d.unit_id || '';
  document.getElementById('d_timeout_ms').value = d.timeout_ms || '';
  document.getElementById('d_retries').value = d.retries || '3';
  document.getElementById('d_current_poll_seconds').value = d.current_poll_seconds || '';
  document.getElementById('d_backfill_max_depth_hours').value = d.backfill_max_depth_hours || '0';
  // archive_at_minute: -1 в БД означает "не задано явно" (сентинел, см.
  // internal/web/api_devices.go) — показываем оператору сразу
  // реальное действующее значение (5), а не сырое "-1", чтобы не
  // пришлось разбираться, что оно значит.
  document.getElementById('d_archive_at_minute').value = (d.archive_at_minute !== undefined && d.archive_at_minute >= 0) ? d.archive_at_minute : '5';
  document.getElementById('d_enabled').checked = !!d.enabled;
  onTransportKindChange();
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
  document.getElementById('d_current_poll_seconds').value = '3600';
  document.getElementById('d_backfill_max_depth_hours').value = '0';
  document.getElementById('d_archive_at_minute').value = '5';
  document.getElementById('d_enabled').checked = true;
  onTransportKindChange();
  document.getElementById('probeMsg').className = 'msg';
  document.getElementById('deviceMsg').className = 'msg';
}

function currentDeviceFormAsJSON() {
  return {
    id: document.getElementById('d_id').value,
    name: document.getElementById('d_name').value,
    kind: document.getElementById('d_kind').value,
    profile: document.getElementById('d_profile').value,
    transport_kind: document.getElementById('d_transport_kind').value,
    host: document.getElementById('d_host').value,
    port: intOrZero(document.getElementById('d_port').value),
    com: document.getElementById('d_com').value,
    baudrate: intOrZero(document.getElementById('d_baudrate').value),
    parity: document.getElementById('d_parity').value,
    stopbits: intOrZero(document.getElementById('d_stopbits').value),
    timeout_ms: intOrZero(document.getElementById('d_timeout_ms').value),
    unit_id: intOrZero(document.getElementById('d_unit_id').value),
    retries: intOrZero(document.getElementById('d_retries').value),
    current_poll_seconds: intOrZero(document.getElementById('d_current_poll_seconds').value),
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
  var xhr = new XMLHttpRequest();
  xhr.open('POST', '/api/devices', true);
  xhr.setRequestHeader('Content-Type', 'application/json');
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4) { return; }
    if (xhr.status === 200) {
      showMsg('deviceMsg', true, 'Сохранено. Для применения запустите/перезапустите mbgw server.');
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
  var xhr = new XMLHttpRequest();
  xhr.open('POST', '/api/devices/probe', true);
  xhr.setRequestHeader('Content-Type', 'application/json');
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4) { return; }
    var data;
    try { data = JSON.parse(xhr.responseText); } catch (e) { showMsg('probeMsg', false, 'Ошибка ответа сервера'); return; }
    if (data.ok) {
      var text = 'Прибор отвечает.';
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
      showMsg('probeMsg', true, text);
    } else {
      showMsg('probeMsg', false, data.error || 'Не удалось опросить прибор');
    }
  };
  xhr.send(JSON.stringify(body));
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
  if (selectId === 'ak_device') { loadAkronAddr(); }
  if (selectId === 'ar_device') { onArchiveDeviceChange(); }
}

function loadChannels() {
  var deviceId = document.getElementById('ch_device').value;
  if (!deviceId) { return; }
  var xhr = new XMLHttpRequest();
  xhr.open('GET', '/api/vkm-channels?device_id=' + encodeURIComponent(deviceId), true);
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4 || xhr.status !== 200) { return; }
    var rows = JSON.parse(xhr.responseText) || [];
    var byTag = {};
    for (var i = 0; i < rows.length; i++) { byTag[rows[i].tag] = rows[i]; }
    var tags = ['ST', 'S', 'T', 'Pi'];
    for (var j = 0; j < tags.length; j++) {
      var t = tags[j];
      document.getElementById('ch_' + t + '_id').value = byTag[t] ? byTag[t].es_channel_id : '';
      document.getElementById('ch_' + t + '_factor').value = byTag[t] ? byTag[t].factor : '1';
    }
  };
  xhr.send();
}

function saveChannels(force) {
  var deviceId = document.getElementById('ch_device').value;
  if (!deviceId) { showMsg('channelsMsg', false, 'Выберите прибор'); return; }
  var tags = ['ST', 'S', 'T', 'Pi'];
  var channels = [];
  var channelIds = [];
  for (var i = 0; i < tags.length; i++) {
    var t = tags[i];
    var idVal = document.getElementById('ch_' + t + '_id').value;
    if (idVal === '') { continue; }
    var chId = intOrZero(idVal);
    channels.push({ tag: t, es_channel_id: chId, factor: floatOrOne(document.getElementById('ch_' + t + '_factor').value) });
    channelIds.push(chId);
  }

  if (force) {
    // проверка истории в ЭС уже пройдена (или пропущена оператором) на
    // предыдущем шаге — идём сразу к сохранению
    doSaveChannels(deviceId, channels, true);
    return;
  }

  // Сначала спрашиваем САМУ ЭС, нет ли в этих каналах уже чужой истории
  // (см. api_channel_check.go) — это ловит конфликт даже с точками,
  // которые вообще не настроены у нас самих, в отличие от проверки
  // внутри doSaveChannels (та знает только про наши собственные приборы).
  document.getElementById('channelsMsg').className = 'msg';
  var checkXhr = new XMLHttpRequest();
  checkXhr.open('POST', '/api/vkm-channels/check-history', true);
  checkXhr.setRequestHeader('Content-Type', 'application/json');
  checkXhr.onreadystatechange = function() {
    if (checkXhr.readyState !== 4) { return; }
    var data = {};
    try { data = JSON.parse(checkXhr.responseText); } catch (e) {}

    if (checkXhr.status === 200 && data.checked === false) {
      // проверка не смогла выполниться (например, подключение к ЭС не
      // настроено) — не блокируем сохранение, просто предупреждаем
      if (!confirm('Не удалось проверить каналы напрямую в ЭС (' + (data.reason || 'причина неизвестна') +
        '). Продолжить сохранение без этой проверки?')) {
        showMsg('channelsMsg', false, 'Сохранение отменено.');
        return;
      }
      doSaveChannels(deviceId, channels, false);
      return;
    }

    if (checkXhr.status === 200 && data.occupied && data.occupied.length > 0) {
      var msg = 'ВНИМАНИЕ: в ЭС уже есть данные в этих каналах — похоже, они заняты другим прибором:\n';
      for (var j = 0; j < data.occupied.length; j++) {
        var o = data.occupied[j];
        msg += '  канал ' + o.channel_id + ': ' + o.row_count + ' записей, с ' + o.oldest + ' по ' + o.newest + '\n';
      }
      msg += '\nСохранить всё равно? Это может испортить данные другого прибора в ЭС!';
      if (!confirm(msg)) {
        showMsg('channelsMsg', false, 'Сохранение отменено — номер канала совпадает с уже используемым в ЭС.');
        return;
      }
    }

    doSaveChannels(deviceId, channels, false);
  };
  checkXhr.send(JSON.stringify({ channel_ids: channelIds }));
}

function doSaveChannels(deviceId, channels, force) {
  var xhr = new XMLHttpRequest();
  xhr.open('POST', '/api/vkm-channels', true);
  xhr.setRequestHeader('Content-Type', 'application/json');
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4) { return; }
    if (xhr.status === 200) {
      showMsg('channelsMsg', true, 'Каналы сохранены.');
    } else if (xhr.status === 409) {
      // конфликт номеров каналов с ДРУГИМ НАШИМ прибором — показываем
      // предупреждение и даём явно подтвердить сохранение всё равно
      var data = {};
      try { data = JSON.parse(xhr.responseText); } catch (e) {}
      var msg = (data.error || 'Обнаружен конфликт каналов.') + ' Сохранить всё равно?';
      if (confirm(msg)) {
        doSaveChannels(deviceId, channels, true);
      } else {
        showMsg('channelsMsg', false, 'Сохранение отменено — исправьте номер канала.');
      }
    } else {
      showMsg('channelsMsg', false, 'Ошибка: HTTP ' + xhr.status);
    }
  };
  xhr.send(JSON.stringify({ device_id: deviceId, channels: channels, force: !!force }));
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

function loadAkronAddr() {
  var deviceId = document.getElementById('ak_device').value;
  if (!deviceId) { return; }
  var xhr = new XMLHttpRequest();
  xhr.open('GET', '/api/akron-northbound?device_id=' + encodeURIComponent(deviceId), true);
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4 || xhr.status !== 200) { return; }
    var data = JSON.parse(xhr.responseText);
    var addr = data.listen_addr || '';
    document.getElementById('ak_addr').value = addr;
    var configured = !!addr;
    document.getElementById('ak_status_configured').style.display = configured ? 'block' : 'none';
    document.getElementById('ak_status_not_configured').style.display = configured ? 'none' : 'block';
    if (configured) { document.getElementById('ak_status_addr').innerText = addr; }
  };
  xhr.send();
}

function saveAkronAddr() {
  var deviceId = document.getElementById('ak_device').value;
  var addr = document.getElementById('ak_addr').value;
  if (!deviceId || !addr) { showMsg('akronMsg', false, 'Выберите прибор и укажите адрес'); return; }
  var xhr = new XMLHttpRequest();
  xhr.open('POST', '/api/akron-northbound', true);
  xhr.setRequestHeader('Content-Type', 'application/json');
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4) { return; }
    if (xhr.status === 200) { showMsg('akronMsg', true, 'Сохранено.'); }
    else { showMsg('akronMsg', false, 'Ошибка: HTTP ' + xhr.status); }
  };
  xhr.send(JSON.stringify({ device_id: deviceId, listen_addr: addr }));
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
  'year': 'Год (часы прибора)'
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

    var conflictBox = document.getElementById('s_port_conflict');
    if (s.actual_port && s.actual_port !== s.configured_port) {
      document.getElementById('s_port_actual').innerText = s.actual_port;
      document.getElementById('s_port_actual2').innerText = s.actual_port;
      document.getElementById('s_port_configured_repeat').innerText = s.configured_port;
      conflictBox.style.display = 'block';
    } else {
      conflictBox.style.display = 'none';
    }

    var note = 'Смена порта применяется сразу, без перезапуска. Отладочный лог тоже применяется сразу.';
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
  var body = { configured_port: port, debug_log_enabled: document.getElementById('s_debug').checked };
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
// включая IE, и не требует от оператора вводить дату руками вообще: поля
// теперь readonly, выбор — только кликом по дню в календаре.

var calendarPopupEl = null;
var calendarActiveInputId = null;

var monthNamesRu = ['Январь', 'Февраль', 'Март', 'Апрель', 'Май', 'Июнь', 'Июль', 'Август', 'Сентябрь', 'Октябрь', 'Ноябрь', 'Декабрь'];
var weekDaysRu = ['Пн', 'Вт', 'Ср', 'Чт', 'Пт', 'Сб', 'Вс'];

function parseISODate(s) {
  var parts = s.split('-');
  return new Date(parseInt(parts[0], 10), parseInt(parts[1], 10) - 1, parseInt(parts[2], 10));
}

// attachCalendar делает поле нажимаемым — клик открывает всплывающий
// календарь под этим полем. Вызывается один раз при загрузке страницы
// для каждого из полей дат.
function attachCalendar(inputId) {
  var input = document.getElementById(inputId);
  if (!input) { return; }
  input.style.cursor = 'pointer';
  input.style.background = '#3c3c3c';
  input.onclick = function() { toggleCalendarPopup(inputId); };
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
  html += '<div style="display:table-cell;text-align:left;"><button type="button" style="background:#3c3c3c;color:#fff;border:none;padding:4px 10px;cursor:pointer;" onclick="calendarChangeMonth(' + year + ',' + month + ',-1)">&lt;</button></div>';
  html += '<div style="display:table-cell;text-align:center;color:#fff;font-size:13px;">' + monthNamesRu[month] + ' ' + year + '</div>';
  html += '<div style="display:table-cell;text-align:right;"><button type="button" style="background:#3c3c3c;color:#fff;border:none;padding:4px 10px;cursor:pointer;" onclick="calendarChangeMonth(' + year + ',' + month + ',1)">&gt;</button></div>';
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

function calendarChangeMonth(year, month, delta) {
  month += delta;
  if (month < 0) { month = 11; year -= 1; }
  if (month > 11) { month = 0; year += 1; }
  renderCalendarMonth(year, month);
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
populateHourSelect('rl_from_h');
populateHourSelect('rl_to_h');

attachCalendar('rl_from');
attachCalendar('rl_to');

loadDevices();
loadProfiles();
resetDeviceForm();
loadDashboard();
// Автообновление вкладки «Главная» — раз в 30с, независимо от того,
// какая вкладка сейчас открыта (дёшево: один маленький GET-запрос), так
// что оператор видит актуальную картину сразу при переключении на неё,
// без ожидания. Тот же интервал, что у старого диагностического
// дашборда на / (см. handleDashboard в server.go, setInterval(loadData,
// 30000)) — уже проверенное на практике значение для этого проекта.
setInterval(loadDashboard, 30000);
</script>
</body>
</html>`

package web

import "net/http"

// api_admin_ui.go serves the T14 minimal-slice admin UI at /admin — a
// single self-contained ES5 HTML page (XMLHttpRequest, var, string
// concatenation — see handleDashboard's doc comment on why: IE on
// Windows Server 2008 R2/2012 has neither Fetch nor async/await, and this
// project builds no separate frontend toolchain). It talks only to the
// JSON API already implemented in api_devices.go/api_probe.go.
//
// Deliberately served at /admin, not /: the existing read-only dashboard
// at / (handleDashboard) is untouched and keeps working exactly as
// before — this is a new, additive screen, not a replacement, so nothing
// that already works can be broken by it.
//
// Layout: one HTML page, five tab panels toggled by show/hide (no
// client-side router, no page reloads):
//  1. Приборы       — list + add/edit form (incl. "Проверить прибор" ->
//     POST /api/devices/probe) + delete.
//  2. Каналы ЭС     — per-device 4-row (ST/S/T/Pi) tag->channel+factor
//     table, only meaningful for kind=vkm360 devices.
//  3. Подключение к ЭС — single SQL Server connection form + "Проверить
//     подключение" (POST /api/es-connection/test, saves nothing).
//  4. Akron northbound — per-device listen address, only meaningful for
//     kind=akron devices.
//  5. Текущие данные — same read-only table the / dashboard shows,
//     reusing GET /api/current, so the operator doesn't need to flip
//     between /admin and / to see both configuration and live data.
func (s *Server) handleAdminUI(w http.ResponseWriter, r *http.Request) {
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
  <button class="tab-btn active" onclick="showTab('devices')">Приборы</button>
  <button class="tab-btn" onclick="showTab('channels')">Каналы ЭС</button>
  <button class="tab-btn" onclick="showTab('esconn')">Подключение к ЭС</button>
  <button class="tab-btn" onclick="showTab('akron')">Akron northbound</button>
  <button class="tab-btn" onclick="showTab('settings')">Настройки</button>
  <button class="tab-btn" onclick="showTab('current')">Текущие данные</button>
</div>

<div class="content">

  <!-- ===================== ПРИБОРЫ ===================== -->
  <div id="panel-devices" class="panel active">
    <div class="section">
      <h3>Список приборов</h3>
      <table>
        <thead><tr><th>ID</th><th>Название</th><th>Тип</th><th>Транспорт</th><th>Включён</th><th></th></tr></thead>
        <tbody id="devicesTable"><tr><td colspan="6">Загрузка...</td></tr></tbody>
      </table>
    </div>

    <div class="section">
      <h3 id="deviceFormTitle">Добавить прибор</h3>
      <div class="form-row"><label>Название</label><input id="d_name" type="text" onkeyup="autoFillID()"></div>
      <p class="small-note" style="margin-left:220px;margin-top:-8px;">ID: <span id="d_id_display">—</span> <input id="d_id" type="text" style="display:none;"></p>
      <div class="form-row"><label>Тип прибора</label>
        <select id="d_kind" onchange="onKindChange()">
          <option value="vkm360">ВКМ-360</option>
          <option value="akron">Акрон</option>
        </select>
      </div>
      <div class="form-row"><label>Путь к профилю</label><input id="d_profile" type="text" placeholder="profiles/acron-01.yaml"></div>
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
        <p class="small-note">Как часто опрашивать мгновенные показания (не архив). Раз в час обычно достаточно — этот шлюз собирает архив, не ведёт непрерывную телеметрию.</p>
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
          <tr><td>Тепловая энергия (ST)</td><td><input id="ch_ST_id" type="text"></td><td><input id="ch_ST_factor" type="text" value="1"></td></tr>
          <tr><td>Масса (S)</td><td><input id="ch_S_id" type="text"></td><td><input id="ch_S_factor" type="text" value="1"></td></tr>
          <tr><td>Температура (T)</td><td><input id="ch_T_id" type="text"></td><td><input id="ch_T_factor" type="text" value="1"></td></tr>
          <tr><td>Давление (Pi)</td><td><input id="ch_Pi_id" type="text"></td><td><input id="ch_Pi_factor" type="text" value="1"></td></tr>
        </tbody>
      </table>
      <p><button class="btn" onclick="saveChannels()">Сохранить каналы</button></p>
      <div id="channelsMsg" class="msg"></div>
    </div>
  </div>

  <!-- ===================== ПОДКЛЮЧЕНИЕ К ЭС ===================== -->
  <div id="panel-esconn" class="panel">
    <div class="section">
      <h3>Подключение к БД Энергосферы (SQL Server)</h3>
      <div class="form-row"><label>Сервер</label><input id="es_server" type="text" placeholder="localhost"></div>
      <div class="form-row"><label>База данных</label><input id="es_database" type="text" placeholder="CSD_Astrakhan"></div>
      <div class="form-row"><label>Логин</label><input id="es_user" type="text" placeholder="AdminBaz"></div>
      <div class="form-row"><label>Пароль</label><input id="es_password" type="password" placeholder="(введите пароль)"></div>
      <div class="form-row"><label>Порт</label><input id="es_port" type="text" placeholder="1433"></div>
      <p class="small-note" id="es_password_note"></p>
      <p>
        <button class="btn secondary" onclick="testESConnection()">Проверить подключение</button>
        <button class="btn" onclick="saveESConnection()">Сохранить</button>
      </p>
      <div id="esTestMsg" class="msg"></div>
      <div id="esSaveMsg" class="msg"></div>
    </div>
  </div>

  <!-- ===================== AKRON NORTHBOUND ===================== -->
  <div id="panel-akron" class="panel">
    <div class="section">
      <h3>Адрес отдачи данных для Akron (northbound)</h3>
      <p class="small-note">ЭС сама подключается по этому адресу через свой драйвер АКРОН-01-1 (тип связи Raw TCP).</p>
      <div class="form-row"><label>Прибор</label>
        <select id="ak_device" onchange="loadAkronAddr()"></select>
      </div>
      <div class="form-row"><label>Адрес (host:port)</label><input id="ak_addr" type="text" placeholder="127.0.0.1:15021"></div>
      <p><button class="btn" onclick="saveAkronAddr()">Сохранить</button></p>
      <div id="akronMsg" class="msg"></div>
    </div>
  </div>

  <!-- ===================== ТЕКУЩИЕ ДАННЫЕ ===================== -->
  <div id="panel-current" class="panel">
    <div class="section">
      <h3>Текущие данные</h3>
      <table>
        <thead><tr><th>Device ID</th><th>Point ID</th><th>Instance</th><th>Value</th><th>Unit</th><th>Quality</th><th>Time</th></tr></thead>
        <tbody id="currentData"><tr><td colspan="7">Загрузка...</td></tr></tbody>
      </table>
    </div>
  </div>

  <!-- ===================== НАСТРОЙКИ ===================== -->
  <div id="panel-settings" class="panel">
    <div class="section">
      <h3>Настройки</h3>
      <div class="form-row"><label>Порт веб-интерфейса</label><input id="s_port" type="text" placeholder="8080"></div>
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

function showTab(name) {
  var panels = document.getElementsByClassName('panel');
  for (var i = 0; i < panels.length; i++) { panels[i].className = 'panel'; }
  document.getElementById('panel-' + name).className = 'panel active';

  var btns = document.getElementsByClassName('tab-btn');
  for (var j = 0; j < btns.length; j++) { btns[j].className = 'tab-btn'; }
  event.target.className = 'tab-btn active';

  if (name === 'current') { loadCurrentData(); }
  if (name === 'channels') { populateDeviceSelect('ch_device', 'vkm360'); }
  if (name === 'akron') { populateDeviceSelect('ak_device', 'akron'); }
  if (name === 'esconn') { loadESConnection(); }
  if (name === 'settings') { loadSettings(); }
}

function showMsg(elId, ok, text) {
  var el = document.getElementById(elId);
  el.className = 'msg ' + (ok ? 'ok' : 'err');
  el.innerText = text;
}

function onKindChange() {
  var kind = document.getElementById('d_kind').value;
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

// autoFillID keeps a hidden d_id field (the actual value submitted to the
// API) in sync with the visible "Название" field, so the operator never
// has to think about or type an ID by hand — see this file's other
// comments on the 2026-08-23 placeholder-vs-value incident that prompted
// simplifying this form wherever possible. Only auto-fills for a NEW
// device (d_id not disabled); editDevice() disables it for existing
// devices, whose ID must never change once saved (it's how es_vkm_channels/
// es_akron_northbound rows reference the device).
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

// validateTransportFields checks the ONE field that has no sensible
// default and genuinely must be typed by hand (COM port for serial
// transports, IP host for TCP transports) — returns an error message, or
// '' if OK. This exists because of a real incident (2026-08-23): fields
// showing example text via the HTML placeholder attribute LOOK filled in
// a screenshot but are actually empty strings until the operator clicks
// in and types something themselves — sending that silently to the
// server produced a confusing downstream transport error instead of a
// clear "you forgot to fill this in" message right where the mistake was
// made. Every OTHER field in the form now has a real default VALUE (not
// just a placeholder hint), so this check only needs to cover the two
// fields that cannot have a sensible default filled in automatically.
function validateTransportFields(body) {
  if (body.transport_kind === 'modbus_tcp') {
    if (!body.host) { return 'Заполните поле "IP-адрес"'; }
  } else {
    if (!body.com) { return 'Заполните поле "COM-порт"'; }
  }
  return '';
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
  document.getElementById('d_enabled').checked = !!d.enabled;
  onTransportKindChange();
  showTab('devices');
  window.scrollTo(0, document.body.scrollHeight);
}

function resetDeviceForm() {
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
    backfill_max_depth_hours: 0,
    gap_scan_window_hours: 0,
    archive_at_minute: -1,
    enabled: document.getElementById('d_enabled').checked
  };
}

function saveDevice() {
  var body = currentDeviceFormAsJSON();
  if (!body.id) { showMsg('deviceMsg', false, 'Заполните поле "Название"'); return; }
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
  for (var i = 0; i < allDevices.length; i++) {
    if (allDevices[i].kind === kindFilter) {
      html += '<option value="' + allDevices[i].id + '">' + allDevices[i].name + ' (' + allDevices[i].id + ')</option>';
    }
  }
  if (html === '') { html = '<option value="">— нет приборов типа ' + kindFilter + ' —</option>'; }
  sel.innerHTML = html;
  if (selectId === 'ch_device') { loadChannels(); }
  if (selectId === 'ak_device') { loadAkronAddr(); }
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

function saveChannels() {
  var deviceId = document.getElementById('ch_device').value;
  if (!deviceId) { showMsg('channelsMsg', false, 'Выберите прибор'); return; }
  var tags = ['ST', 'S', 'T', 'Pi'];
  var channels = [];
  for (var i = 0; i < tags.length; i++) {
    var t = tags[i];
    var idVal = document.getElementById('ch_' + t + '_id').value;
    if (idVal === '') { continue; }
    channels.push({ tag: t, es_channel_id: intOrZero(idVal), factor: floatOrOne(document.getElementById('ch_' + t + '_factor').value) });
  }
  var xhr = new XMLHttpRequest();
  xhr.open('POST', '/api/vkm-channels', true);
  xhr.setRequestHeader('Content-Type', 'application/json');
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4) { return; }
    if (xhr.status === 200) { showMsg('channelsMsg', true, 'Каналы сохранены.'); }
    else { showMsg('channelsMsg', false, 'Ошибка: HTTP ' + xhr.status); }
  };
  xhr.send(JSON.stringify({ device_id: deviceId, channels: channels }));
}

function loadESConnection() {
  var xhr = new XMLHttpRequest();
  xhr.open('GET', '/api/es-connection', true);
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4 || xhr.status !== 200) { return; }
    var c = JSON.parse(xhr.responseText);
    document.getElementById('es_server').value = c.sql_server || '';
    document.getElementById('es_database').value = c.sql_database || '';
    document.getElementById('es_user').value = c.sql_user || '';
    document.getElementById('es_port').value = c.sql_port || '';
    document.getElementById('es_password_note').innerText = c.password_set ?
      'Пароль уже сохранён (не показывается). Введите новый, только если хотите его изменить.' :
      'Пароль ещё не задан.';
  };
  xhr.send();
}

function esConnectionFormAsJSON() {
  return {
    sql_server: document.getElementById('es_server').value,
    sql_database: document.getElementById('es_database').value,
    sql_user: document.getElementById('es_user').value,
    sql_password: document.getElementById('es_password').value,
    sql_port: intOrZero(document.getElementById('es_port').value)
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
    if (data.ok) { showMsg('esTestMsg', true, 'Подключение успешно.'); }
    else { showMsg('esTestMsg', false, 'Ошибка: ' + data.error); }
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
    document.getElementById('ak_addr').value = data.listen_addr || '';
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

function loadCurrentData() {
  var xhr = new XMLHttpRequest();
  xhr.open('GET', '/api/current', true);
  xhr.onreadystatechange = function() {
    if (xhr.readyState !== 4 || xhr.status !== 200) { return; }
    var data = JSON.parse(xhr.responseText) || [];
    var rows = '';
    for (var i = 0; i < data.length; i++) {
      var r = data[i];
      var t = r.Timestamp ? new Date(r.Timestamp).toLocaleTimeString('ru-RU') : '-';
      var qClass = r.Quality === 'VALID' ? 'status-good' : 'status-bad';
      rows += '<tr><td>' + r.DeviceID + '</td><td>' + r.PointID + '</td><td>' + r.Instance +
        '</td><td>' + r.Value + '</td><td>' + r.Unit + '</td><td class="' + qClass + '">' +
        r.Quality + '</td><td>' + t + '</td></tr>';
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
    var note = 'Настроенный порт: ' + s.configured_port + '.';
    if (s.actual_port && s.actual_port !== s.configured_port) {
      note += ' ВНИМАНИЕ: сервер сейчас фактически работает на порту ' + s.actual_port +
        ' (настроенный порт был занят при запуске).';
    }
    note += ' Изменение вступит в силу после перезапуска mbgw server (или службы mbgw_service).';
    document.getElementById('s_port_note').innerText = note;
  };
  xhr.send();
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
      showMsg('settingsMsg', true, 'Сохранено. Изменения вступят в силу после перезапуска mbgw server (или службы mbgw_service).');
      loadSettings();
    } else {
      showMsg('settingsMsg', false, 'Ошибка: HTTP ' + xhr.status);
    }
  };
  xhr.send(JSON.stringify(body));
}

loadDevices();
resetDeviceForm();
</script>
</body>
</html>`

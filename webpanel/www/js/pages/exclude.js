import { apiGet, apiPost, apiPostText, errHtml, toastErr } from "../core/api.js";
import { $app, _icons, escapeHtml, skeletonLines } from "../core/dom.js";
import { _newLoad, _stale } from "../core/loadorder.js";
import { confirmModal } from "../job.js";
import { whitelistRows, replaceWhitelistSelection, selectWhitelistRange } from "../core/whitelist.js";
import { toast } from "../core/toast.js";

//
// Два способа сказать «сюда не лезь», и они разные по существу, а не по
// удобству. Домен исключается по имени и только там, где имя вообще видно в
// запросе. Адрес исключается по получателю пакета — поэтому работает и там,
// где имени нет: камеры, домофоны, звонки, игры.
//
// Одна страница, две подвкладки — тем же приёмом, что «Стратегии»: подвкладка
// это адрес, значит на неё можно сослаться и она переживает перезагрузку
// страницы. Маршруты остались историческими (#/whitelist — «Домены»,
// #/exclude — «Адреса»), чтобы старые закладки открывали то же содержимое.
const EXCLUDE_TABS = [
  { id: "domains", route: "whitelist", label: "Домены",
    hint: "Исключить сайт по его имени — сразу со всеми поддоменами" },
  { id: "addresses", route: "exclude", label: "Адреса",
    hint: "Исключить по адресу получателя — там, где имени в запросе нет: камеры, домофоны, звонки" },
];

function excludeShell(activeId, bodyHtml) {
  const tabs = EXCLUDE_TABS.map(t => `
    <a href="#/${t.route}" class="strat-tab${t.id === activeId ? " active" : ""}"
       role="tab" aria-selected="${t.id === activeId}" title="${escapeHtml(t.hint)}">
      ${escapeHtml(t.label)}
    </a>`).join("");
  const active = EXCLUDE_TABS.find(t => t.id === activeId) || EXCLUDE_TABS[0];
  return `
    <h1 class="page-title">Исключения</h1>
    <div class="strat-tabs" role="tablist" aria-label="Виды исключений">${tabs}</div>
    <p class="desc strat-tabhint">${escapeHtml(active.hint)}</p>
    ${bodyHtml}
  `;
}

export async function renderExcludeAddresses() {
  $app.innerHTML = excludeShell("addresses", `
    <div class="card">
      <h3>Не трогать эти адреса</h3>
      <p class="desc">
        Всё, что идёт на перечисленные здесь адреса, z2k пропускает как есть —
        как будто обход для них выключен. Исключение работает по адресу
        получателя, поэтому помогает и там, где имени сайта в запросе нет
        вообще: камеры и домофоны, звонки и видеосвязь, игры и обмен данными
        между устройствами напрямую.
      </p>
      <p class="desc">
        Вписывать нужно <b>адрес</b> (например <code>203.0.113.7</code>) или
        <b>подсеть</b> (например <code>203.0.113.0/24</code>). Работает сразу
        и остаётся в силе после перезагрузки роутера. Имя сайта здесь не
        сработает — для него вкладка <a href="#/whitelist">«Домены»</a>.
      </p>
      <p class="desc">
        Локальная сеть (192.168.x, 10.x, 172.16–31.x и подобные) исключена
        всегда и без этого списка — добавлять её сюда не нужно.
      </p>
      <div class="wl-add">
        <label class="field">
          <span class="field-label">Адрес или подсеть</span>
          <input id="ex-input" type="text" placeholder="203.0.113.7 или 203.0.113.0/24"
                 inputmode="url" autocomplete="off" autocapitalize="off"
                 spellcheck="false" autocorrect="off">
        </label>
        <button class="btn btn-primary" id="ex-add-btn">Добавить</button>
      </div>
      <ul class="wl-list" id="ex-list">${skeletonLines(5)}</ul>
    </div>
    <div id="ex-legacy"></div>
  `);
  document.getElementById("ex-add-btn").addEventListener("click", exAdd);
  document.getElementById("ex-input").addEventListener("keydown", e => {
    if (e.key === "Enter") exAdd();
  });
  loadExclude();
}

async function loadExclude() {
  const list = document.getElementById("ex-list");
  const seq = _newLoad("exclude");
  try {
    const d = await apiGet("/exclude");
    if (_stale("exclude", seq)) return;
    const entries = d.entries || [];
    if (!entries.length) {
      list.innerHTML = `<li style="color:var(--text-muted)">(пусто)</li>`;
    } else {
      list.innerHTML = entries.map(en => `
        <li><span>${escapeHtml(en)}</span><button class="btn-icon" title="Удалить" aria-label="Удалить ${escapeHtml(en)}" data-del="${escapeHtml(en)}">${_icons.close}</button></li>
      `).join("");
      list.querySelectorAll("button[data-del]").forEach(btn => {
        btn.addEventListener("click", () => exDelete(btn.dataset.del));
      });
    }
    renderExcludeLegacy(d.legacy_domains || []);
  } catch (e) {
    if (_stale("exclude", seq)) return;
    list.innerHTML = `<li style="color:var(--bad)">${errHtml(e)}</li>`;
  }
}

// Имена сайтов, осевшие в адресном списке, пока панель их сюда принимала.
// Они не действовали ни дня, но и молча прятать их нельзя — человек вписывал
// их осознанно и считает, что они работают. Блок появляется только когда
// такие записи есть.
function renderExcludeLegacy(domains) {
  const box = document.getElementById("ex-legacy");
  if (!box) return;
  if (!domains.length) { box.innerHTML = ""; return; }
  box.innerHTML = `
    <div class="card">
      <h3>Эти записи ничего не делают</h3>
      <p class="desc">
        Раньше сюда можно было вписать и имя сайта. По имени здесь ничего не
        исключается, поэтому такие записи просто лежат в списке и ни на что
        не влияют. Чтобы они заработали, добавьте их на вкладке
        <a href="#/whitelist">«Домены»</a>, а отсюда удалите.
      </p>
      <ul class="wl-list" id="ex-legacy-list">${domains.map(dom => `
        <li><span>${escapeHtml(dom)}</span><button class="btn-icon" title="Удалить" aria-label="Удалить ${escapeHtml(dom)}" data-del="${escapeHtml(dom)}">${_icons.close}</button></li>
      `).join("")}</ul>
    </div>
  `;
  box.querySelectorAll("button[data-del]").forEach(btn => {
    btn.addEventListener("click", () => exDelete(btn.dataset.del));
  });
}

async function exAdd() {
  const inp = document.getElementById("ex-input");
  const entry = inp.value.trim();
  if (!entry) return;
  try {
    await apiPost("/exclude/add", { entry });
    inp.value = "";
    toast("Добавлено");
    loadExclude();
  } catch (e) {
    toastErr("Ошибка: ", e);
  }
}

async function exDelete(entry) {
  try {
    await apiPost("/exclude/delete", { entry });
    toast("Удалено");
    loadExclude();
  } catch (e) {
    toastErr("Ошибка: ", e);
  }
}

let wlView = null;

export async function renderExcludeDomains() {
  $app.innerHTML = excludeShell("domains", `
    <div class="card" id="wl-card">
      <h3>Не трогать эти сайты</h3>
      <p class="desc">z2k пропускает эти сайты без обработки. <code>example.com</code>
        включает все его поддомены. Для IP-адресов используйте вкладку <a href="#/exclude">«Адреса»</a>.</p>
      <p class="desc">Изменения действуют через несколько секунд, без перезапуска сервиса.</p>
      <div class="wl-add">
        <label class="field"><span class="field-label">Новый сайт</span>
          <input id="wl-input" type="text" placeholder="example.com" inputmode="url"
            autocomplete="off" autocapitalize="off" spellcheck="false"></label>
        <button class="btn btn-primary" id="wl-add-btn">Добавить</button>
        <button class="btn" id="wl-import-btn">Импорт из файла</button>
        <input type="file" id="wl-import-file" accept=".txt,text/plain" hidden>
      </div>
      <div class="wl-tools">
        <label class="field wl-search"><span class="field-label">Поиск в списке</span>
          <input id="wl-search" type="search" placeholder="Имя или часть имени" autocomplete="off"></label>
        <button class="btn" id="wl-edit-all">Редактировать весь список</button>
        <button class="btn" id="wl-refresh">Обновить список</button>
      </div>
      <div class="wl-selection">
        <button class="btn" id="wl-select">Выбрать найденные</button>
        <button class="btn" id="wl-unselect">Снять выделение</button>
        <span id="wl-count" role="status" aria-live="polite"></span>
      </div>
      <div class="wl-actions">
        <button class="btn" id="wl-edit-selected">Редактировать выбранные</button>
        <button class="btn wl-danger" id="wl-delete-selected">Удалить выбранные</button>
        <button class="btn wl-danger" id="wl-clear">Очистить список</button>
        <button class="btn" id="wl-undo" hidden>Отменить последнее изменение</button>
      </div>
      <p class="wl-error" id="wl-error" role="alert" hidden></p>
      <ul class="wl-list wl-bulk-list" id="wl-list" aria-label="Домены-исключения">${skeletonLines(5)}</ul>
      <p class="desc wl-tip">Флажки выбирают записи. Shift + клик выделяет диапазон в найденных строках.</p>
      <section class="wl-editor" id="wl-editor" hidden aria-labelledby="wl-editor-title">
        <h3 id="wl-editor-title"></h3>
        <p class="desc" id="wl-editor-hint"></p>
        <label class="field"><span class="field-label">Один домен на строку</span>
          <textarea id="wl-editor-text" rows="12" spellcheck="false" autocapitalize="off"></textarea></label>
        <p class="wl-error" id="wl-editor-error" role="alert" hidden></p>
        <div class="wl-actions">
          <button class="btn btn-primary" id="wl-save">Сохранить</button>
          <button class="btn" id="wl-cancel">Отмена</button>
        </div>
      </section>
    </div>
  `);
  const view = { root: document.getElementById("wl-card"), text: "", revision: "", rows: [],
    selected: new Set(), anchor: null, busy: false, loaded: false, editor: null, undo: null };
  wlView = view;
  const on = (id, event, fn) => wlEl(view, id).addEventListener(event, fn);
  on("wl-search", "input", () => { view.anchor = null; wlDraw(view); });
  on("wl-select", "click", () => { wlVisible(view).forEach(r => view.selected.add(r.id)); wlDraw(view); });
  on("wl-unselect", "click", () => { view.selected.clear(); view.anchor = null; wlDraw(view); });
  on("wl-edit-all", "click", () => wlEdit(view, false));
  on("wl-edit-selected", "click", () => wlEdit(view, true));
  on("wl-cancel", "click", () => { view.editor = null; wlEl(view,"wl-editor").hidden = true; wlError(view); wlControls(view); });
  on("wl-save", "click", () => wlSaveEditor(view));
  on("wl-delete-selected", "click", () => wlRemove(view, false));
  on("wl-clear", "click", () => wlRemove(view, true));
  on("wl-refresh", "click", () => loadWhitelist(view));
  on("wl-undo", "click", () => {
    if (view.undo) wlCommit(view, view.undo.text, view.undo.revision, false);
  });
  on("wl-add-btn", "click", () => wlAdd(view));
  on("wl-input", "keydown", e => { if(e.key === "Enter") wlAdd(view); });
  on("wl-import-btn", "click", () => wlEl(view,"wl-import-file").click());
  on("wl-import-file", "change", e => wlImport(view,e));
  wlControls(view);
  await loadWhitelist(view);
}

function wlEl(view, id) { return view.root.querySelector("#" + id); }
function wlCurrent(view) { return wlView === view && view.root.isConnected; }
function wlVisible(view) {
  const query = wlEl(view,"wl-search").value.trim().toLowerCase();
  return view.rows.filter(r => r.domain.toLowerCase().includes(query));
}
function wlError(view, message = "") {
  const el = wlEl(view,"wl-error"); el.textContent = message; el.hidden = !message || !!view.editor;
  const editorError = wlEl(view,"wl-editor-error");
  editorError.textContent = message; editorError.hidden = !message || !view.editor;
  if(message) (view.editor ? editorError : el).scrollIntoView({ block: "nearest" });
}
function wlControls(view) {
  const blocked = view.busy || !view.loaded;
  view.root.setAttribute("aria-busy", String(view.busy));
  for (const el of view.root.querySelectorAll("button, input, textarea")) el.disabled = blocked || !!view.editor;
  for (const id of ["wl-save","wl-cancel","wl-editor-text"]) wlEl(view,id).disabled = blocked;
  if (!blocked && !view.editor) {
    for (const id of ["wl-edit-selected","wl-delete-selected","wl-unselect"]) wlEl(view,id).disabled = !view.selected.size;
    wlEl(view,"wl-select").disabled = !wlVisible(view).length;
    wlEl(view,"wl-clear").disabled = !view.rows.length;
  }
  wlEl(view,"wl-undo").hidden = !view.undo;
  // A failed initial load must still be recoverable.
  wlEl(view,"wl-refresh").disabled = view.busy || !!view.editor;
}
function wlDraw(view) {
  const visible = wlVisible(view);
  wlEl(view,"wl-count").textContent = `Найдено ${visible.length} из ${view.rows.length} · Выбрано ${view.selected.size}`;
  wlEl(view,"wl-list").innerHTML = visible.length ? visible.map(r => `
    <li><label class="wl-row"><input type="checkbox" data-row="${r.id}" ${view.selected.has(r.id) ? "checked" : ""}>
      <span>${escapeHtml(r.domain)}</span></label>
      <button class="btn-icon" data-del="${r.id}" aria-label="Удалить ${escapeHtml(r.domain)}" title="Удалить">${_icons.close}</button></li>
  `).join("") : `<li class="wl-empty">${view.rows.length ? "Ничего не найдено. Измените поисковый запрос." : "Список пуст. Добавьте сайт или импортируйте файл."}</li>`;
  view.root.querySelectorAll("[data-row]").forEach(el => el.addEventListener("click", e => {
    const id = Number(el.dataset.row);
    view.selected = selectWhitelistRange(view.selected, visible.map(r => r.id), e.shiftKey ? view.anchor : id, id, el.checked);
    view.anchor = id;
    // Keep keyboard focus on the same checkbox; do not replace the list on selection.
    view.root.querySelectorAll("[data-row]").forEach(box => { box.checked = view.selected.has(Number(box.dataset.row)); });
    wlEl(view,"wl-count").textContent = `Найдено ${visible.length} из ${view.rows.length} · Выбрано ${view.selected.size}`;
    wlControls(view);
  }));
  view.root.querySelectorAll("[data-del]").forEach(el => el.addEventListener("click", () =>
    wlCommit(view,replaceWhitelistSelection(view.text,new Set([Number(el.dataset.del)]),""),view.revision)));
  wlControls(view);
}
async function loadWhitelist(view) {
  const seq = _newLoad("whitelist");
  view.busy = true; wlControls(view);
  try {
    const data = await apiGet("/whitelist");
    if (!wlCurrent(view) || _stale("whitelist", seq)) return;
    if (!data.revision || typeof data.text !== "string") throw new Error("Обновите панель: сервер не поддерживает групповые операции.");
    view.text = data.text ? data.text + "\n" : ""; view.revision = data.revision;
    view.rows = whitelistRows(view.text); view.selected.clear(); view.anchor = null; view.loaded = true;
    wlError(view); wlDraw(view);
  } catch (e) {
    if (wlCurrent(view) && !_stale("whitelist",seq)) { wlError(view,e.message); if(!view.loaded) wlEl(view,"wl-list").innerHTML = ""; }
  } finally {
    if (wlCurrent(view) && !_stale("whitelist",seq)) { view.busy = false; wlControls(view); }
  }
}
function wlEdit(view, selected) {
  if (view.busy || !view.loaded) return;
  const ids = selected ? new Set(view.selected) : null;
  view.editor = { text: view.text, revision: view.revision, ids };
  wlEl(view,"wl-editor-title").textContent = selected ? `Редактирование выбранных: ${ids.size}` : "Редактирование всего списка";
  wlEl(view,"wl-editor-hint").textContent = selected ? "Замените или удалите строки ниже. Невыбранные записи останутся на месте." : "Можно выделять, удалять и заменять любой фрагмент. Строки с # — комментарии.";
  wlEl(view,"wl-editor-text").value = selected ? view.rows.filter(r => ids.has(r.id)).map(r => r.domain).join("\n") : view.text;
  wlEl(view,"wl-editor").hidden = false; wlError(view); wlControls(view);
  wlEl(view,"wl-editor-text").focus();
}
async function wlSaveEditor(view) {
  const editor = view.editor;
  if (!editor || view.busy) return;
  const input = wlEl(view,"wl-editor-text").value;
  const next = editor.ids ? replaceWhitelistSelection(editor.text,editor.ids,input) : input;
  if (whitelistRows(next).length < whitelistRows(editor.text).length) {
    view.busy = true; wlControls(view);
    const ok = await confirmModal("Сохранить сокращённый список?", `Доменов было: ${whitelistRows(editor.text).length}. Станет: ${whitelistRows(next).length}. Изменение можно отменить до следующей правки списка.`, "Сохранить", "Вернуться к тексту");
    view.busy = false; if(!wlCurrent(view)) return; wlControls(view); if(!ok) return;
  }
  await wlCommit(view,next,editor.revision);
}
async function wlRemove(view, all) {
  if(view.busy || !view.loaded) return;
  const ids = all ? new Set(view.rows.map(r => r.id)) : new Set(view.selected);
  if(!ids.size) return;
  view.busy = true; wlControls(view);
  const ok = await confirmModal(all ? "Очистить список исключений?" : "Удалить выбранные домены?",
    `Будет удалено записей: ${ids.size}. Эти сайты перестанут исключаться из обработки z2k.`, "Удалить", "Отмена");
  view.busy = false; if(!wlCurrent(view)) return; wlControls(view); if(!ok) return;
  await wlCommit(view,replaceWhitelistSelection(view.text,ids,""),view.revision);
}
async function wlCommit(view, text, revision, remember = true) {
  if(view.busy || !view.loaded) return false;
  if(new TextEncoder().encode(text).length > 1048576) { wlError(view,"Список больше 1 МБ. Сократите его перед сохранением."); return false; }
  const previous = view.text;
  view.busy = true; wlError(view); wlControls(view);
  try {
    const result = await apiPostText("/whitelist/save?revision=" + encodeURIComponent(revision),text);
    if(!wlCurrent(view)) return true;
    view.undo = remember ? { text: previous, revision: result.revision } : null;
    view.editor = null; wlEl(view,"wl-editor").hidden = true;
    toast(remember ? "Список сохранён" : "Изменение отменено");
    await loadWhitelist(view);
    return true;
  } catch(e) {
    if(wlCurrent(view)) wlError(view,e.message);
    return false;
  } finally {
    if(wlCurrent(view)) { view.busy = false; wlControls(view); }
  }
}
async function wlAdd(view) {
  if(view.busy || view.editor || !view.loaded) return;
  const input = wlEl(view,"wl-input"); const domain = input.value.trim();
  if(domain && await wlCommit(view,view.text + domain + "\n",view.revision)) input.value = "";
}
async function wlImport(view,e) {
  const file = e.target.files && e.target.files[0]; e.target.value = "";
  if(!file || view.busy || view.editor || !view.loaded) return;
  if(file.size > 1048576) { wlError(view,"Файл больше 1 МБ."); return; }
  const revision = view.revision; const text = view.text;
  view.busy = true; wlControls(view);
  try {
    const addition = await file.text();
    if(!wlCurrent(view)) return;
    view.busy = false;
    await wlCommit(view,text + addition,revision);
  } catch(err) { if(wlCurrent(view)) wlError(view,"Не удалось прочитать файл: " + err.message); }
  finally { if(wlCurrent(view)) { view.busy = false; wlControls(view); } }
}

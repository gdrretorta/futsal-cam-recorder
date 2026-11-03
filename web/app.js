"use strict";

const $ = (id) => document.getElementById(id);
const STATUS_INTERVAL = 2000;
const SNAPSHOT_INTERVAL = 10000;

const STATE_LABELS = {
  idle: "Pronto a gravar",
  connecting: "A ligar à câmara…",
  recording: "A gravar",
  reconnecting: "Ligação perdida — a religar…",
  stopping: "A terminar a gravação…",
};
const PILL = {
  idle: ["Pronto", "ready"],
  connecting: ["A ligar", "busy"],
  recording: ["A gravar", "recording"],
  reconnecting: ["A religar", "busy"],
  stopping: ["A terminar", "busy"],
};

let status = null;
let listKey = "";
let timerStart = null;
let pending = false;
const REC_PAGE = 5;
let recShown = REC_PAGE; // recordings listed; "Ver mais" adds REC_PAGE
let recordings = [];
let me = null;            // {authRequired, user} from /api/me
let loadedVersion = null; // version that served this page
let updating = null;      // while an update installs: {version, since}

// ---------- API ----------

async function api(method, url, body) {
  const opts = { method, headers: {} };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(url, opts);
  // Session expired, user deleted or password changed elsewhere.
  if (res.status === 401 && url !== "/api/login") loadMe();
  if (!res.ok) {
    let msg = res.statusText;
    try { msg = (await res.json()).error || msg; } catch { /* not JSON */ }
    const err = new Error(msg);
    err.status = res.status;
    throw err;
  }
  const type = res.headers.get("Content-Type") || "";
  if (type.includes("application/json")) return res.json();
  if (type.startsWith("image/")) return res.blob();
  return null;
}

function toast(msg, isError = false) {
  // Requests cut short by a logout, or refused until the password is changed.
  if (isError && !($("loginView").hidden && $("passwordView").hidden)) return;
  const el = $("toast");
  el.textContent = msg;
  el.classList.toggle("error", isError);
  el.hidden = false;
  clearTimeout(toast.t);
  toast.t = setTimeout(() => (el.hidden = true), isError ? 6000 : 3000);
}

// ---------- Formatting ----------

function fmtDuration(ms) {
  const s = Math.max(0, Math.floor(ms / 1000));
  const p = (n) => String(n).padStart(2, "0");
  return `${p(Math.floor(s / 3600))}:${p(Math.floor((s % 3600) / 60))}:${p(s % 60)}`;
}

function fmtBytes(n) {
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return `${n.toFixed(i >= 3 ? 1 : 0)} ${units[i]}`;
}

const dateFmt = new Intl.DateTimeFormat("pt-PT", { weekday: "short", day: "numeric", month: "short", year: "numeric", hour: "2-digit", minute: "2-digit" });
const dayFmt = new Intl.DateTimeFormat("pt-PT", { weekday: "short", day: "numeric", month: "short", year: "numeric" });
const timeFmt = new Intl.DateTimeFormat("pt-PT", { hour: "2-digit", minute: "2-digit" });

// Recordings are named 2026-10-29_153000.mp4; fall back to the file date.
function recordingDate(rec) {
  const m = rec.name.match(/^(\d{4})-(\d{2})-(\d{2})_(\d{2})(\d{2})(\d{2})/);
  if (m) return new Date(+m[1], +m[2] - 1, +m[3], +m[4], +m[5], +m[6]);
  return new Date(rec.modTime);
}

// ---------- Status ----------

async function refreshStatus() {
  let s;
  try {
    s = await api("GET", "/api/status");
  } catch {
    // The app is down for a moment while it restarts with a new version. The
    // download, install and restart take a second or two, so the last status
    // seen is usually still "downloading".
    if (status && UPDATE_BUSY.includes(status.update.state)) showUpdating(newVersion(status.update));
    if (updating) return renderUpdating();
    $("pill").textContent = "Sem ligação";
    $("pill").className = "pill busy";
    return;
  }
  // A new version is running: reload to get its page and assets.
  if (loadedVersion === null) loadedVersion = s.version;
  if (s.version !== loadedVersion) {
    showUpdating(s.version);
    $("updateOverlayText").textContent = `Versão ${s.version} instalada. A recarregar…`;
    location.reload();
    return;
  }
  if (s.loginRequired) {
    if ($("loginView").hidden && $("passwordView").hidden) loadMe();
    return;
  }
  status = s;
  if (status.update.state === "applying") showUpdating(newVersion(status.update));
  else if (updating) hideUpdating(); // the install failed and the old version kept running
  renderStatus();
}

// ---------- Update overlay ----------

const UPDATE_SLOW = 3 * 60 * 1000;
const UPDATE_BUSY = ["checking", "downloading", "waiting", "applying"];

// The version being installed, if known. While checking, "latest" still holds
// the previous check's result, which can be the running version.
function newVersion(u) {
  return u.state !== "checking" && u.latest && u.latest !== u.current ? u.latest : null;
}

function showUpdating(version) {
  if (!updating) {
    updating = { version, since: Date.now() };
    document.querySelectorAll("dialog[open]").forEach((d) => d.close());
    for (const el of document.querySelectorAll("body > header, body > main, body > footer")) el.inert = true;
    $("updateOverlay").hidden = false;
  }
  renderUpdating();
}

function hideUpdating() {
  updating = null;
  for (const el of document.querySelectorAll("body > header, body > main, body > footer")) el.inert = false;
  $("updateOverlay").hidden = true;
}

function renderUpdating() {
  const slow = Date.now() - updating.since > UPDATE_SLOW;
  const v = updating.version ? ` a versão ${updating.version}` : " a nova versão";
  $("updateOverlayText").textContent = slow
    ? "A atualização está a demorar mais do que o esperado. A página recarrega sozinha quando o gravador voltar."
    : `A instalar${v} e a reiniciar o gravador. A página recarrega sozinha quando terminar.`;
  $("updateReloadBtn").hidden = !slow;
}

$("updateReloadBtn").addEventListener("click", () => location.reload());

function renderStatus() {
  const r = status.recorder;
  const active = r.state !== "idle";
  document.title = (r.state === "recording" ? "● " : "") + status.title;
  $("title").textContent = status.title;

  const [pillText, pillClass] = PILL[r.state] || [r.state, ""];
  $("pill").textContent = pillText;
  $("pill").className = "pill " + pillClass;

  $("stateText").textContent = STATE_LABELS[r.state] || r.state;
  $("stateError").textContent = r.error || "";
  $("stateError").hidden = !r.error;
  $("recBadge").hidden = !(r.state === "recording" || r.state === "reconnecting");
  renderLive();
  liveFollowState();
  $("setupNotice").hidden = status.configured;
  // Only an admin can open the settings (or anyone, while there are no users).
  const canSetUp = !me || !me.authRequired || role() === "admin";
  $("setupNotice").querySelector("button").hidden = !canSetUp;
  $("setupNotice").querySelector("p").textContent = canSetUp
    ? "A câmara ainda não está configurada."
    : "A câmara ainda não está configurada. Peça a um administrador para a configurar.";

  const btn = $("recordBtn");
  btn.classList.toggle("stop", active);
  btn.classList.toggle("busy", r.state === "connecting" || r.state === "stopping");
  btn.disabled = pending || r.state === "stopping" || (!active && !status.configured) || status.update.state === "applying";
  btn.setAttribute("aria-label", active ? "Parar gravação" : "Iniciar gravação");
  btn.title = active ? "Parar gravação" : "Iniciar gravação";

  timerStart = r.startedAt ? new Date(r.startedAt) : null;
  renderTimer();

  $("version").textContent = status.version;
  renderUpdate();

  if (status.disk) {
    const used = 1 - status.disk.free / status.disk.total;
    $("disk").hidden = false;
    $("diskFill").style.width = `${(used * 100).toFixed(1)}%`;
    $("diskFill").classList.toggle("low", status.disk.free < 10 * 1024 ** 3);
    $("diskText").textContent = `${fmtBytes(status.disk.free)} livres`;
  }

  // Reload the list whenever a file starts, stops or finishes processing.
  const key = `${r.state}|${r.file || ""}|${(r.finalizing || []).join(",")}`;
  if (key !== listKey) {
    listKey = key;
    loadRecordings();
  }
}

function renderTimer() {
  $("timer").textContent = timerStart ? fmtDuration(Date.now() - timerStart) : "00:00:00";
}

// ---------- Updates (footer notice and the "Sistema" settings) ----------

// What needs an admin's attention, shown in the footer and on the settings'
// "Sistema" link. "" when nothing does.
function updateAttention(u) {
  switch (u.state) {
    case "downloading": return `A descarregar a versão ${u.latest}…`;
    case "waiting": return `Versão ${u.latest} pronta a instalar`;
    case "error": return "A atualização falhou";
    default: return "";
  }
}

// The recorder's update state in the settings: [text, class].
function updateState(u) {
  const checked = u.lastCheck ? ` · verificado ${ago(u.lastCheck)}` : "";
  switch (u.state) {
    case "checking": return ["A verificar atualizações…", "busy"];
    case "downloading": return [`A descarregar a versão ${u.latest}…`, "busy"];
    case "waiting": return [`Versão ${u.latest} descarregada: instala quando não houver gravações`, "warn"];
    case "applying": return [`A instalar a versão ${u.latest}…`, "busy"];
    case "error": return [u.error || "A atualização falhou", "bad"];
    case "disabled": return ["Atualizações automáticas desligadas" + checked, ""];
    default: return u.lastCheck ? ["Atualizado" + checked, "ok"] : ["Ainda não verificado", ""];
  }
}

// "há 5 min", "há 2 h", "em 31/10".
function ago(iso) {
  const ms = Date.now() - new Date(iso);
  const min = Math.floor(ms / 60000);
  if (min < 1) return "agora mesmo";
  if (min < 60) return `há ${min} min`;
  if (min < 24 * 60) return `há ${Math.floor(min / 60)} h`;
  return "em " + new Date(iso).toLocaleDateString("pt-PT", { day: "numeric", month: "numeric" });
}

const RELEASES = "https://github.com/gdrretorta/futsal-cam-recorder/releases";
const releaseUrl = (v) => (/^\d+\.\d+\.\d+$/.test(v || "") ? `${RELEASES}/tag/v${v}` : RELEASES);

function setState(el, [text, kind]) {
  el.textContent = text;
  el.className = "sys-state" + (kind ? " " + kind : "");
  el.hidden = !text;
}

function renderUpdate() {
  if (!status) return;
  const u = status.update;
  const attn = role() === "admin" ? updateAttention(u) : "";
  $("updateBadge").hidden = !attn;
  $("updateBadgeText").textContent = attn;
  $("systemNav").querySelector(".attn-dot").hidden = !attn;
  $("sysVersion").textContent = `versão ${status.version}`;
  setState($("sysState"), updateState(u));
  const notesFor = u.latest && u.latest !== status.version && u.state !== "error" ? u.latest : status.version;
  $("sysNotes").href = releaseUrl(notesFor);
  $("updateBtn").disabled = UPDATE_BUSY.includes(u.state);
}

$("recordBtn").addEventListener("click", async () => {
  if (!status) return;
  const active = status.recorder.state !== "idle";
  pending = true;
  renderStatus();
  try {
    await api("POST", active ? "/api/recording/stop" : "/api/recording/start");
  } catch (e) {
    toast(e.message, true);
  } finally {
    pending = false;
    refreshStatus();
  }
});

// ---------- Camera preview ----------

async function refreshSnapshot() {
  if (document.hidden || !status || !status.configured || live) return;
  try {
    const blob = await api("GET", "/api/snapshot");
    const img = $("preview");
    const old = img.src;
    img.src = URL.createObjectURL(blob);
    if (old) URL.revokeObjectURL(old);
    img.hidden = false;
    $("previewEmpty").hidden = true;
  } catch (e) {
    $("preview").hidden = true;
    $("previewEmpty").hidden = false;
    $("previewEmpty").textContent = "Sem imagem: " + e.message;
  }
}

// ---------- Live view (HLS) ----------
// The server reads the live view from the recording itself while recording
// (no second stream from the camera), and runs its own ffmpeg only while
// someone watches and nothing is recording. At most a few pages may watch at
// once; the id below is how the server counts this page.

const LIVE_VIEWER = Math.random().toString(36).slice(2) + Date.now().toString(36);
const LIVE_URL = "/api/live?viewer=" + LIVE_VIEWER;
const LIVE_RETRIES = 5;
let live = null;        // {hls} or {native: true} while the player runs
let liveWanted = false; // the user asked for the live view
let liveTries = 0;
let liveState = "";     // recorder state the player was started in

function loadHls() {
  if (window.Hls) return Promise.resolve(window.Hls);
  return new Promise((resolve, reject) => {
    const s = document.createElement("script");
    s.src = document.querySelector('meta[name="hls-src"]').content;
    s.onload = () => resolve(window.Hls);
    s.onerror = () => reject(new Error("não foi possível carregar o leitor de vídeo"));
    document.head.append(s);
  });
}

function renderLive() {
  const btn = $("liveBtn");
  btn.hidden = !status || !status.configured;
  btn.classList.toggle("on", liveWanted);
  btn.textContent = liveWanted ? (live ? "● Ao vivo · Parar" : "A ligar…") : "▶ Ver em direto";
}

function teardownLive() {
  const video = $("live");
  if (live && live.hls) live.hls.destroy();
  live = null;
  video.pause();
  video.removeAttribute("src");
  video.load();
  video.hidden = true;
  if ($("preview").getAttribute("src")) $("preview").hidden = false;
}

async function startLive() {
  liveWanted = true;
  renderLive();
  const video = $("live");
  try {
    // Asked once first, for a clear message (too many viewers, camera
    // down) instead of a player error. It also waits for the first segment.
    const res = await fetch(LIVE_URL);
    if (!res.ok) {
      let msg = res.statusText;
      try { msg = (await res.json()).error || msg; } catch { /* not JSON */ }
      const err = new Error(msg);
      err.status = res.status;
      throw err;
    }
    if (!liveWanted || document.hidden) return;
    teardownLive();
    // iPhones (no MediaSource) play HLS natively; elsewhere hls.js does it,
    // with a shorter delay than most native players.
    if (!window.MediaSource && !window.ManagedMediaSource && video.canPlayType("application/vnd.apple.mpegurl")) {
      live = { native: true };
      video.src = LIVE_URL;
    } else {
      const Hls = await loadHls();
      if (!liveWanted || document.hidden) return;
      if (!Hls.isSupported()) throw new Error("este browser não consegue mostrar o direto");
      const hls = new Hls({ liveSyncDurationCount: 2, liveMaxLatencyDurationCount: 6, maxLiveSyncPlaybackRate: 1.2, backBufferLength: 10 });
      hls.on(Hls.Events.ERROR, (_, d) => {
        if (d.fatal) liveFailed(new Error("o direto foi interrompido"));
      });
      live = { hls };
      hls.loadSource(LIVE_URL);
      hls.attachMedia(video);
    }
    liveState = status ? status.recorder.state : "";
    video.hidden = false;
    $("preview").hidden = true;
    $("previewEmpty").hidden = true;
    video.play().catch(() => { /* autoplay refused: the controls are there */ });
    liveTries = 0;
  } catch (e) {
    liveFailed(e);
  }
  renderLive();
}

// A dropped live view is retried a few times (camera reconnecting, recording
// just started); a refusal is not.
function liveFailed(e) {
  teardownLive();
  const retry = liveWanted && ![409, 429].includes(e.status) && liveTries < LIVE_RETRIES;
  if (retry) {
    liveTries++;
    clearTimeout(liveFailed.t);
    liveFailed.t = setTimeout(() => { if (liveWanted && !live && !document.hidden) startLive(); }, 3000);
  } else if (liveWanted) {
    liveWanted = false;
    toast("Direto: " + e.message, true);
    refreshSnapshot();
  }
  renderLive();
}

function stopLive() {
  liveWanted = false;
  liveTries = 0;
  clearTimeout(liveFailed.t);
  teardownLive();
  renderLive();
  refreshSnapshot();
}

$("liveBtn").addEventListener("click", () => (liveWanted ? stopLive() : startLive()));
$("live").addEventListener("error", () => { if (live && live.native) liveFailed(new Error("o direto foi interrompido")); });

// Starting or stopping a recording switches the server to another file: the
// player starts again on it instead of waiting for the old one.
function liveFollowState() {
  const s = status && status.recorder.state;
  if (!live || !s || s === liveState || !["idle", "recording"].includes(s)) return;
  liveState = s;
  teardownLive();
  startLive();
}

// ---------- Recordings ----------

async function loadRecordings() {
  let list;
  try {
    list = await api("GET", "/api/recordings");
  } catch (e) {
    toast("Erro ao carregar gravações: " + e.message, true);
    return;
  }
  recordings = list;
  renderRecordings();
}

// The list shows REC_PAGE recordings at a time, so the page has one scroll.
function renderRecordings() {
  const list = recordings;
  $("recList").replaceChildren(...list.slice(0, recShown).map(renderRecording));
  $("recEmpty").hidden = list.length > 0;
  const total = list.reduce((s, r) => s + r.size, 0);
  $("recCount").textContent = list.length ? `${list.length} · ${fmtBytes(total)}` : "";
  const rest = list.length - recShown;
  $("recMore").hidden = rest <= 0;
  $("recMore").textContent = `Ver mais (${rest})`;
}

$("recMore").addEventListener("click", () => {
  recShown += REC_PAGE;
  renderRecordings();
});

const ICONS = {
  play: '<svg viewBox="0 0 24 24"><path d="M7 4.5v15l12-7.5-12-7.5Z"/></svg>',
  link: '<svg viewBox="0 0 24 24"><path d="M10 14a4.5 4.5 0 0 0 6.4 0l3.2-3.2a4.5 4.5 0 0 0-6.4-6.4L12 5.6"/><path d="M14 10a4.5 4.5 0 0 0-6.4 0l-3.2 3.2a4.5 4.5 0 0 0 6.4 6.4l1.2-1.2"/></svg>',
  download: '<svg viewBox="0 0 24 24"><path d="M12 4v11m0 0-4.5-4.5M12 15l4.5-4.5M5 20h14"/></svg>',
  edit: '<svg viewBox="0 0 24 24"><path d="M4 20h4L19 9l-4-4L4 16v4Z"/><path d="m13.5 6.5 4 4"/></svg>',
  star: '<svg viewBox="0 0 24 24"><path d="m12 3.5 2.6 5.3 5.9.9-4.25 4.1 1 5.8L12 16.9l-5.25 2.7 1-5.8L3.5 9.7l5.9-.9L12 3.5Z"/></svg>',
  trash: '<svg viewBox="0 0 24 24"><path d="M4 7h16M10 11v6m4-6v6M6 7l1 13h10l1-13M9 7V4h6v3"/></svg>',
};

function iconButton(icon, title, onClick, extraClass = "") {
  const b = document.createElement("button");
  b.className = "icon-btn " + extraClass;
  b.title = title;
  b.setAttribute("aria-label", title);
  b.innerHTML = ICONS[icon];
  b.addEventListener("click", onClick);
  return b;
}

function renderRecording(rec) {
  const li = document.createElement("li");
  li.className = "rec-item";
  const url = `/recordings/${encodeURIComponent(rec.name)}`;

  const info = document.createElement("div");
  info.className = "rec-info";
  const name = document.createElement("div");
  name.className = "rec-name";
  // On a phone the time moves to the second line, so the date fits (style.css).
  const when = recordingDate(rec);
  const time = timeFmt.format(when);
  const span = (cls, text) => Object.assign(document.createElement("span"), { className: cls, textContent: text });
  name.append(dayFmt.format(when), span("rec-time", `, ${time}`));
  if (rec.processing) {
    const tag = document.createElement("span");
    tag.className = "tag";
    tag.textContent = "A processar…";
    name.append(tag);
  }
  const meta = document.createElement("div");
  meta.className = "muted rec-meta";
  meta.append(span("rec-time-short", `${time} · `), span("rec-file", `${rec.name} · `), fmtBytes(rec.size));
  info.append(name, meta);

  const actions = document.createElement("div");
  actions.className = "rec-actions";
  const play = iconButton("play", "Ver", () => openPlayer(rec, url));
  const link = iconButton("link", "Copiar link", () => copyLink(new URL(url, location.href).href));
  const dl = iconButton("download", "Descarregar", () => { location.href = url + "?download=1"; });
  const keep = iconButton("star", rec.protected ? "Protegida: não é apagada (tirar estrela)" : "Não apagar (proteger da limpeza automática)",
    () => setProtected(rec, !rec.protected), rec.protected ? "keep on" : "keep");
  keep.setAttribute("aria-pressed", rec.protected ? "true" : "false");
  const del = iconButton("trash", rec.protected ? "Protegida: tire a estrela para apagar" : "Apagar", () => confirmDelete(rec), "del");
  for (const b of [play, link, dl, keep, del]) b.disabled = !!rec.processing;
  if (rec.protected) del.disabled = true;
  actions.append(play, link, dl, keep, del);

  li.append(info, actions);
  return li;
}

// navigator.clipboard only exists in a secure context (https or localhost), so
// opening the page by LAN IP over http falls back to execCommand, then prompt.
async function copyLink(href) {
  try {
    await navigator.clipboard.writeText(href);
    toast("Link copiado");
    return;
  } catch { /* not available here */ }
  const ta = document.createElement("textarea");
  ta.value = href;
  ta.setAttribute("readonly", "");
  ta.style.position = "fixed";
  ta.style.opacity = "0";
  document.body.append(ta);
  ta.select();
  let ok = false;
  try { ok = document.execCommand("copy"); } catch { /* unsupported */ }
  ta.remove();
  if (ok) toast("Link copiado");
  else prompt("Copie o link:", href);
}

function openPlayer(rec, url) {
  $("playerTitle").textContent = dateFmt.format(recordingDate(rec));
  $("player").src = url;
  $("playerDlg").showModal();
}
$("playerDlg").addEventListener("close", () => {
  $("player").pause();
  $("player").removeAttribute("src");
  $("player").load();
});

// confirmAction asks before something that can't be undone, then runs action.
function confirmAction(title, text, action) {
  const dlg = $("confirmDlg");
  $("confirmTitle").textContent = title;
  $("confirmText").textContent = text + " Esta ação não pode ser desfeita.";
  dlg.returnValue = "";
  dlg.onclose = () => { if (dlg.returnValue === "ok") action(); };
  dlg.showModal();
}

async function setProtected(rec, on) {
  try {
    await api("PUT", `/api/recordings/${encodeURIComponent(rec.name)}/protected`, { protected: on });
    toast(on ? "Gravação protegida: não vai ser apagada" : "Proteção retirada");
  } catch (e) {
    toast(e.message, true);
  }
  loadRecordings();
}

function confirmDelete(rec) {
  confirmAction("Apagar gravação?", `${dateFmt.format(recordingDate(rec))} (${rec.name}, ${fmtBytes(rec.size)}).`, async () => {
    try {
      await api("DELETE", `/api/recordings/${encodeURIComponent(rec.name)}`);
      toast("Gravação apagada");
    } catch (e) {
      toast(e.message, true);
    }
    loadRecordings();
  });
}

// ---------- Settings ----------

const form = $("settingsForm");
const fields = form.elements;

const NO_USERS = "Ainda não existe nenhum utilizador. Crie um administrador no PC: corra o instalador novamente ou execute \"futsal-cam-recorder.exe add-user <email> admin\".";

// The settings are a page of their own (#definicoes), shown in place of the
// recorder, so the browser's back button returns to the recorder.
const SETTINGS_HASH = "#definicoes";
let settingsOpen = false;
let settingsPushed = false; // we added the history entry, so "back" removes it

function clearSettingsHash() {
  if (location.hash === SETTINGS_HASH) history.replaceState(null, "", location.pathname + location.search);
}

function showSettings(open) {
  settingsOpen = open;
  applyAccess();
  if (!open) clearTimeout(loadTunnel.t);
}

// closeSettings returns to the recorder, also from the address bar's point of view.
function closeSettings() {
  if (location.hash === SETTINGS_HASH && settingsPushed) {
    history.back(); // popstate hides the page
    return;
  }
  clearSettingsHash();
  showSettings(false);
}

window.addEventListener("popstate", () => {
  const want = location.hash === SETTINGS_HASH;
  if (!want) settingsPushed = false;
  if (want && !settingsOpen) openSettings();
  else if (!want && settingsOpen) showSettings(false);
});

// The settings are admin-only. While there are no users, nobody can open
// them: explain how to create the first admin.
async function openSettings() {
  if (!me || !me.user) {
    toast(NO_USERS, true);
    clearSettingsHash();
    return;
  }
  let s;
  try {
    s = await api("GET", "/api/settings");
  } catch (e) {
    toast(e.message, true);
    if (!settingsOpen) clearSettingsHash();
    return;
  }
  fields.host.value = s.camera.host;
  fields.port.value = s.camera.port;
  fields.path.value = s.camera.path;
  fields.user.value = s.camera.user;
  fields.password.value = "";
  fields.password.placeholder = s.camera.hasPassword ? "•••••• (inalterada)" : "";
  fields.password.dataset.touched = "";
  fields.recordingsDir.value = s.recordingsDir;
  fields.retentionDays.value = s.retentionDays;
  fields.title.value = s.title;
  fields.autoUpdate.checked = s.autoUpdate;
  fields.notifyHours.value = s.notify.longRecordingHours;
  fields.notifyCameraDown.value = s.notify.cameraDownMinutes;
  fields.notifyLowDisk.value = s.notify.lowDiskGB;
  fields.notifyFrom.value = s.notify.from;
  fields.notifySite.value = s.notify.siteUrl;
  fields.notifyKey.value = "";
  fields.notifyKey.placeholder = s.notify.hasApiKey ? "•••••• (inalterada)" : "re_…";
  fields.notifyKey.dataset.touched = "";
  $("notifyTestResult").textContent = "";
  fields.tunnelAuto.checked = s.tunnel.autoUpdate;
  $("tunnelVersion").textContent = "";
  $("tunnelState").hidden = true;
  $("settingsError").hidden = true;
  $("testResult").textContent = "";
  $("testImg").hidden = true;
  if (location.hash !== SETTINGS_HASH) {
    history.pushState(null, "", SETTINGS_HASH);
    settingsPushed = true;
  }
  showSettings(true);
  window.scrollTo(0, 0);
  renderUpdate();
  loadTunnel();
  loadLogs();
  return true;
}

fields.password.addEventListener("input", () => (fields.password.dataset.touched = "1"));
fields.notifyKey.addEventListener("input", () => (fields.notifyKey.dataset.touched = "1"));

function formSettings() {
  const camera = {
    host: fields.host.value.trim(),
    port: Number(fields.port.value),
    path: fields.path.value.trim(),
    user: fields.user.value.trim(),
  };
  // Only send the password when it was edited, so the stored one is kept.
  if (fields.password.dataset.touched) camera.password = fields.password.value;
  return {
    title: fields.title.value.trim(),
    camera,
    recordingsDir: fields.recordingsDir.value.trim(),
    retentionDays: Number(fields.retentionDays.value) || 0,
    autoUpdate: fields.autoUpdate.checked,
    tunnel: { autoUpdate: fields.tunnelAuto.checked },
    notify: {
      longRecordingHours: Number(fields.notifyHours.value) || 0,
      cameraDownMinutes: Number(fields.notifyCameraDown.value) || 0,
      lowDiskGB: Number(fields.notifyLowDisk.value) || 0,
      from: fields.notifyFrom.value.trim(),
      siteUrl: fields.notifySite.value.trim(),
      // Like the camera password: only sent when edited.
      ...(fields.notifyKey.dataset.touched ? { apiKey: fields.notifyKey.value.trim() } : {}),
    },
  };
}

// ---------- cloudflared (Cloudflare Tunnel) ----------

const TUNNEL_STATES = {
  checking: "A verificar versões…",
  downloading: "A transferir a nova versão…",
  waiting: "À espera que a gravação termine…",
  updating: "A instalar: o site fica indisponível uns segundos…",
  verifying: "A confirmar que o túnel voltou…",
  rollingBack: "O túnel não voltou: a repor a versão anterior…",
};

async function loadTunnel() {
  clearTimeout(loadTunnel.t);
  if (!settingsOpen) return;
  let t;
  try {
    t = await api("GET", "/api/tunnel");
  } catch {
    // During the swap this very page loses its connection for a moment.
    $("tunnelState").textContent = "Sem ligação ao site (normal durante a atualização)…";
    $("tunnelState").hidden = false;
    loadTunnel.t = setTimeout(loadTunnel, 3000);
    return;
  }
  const busy = t.state in TUNNEL_STATES;
  $("tunnelVersion").textContent = t.installed ? `versão ${t.installed}` : "";
  let st;
  if (t.state === "unsupported") st = ["Só disponível no PC Windows onde corre o cloudflared.", ""];
  else if (busy) st = [TUNNEL_STATES[t.state], "busy"];
  else if (!t.installed) st = ["A procurar o cloudflared…", ""];
  else if (t.failed) st = ["A última atualização falhou: " + t.message, "bad"];
  else if (t.latest && t.latest !== t.installed) st = [`Versão ${t.latest} disponível`, "warn"];
  else st = ["Atualizado" + (t.lastCheck ? ` · verificado ${ago(t.lastCheck)}` : ""), "ok"];
  setState($("tunnelState"), st);
  $("tunnelUpdateBtn").disabled = busy || t.state === "unsupported";
  loadTunnel.t = setTimeout(loadTunnel, busy || !t.installed ? 3000 : 30000);
}

$("tunnelUpdateBtn").addEventListener("click", async () => {
  try {
    await api("POST", "/api/tunnel/update");
  } catch (e) {
    toast(e.message, true);
  }
  loadTunnel();
});

$("notifyTestBtn").addEventListener("click", async () => {
  const btn = $("notifyTestBtn");
  btn.disabled = true;
  $("notifyTestResult").textContent = "A enviar…";
  try {
    const r = await api("POST", "/api/settings/notify-test", formSettings());
    $("notifyTestResult").textContent = "✓ Enviado para " + r.to.join(", ");
  } catch (e) {
    $("notifyTestResult").textContent = "✗ " + e.message;
  } finally {
    btn.disabled = false;
  }
});

form.addEventListener("submit", async (ev) => {
  ev.preventDefault();
  try {
    await api("PUT", "/api/settings", formSettings());
    closeSettings();
    toast("Definições guardadas");
    refreshStatus().then(refreshSnapshot);
  } catch (e) {
    $("settingsError").textContent = e.message;
    $("settingsError").hidden = false;
  }
});

// ---------- Login & account ----------

const ROLES = { admin: "Administrador", userManager: "Gestor de utilizadores", user: "Utilizador" };
const role = () => me && me.user ? me.user.role : null;

async function loadMe() {
  try {
    me = await api("GET", "/api/me");
  } catch {
    // Offline: keep the current state, or keep trying if there is none yet.
    if (!me) setTimeout(loadMe, 3000);
    return;
  }
  applyAccess();
}

// applyAccess shows the login page, the "choose your password" page (when
// someone else set the password) or the recorder, with the buttons the
// user's role allows. While there are no users the recorder is open to all.
let wantLogin = false; // on the PC itself: the user asked to log in

function applyAccess() {
  const loggedOut = (me.authRequired && !me.user) || wantLogin;
  const mustChange = !!(me.user && me.user.mustChangePassword);
  $("bootView").hidden = true;
  const wasMustChange = !$("passwordView").hidden;
  const wasLoggedOut = !$("loginView").hidden || wasMustChange;
  $("loginView").hidden = !loggedOut;
  $("passwordView").hidden = !mustChange;
  if ((loggedOut || mustChange) && settingsOpen) {
    settingsOpen = false;
    clearSettingsHash();
  }
  $("topbarActions").hidden = loggedOut || mustChange;
  $("app").hidden = loggedOut || mustChange || settingsOpen;
  $("settingsView").hidden = !settingsOpen;
  document.querySelector("body > footer").hidden = loggedOut || mustChange || settingsOpen;
  if (mustChange) {
    if (wasMustChange) return; // don't wipe what is being typed
    clearRecorder();
    // Just logged in: the current password is known, don't ask again.
    passwordForm.reset();
    passwordForm.elements.currentPassword.value = loginPassword;
    passwordForm.elements.currentPassword.required = !loginPassword;
    $("tempPasswordLabel").hidden = !!loginPassword;
    $("passwordError").hidden = true;
    passwordForm.elements[loginPassword ? "password" : "currentPassword"].focus();
    return;
  }
  loginPassword = "";
  if (loggedOut) {
    clearRecorder();
    $("loginBackBtn").hidden = !wantLogin;
    loginForm.elements.email.focus();
    return;
  }
  $("settingsBtn").hidden = me.authRequired && role() !== "admin";
  // Lost access to the settings (role changed, logged out on the PC…).
  if (settingsOpen && $("settingsBtn").hidden) closeSettings();
  $("usersBtn").hidden = !(role() === "admin" || role() === "userManager");
  // On the PC itself without a session, the same button logs in.
  $("accountBtn").hidden = !me.user;
  $("accountBtn").title = me.local ? "Iniciar sessão" : "A minha conta";
  $("accountBtn").setAttribute("aria-label", $("accountBtn").title);
  renderUpdate();
  if (wasLoggedOut) refreshStatus().then(refreshSnapshot);
}

// Nothing of the recorder stays on screen after logging out.
function clearRecorder() {
  document.querySelectorAll("dialog[open]").forEach((d) => d.close());
  clearTimeout(loadTunnel.t);
  status = null;
  recordings = [];
  listKey = "";
  $("recList").replaceChildren();
  $("preview").hidden = true;
  $("preview").removeAttribute("src");
  stopLive();
}

let loginPassword = ""; // kept only until the forced password change

const passwordForm = $("passwordForm");

passwordForm.addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const f = passwordForm.elements;
  const err = $("passwordError");
  err.hidden = true;
  if (f.password.value !== f.confirm.value) {
    err.textContent = "As palavras-passe não coincidem.";
    err.hidden = false;
    return;
  }
  try {
    await api("PUT", "/api/me/password", { currentPassword: f.currentPassword.value, password: f.password.value });
    loginPassword = "";
    toast("Palavra-passe guardada");
    await loadMe();
  } catch (e) {
    err.textContent = e.message;
    err.hidden = false;
  }
});

const loginForm = $("loginForm");

loginForm.addEventListener("submit", async (ev) => {
  ev.preventDefault();
  $("loginError").hidden = true;
  try {
    me = await api("POST", "/api/login", {
      email: loginForm.elements.email.value.trim(),
      password: loginForm.elements.password.value,
    });
    if (me.user.mustChangePassword) loginPassword = loginForm.elements.password.value;
    loginForm.reset();
    wantLogin = false;
    applyAccess();
  } catch (e) {
    $("loginError").textContent = e.message;
    $("loginError").hidden = false;
    loginForm.elements.password.select();
  }
});

const accountForm = $("accountForm");

$("loginBackBtn").addEventListener("click", () => {
  wantLogin = false;
  loginForm.reset();
  $("loginError").hidden = true;
  applyAccess();
});

$("accountBtn").addEventListener("click", () => {
  if (me.local) {
    wantLogin = true;
    applyAccess();
    return;
  }
  accountForm.reset();
  $("accountEmail").textContent = me.user.email;
  $("accountRole").textContent = ROLES[me.user.role] || me.user.role;
  $("accountError").hidden = true;
  $("accountDlg").showModal();
});

accountForm.addEventListener("submit", async (ev) => {
  ev.preventDefault();
  try {
    await api("PUT", "/api/me/password", {
      currentPassword: accountForm.elements.currentPassword.value,
      password: accountForm.elements.password.value,
    });
    $("accountDlg").close();
    toast("Palavra-passe alterada");
  } catch (e) {
    $("accountError").textContent = e.message;
    $("accountError").hidden = false;
  }
});

async function logout() {
  try {
    await api("POST", "/api/logout");
  } catch { /* the cookie is cleared either way */ }
  loginPassword = "";
  loadMe();
}
$("logoutBtn").addEventListener("click", logout);
$("passwordLogoutBtn").addEventListener("click", logout);

// ---------- Users ----------

async function openUsers() {
  let list;
  try {
    list = await api("GET", "/api/users");
  } catch (e) {
    toast(e.message, true);
    return;
  }
  $("userList").replaceChildren(...list.map(renderUser));
  if (!$("usersDlg").open) $("usersDlg").showModal();
}

function renderUser(u) {
  const li = document.createElement("li");
  li.className = "rec-item";
  const info = document.createElement("div");
  info.className = "rec-info";
  const name = document.createElement("div");
  name.className = "rec-name";
  name.textContent = u.email;
  const meta = document.createElement("div");
  meta.className = "muted";
  meta.textContent = (ROLES[u.role] || u.role) + (u.id === me.user.id ? " · a sua conta" : "") +
    (u.mustChangePassword ? " · ainda não escolheu a palavra-passe" : "");
  info.append(name, meta);

  // User managers only manage plain users (the server refuses the rest too).
  const locked = role() !== "admin" && u.role !== "user";
  const actions = document.createElement("div");
  actions.className = "rec-actions";
  const edit = iconButton("edit", "Editar", () => openUserForm(u));
  const del = iconButton("trash", "Apagar", () => confirmAction("Apagar utilizador?", `${u.email} deixa de poder entrar.`, async () => {
    try {
      await api("DELETE", `/api/users/${encodeURIComponent(u.id)}`);
      toast("Utilizador apagado");
    } catch (e) {
      toast(e.message, true);
    }
    openUsers();
  }), "del");
  edit.disabled = locked;
  del.disabled = locked || u.id === me.user.id;
  if (locked) edit.title = del.title = "Só um administrador pode alterar administradores e gestores de utilizadores";
  actions.append(edit, del);
  li.append(info, actions);
  return li;
}

const userForm = $("userForm");
let editingUser = null;

function openUserForm(u) {
  editingUser = u;
  userForm.reset();
  const select = userForm.elements.role;
  const roles = role() === "admin" ? ["user", "userManager", "admin"] : ["user"];
  select.replaceChildren(...roles.map((r) => new Option(ROLES[r], r)));
  select.value = u ? u.role : "user";
  select.disabled = roles.length === 1;
  userForm.elements.email.value = u ? u.email : "";
  userForm.elements.password.required = !u;
  const self = u && u.id === me.user.id;
  userForm.elements.password.placeholder = u ? "Deixe vazio para manter" : "";
  $("userPasswordLabel").textContent = u ? "Nova palavra-passe" : "Palavra-passe temporária (mínimo 8 caracteres)";
  $("userPasswordHint").hidden = self;
  $("userDlgTitle").textContent = u ? "Editar utilizador" : "Novo utilizador";
  $("userError").hidden = true;
  $("userDlg").showModal();
}

userForm.addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const body = {
    email: userForm.elements.email.value.trim(),
    role: userForm.elements.role.value,
    password: userForm.elements.password.value,
  };
  try {
    if (editingUser) await api("PUT", `/api/users/${encodeURIComponent(editingUser.id)}`, body);
    else await api("POST", "/api/users", body);
    $("userDlg").close();
    const reset = !editingUser || (body.password && editingUser.id !== me.user.id);
    toast((editingUser ? "Utilizador guardado" : "Utilizador criado") +
      (reset ? ". Vai ter de escolher uma palavra-passe nova ao entrar." : ""), false);
    // Editing yourself can change what you may see.
    if (editingUser && editingUser.id === me.user.id) await loadMe();
    if (role() === "admin" || role() === "userManager") openUsers();
    else $("usersDlg").close();
  } catch (e) {
    $("userError").textContent = e.message;
    $("userError").hidden = false;
  }
});

$("usersBtn").addEventListener("click", openUsers);
$("addUserBtn").addEventListener("click", () => openUserForm(null));

$("testBtn").addEventListener("click", async () => {
  const btn = $("testBtn");
  btn.disabled = true;
  $("testResult").textContent = "A testar…";
  $("testImg").hidden = true;
  try {
    const blob = await api("POST", "/api/settings/test", formSettings());
    $("testImg").src = URL.createObjectURL(blob);
    $("testImg").hidden = false;
    $("testResult").textContent = "✓ Ligação OK";
  } catch (e) {
    $("testResult").textContent = "✗ " + e.message;
  } finally {
    btn.disabled = false;
  }
});

// ---------- Logs & updates (admin only) ----------

// The log lines are slog's text format:
// time=2026-11-03T22:40:02.123+00:00 level=INFO msg="…" key=value…
const LOG_RE = /^time=(\S+) level=(\S+) msg=("(?:[^"\\]|\\.)*"|\S*) ?(.*)$/;
const LOG_LEVELS = { DEBUG: "DEBUG", INFO: "INFO", WARN: "AVISO", ERROR: "ERRO" };
let logLines = [];
let logLevel = "";

function parseLog(line) {
  const m = LOG_RE.exec(line);
  if (!m) return { line, time: "", level: "", msg: line, attrs: "" };
  let msg = m[3];
  if (msg.startsWith('"')) {
    try { msg = JSON.parse(msg); } catch { /* keep it quoted */ }
  }
  return { line, time: m[1], level: m[2], msg, attrs: m[4] };
}

// HH:MM:SS today, else also the day.
function logTime(iso) {
  const d = new Date(iso);
  if (isNaN(d)) return iso;
  const t = d.toLocaleTimeString("pt-PT", { hour12: false });
  return d.toDateString() === new Date().toDateString()
    ? t : d.toLocaleDateString("pt-PT", { day: "2-digit", month: "2-digit" }) + " " + t;
}

async function loadLogs() {
  try {
    logLines = ((await api("GET", "/api/logs")) || []).map(parseLog);
  } catch (e) {
    toast(e.message, true);
    return;
  }
  renderLogs(true);
}

function shownLogs() {
  const q = $("logsSearch").value.trim().toLowerCase();
  return logLines.filter((l) =>
    (!logLevel || l.level === logLevel || (logLevel === "WARN" && l.level === "ERROR")) &&
    (!q || l.line.toLowerCase().includes(q)));
}

function renderLogs(toEnd) {
  const box = $("logs");
  const atEnd = box.scrollHeight - box.scrollTop - box.clientHeight < 40;
  const rows = shownLogs().map((l) => {
    const row = document.createElement("div");
    row.className = "log-row" + (l.level === "WARN" ? " warn" : l.level === "ERROR" ? " bad" : "");
    const time = document.createElement("span");
    time.className = "log-time";
    time.textContent = l.time ? logTime(l.time) : "";
    const level = document.createElement("span");
    level.className = "log-level";
    level.textContent = LOG_LEVELS[l.level] || l.level;
    const msg = document.createElement("span");
    msg.className = "log-msg";
    msg.textContent = l.msg + " ";
    const attrs = document.createElement("span");
    attrs.className = "muted";
    attrs.textContent = l.attrs;
    msg.append(attrs);
    row.append(time, level, msg);
    return row;
  });
  if (!rows.length) {
    const empty = document.createElement("p");
    empty.className = "muted log-empty";
    empty.textContent = logLines.length ? "Nenhuma linha corresponde ao filtro." : "(vazio)";
    rows.push(empty);
  }
  box.replaceChildren(...rows);
  const count = (lvl) => logLines.filter((l) => l.level === lvl).length;
  const warns = count("WARN") + count("ERROR"), errors = count("ERROR");
  document.querySelector('.seg [data-level="WARN"]').textContent = warns ? `Avisos (${warns})` : "Avisos";
  document.querySelector('.seg [data-level="ERROR"]').textContent = errors ? `Erros (${errors})` : "Erros";
  if (toEnd || atEnd) box.scrollTop = box.scrollHeight;
}

document.querySelectorAll(".seg [data-level]").forEach((b) => b.addEventListener("click", () => {
  logLevel = b.dataset.level;
  document.querySelectorAll(".seg [data-level]").forEach((o) => o.setAttribute("aria-pressed", String(o === b)));
  renderLogs(true);
}));
$("logsSearch").addEventListener("input", () => renderLogs(true));
// Enter in the search box must not submit (save) the settings form.
$("logsSearch").addEventListener("keydown", (ev) => { if (ev.key === "Enter") ev.preventDefault(); });
$("logsRefresh").addEventListener("click", loadLogs);
$("logsCopy").addEventListener("click", async () => {
  try {
    await navigator.clipboard.writeText(shownLogs().map((l) => l.line).join("\n"));
    toast("Registos copiados");
  } catch {
    toast("Não foi possível copiar", true);
  }
});
$("logsDownload").addEventListener("click", () => {
  const blob = new Blob([logLines.map((l) => l.line).join("\n") + "\n"], { type: "text/plain" });
  const a = document.createElement("a");
  a.href = URL.createObjectURL(blob);
  a.download = `recorder-${new Date().toISOString().slice(0, 10)}.log`;
  a.click();
  setTimeout(() => URL.revokeObjectURL(a.href), 1000);
});

// The footer notice opens the settings at "Sistema".
$("updateBadge").addEventListener("click", async (ev) => {
  ev.preventDefault();
  if (settingsOpen || await openSettings()) $("s-system").scrollIntoView({ behavior: "smooth" });
});

$("updateBtn").addEventListener("click", async () => {
  try {
    const u = await api("POST", "/api/update/check");
    if (status) {
      // The check starts a moment later: show it already.
      status.update = UPDATE_BUSY.includes(u.state) ? u : { ...u, state: "checking" };
      renderUpdate();
    }
    setTimeout(refreshStatus, 1500);
  } catch (e) {
    toast(e.message, true);
  }
});

// ---------- Wiring ----------

$("settingsBtn").addEventListener("click", () => (settingsOpen ? closeSettings() : openSettings()));
document.querySelectorAll("[data-settings-back]").forEach((b) => b.addEventListener("click", closeSettings));
// The logo and title go back to the recorder (the "home"), e.g. from the
// settings page, without reloading the page.
$("homeLink").addEventListener("click", (ev) => {
  if (ev.ctrlKey || ev.metaKey || ev.shiftKey || ev.button !== 0) return; // new tab/window
  ev.preventDefault();
  document.querySelectorAll("dialog[open]").forEach((d) => d.close());
  if (settingsOpen) closeSettings();
  window.scrollTo({ top: 0, behavior: "smooth" });
});

// The section links scroll without touching the address (it holds #definicoes).
document.querySelectorAll(".settings-nav a").forEach((a) =>
  a.addEventListener("click", (ev) => {
    ev.preventDefault();
    document.querySelector(a.getAttribute("href")).scrollIntoView({ behavior: "smooth", block: "start" });
  }));
document.querySelectorAll("[data-open-settings]").forEach((b) => b.addEventListener("click", openSettings));
document.querySelectorAll("dialog [data-close]").forEach((b) =>
  b.addEventListener("click", () => b.closest("dialog").close()));

document.addEventListener("visibilitychange", () => {
  // A hidden page stops downloading the live view (and frees its place).
  if (document.hidden && live) teardownLive();
  if (!document.hidden && liveWanted && !live) startLive();
  if (!document.hidden) refreshStatus().then(refreshSnapshot);
});
// Mobile browsers restore a page from the back/forward cache without a
// visibilitychange: check the version again so an update reloads it.
window.addEventListener("pageshow", (ev) => {
  if (ev.persisted) refreshStatus().then(refreshSnapshot);
});

setInterval(renderTimer, 500);
setInterval(() => { if (!document.hidden) refreshStatus(); }, STATUS_INTERVAL);
setInterval(() => { if (!updating) refreshSnapshot(); }, SNAPSHOT_INTERVAL);
setInterval(() => { if (!document.hidden) loadRecordings(); }, 60000);
loadMe()
  .then(() => {
    // Reloaded on the settings page: open it again if this user may.
    if (location.hash !== SETTINGS_HASH) return;
    if (me && me.user && !$("app").hidden && !$("settingsBtn").hidden) openSettings();
    else clearSettingsHash();
  })
  .then(() => refreshStatus())
  .then(refreshSnapshot);

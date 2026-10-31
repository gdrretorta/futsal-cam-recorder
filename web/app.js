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
  $("setupNotice").hidden = status.configured;

  const btn = $("recordBtn");
  btn.classList.toggle("stop", active);
  btn.classList.toggle("busy", r.state === "connecting" || r.state === "stopping");
  btn.disabled = pending || r.state === "stopping" || (!active && !status.configured) || status.update.state === "applying";
  btn.setAttribute("aria-label", active ? "Parar gravação" : "Iniciar gravação");
  btn.title = active ? "Parar gravação" : "Iniciar gravação";

  timerStart = r.startedAt ? new Date(r.startedAt) : null;
  renderTimer();

  $("version").textContent = status.version;
  $("updateText").textContent = updateText(status.update);

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

function updateText(u) {
  switch (u.state) {
    case "checking": return "A verificar atualizações…";
    case "downloading": return `A descarregar ${u.latest}…`;
    case "waiting": return `${u.latest} pronta — instala quando não houver gravações`;
    case "applying": return `A instalar ${u.latest}…`;
    case "error": return u.error;
    case "disabled": return "Atualizações automáticas desligadas";
    default: return u.lastCheck && u.latest ? "Atualizado" : "";
  }
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
  if (document.hidden || !status || !status.configured) return;
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
  name.textContent = dateFmt.format(recordingDate(rec));
  if (rec.processing) {
    const tag = document.createElement("span");
    tag.className = "tag";
    tag.textContent = "A processar…";
    name.append(tag);
  }
  const meta = document.createElement("div");
  meta.className = "muted rec-meta";
  meta.textContent = `${rec.name} · ${fmtBytes(rec.size)}`;
  info.append(name, meta);

  const actions = document.createElement("div");
  actions.className = "rec-actions";
  const play = iconButton("play", "Ver", () => openPlayer(rec, url));
  const link = iconButton("link", "Copiar link", () => copyLink(new URL(url, location.href).href));
  const dl = iconButton("download", "Descarregar", () => { location.href = url + "?download=1"; });
  const del = iconButton("trash", "Apagar", () => confirmDelete(rec), "del");
  for (const b of [play, link, dl, del]) b.disabled = !!rec.processing;
  actions.append(play, link, dl, del);

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

function confirmDelete(rec) {
  const dlg = $("confirmDlg");
  $("confirmText").textContent = `${dateFmt.format(recordingDate(rec))} (${rec.name}, ${fmtBytes(rec.size)}). Esta ação não pode ser desfeita.`;
  dlg.returnValue = "";
  dlg.onclose = async () => {
    if (dlg.returnValue !== "ok") return;
    try {
      await api("DELETE", `/api/recordings/${encodeURIComponent(rec.name)}`);
      toast("Gravação apagada");
    } catch (e) {
      toast(e.message, true);
    }
    loadRecordings();
  };
  dlg.showModal();
}

// ---------- Settings ----------

const form = $("settingsForm");
const fields = form.elements;

// The settings are admin-only: log in first when there is no session.
async function openSettings() {
  let admin;
  try {
    admin = await api("GET", "/api/admin");
  } catch (e) {
    toast(e.message, true);
    return;
  }
  showAdminLinks(admin.loggedIn);
  if (!admin.loggedIn) {
    openLogin(admin.passwordSet);
    return;
  }
  let s;
  try {
    s = await api("GET", "/api/settings");
  } catch (e) {
    toast(e.message, true);
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
  fields.adminPassword.value = "";
  $("settingsError").hidden = true;
  $("testResult").textContent = "";
  $("testImg").hidden = true;
  $("settingsDlg").showModal();
}

fields.password.addEventListener("input", () => (fields.password.dataset.touched = "1"));

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
  };
}

form.addEventListener("submit", async (ev) => {
  ev.preventDefault();
  try {
    await api("PUT", "/api/settings", formSettings());
    if (fields.adminPassword.value) {
      await api("PUT", "/api/admin/password", { password: fields.adminPassword.value });
    }
    $("settingsDlg").close();
    toast("Definições guardadas");
    refreshStatus().then(refreshSnapshot);
  } catch (e) {
    if (e.status === 401) {
      // Session expired (or the app restarted after an update).
      $("settingsDlg").close();
      openLogin(true);
      return;
    }
    $("settingsError").textContent = e.message;
    $("settingsError").hidden = false;
  }
});

// ---------- Admin login ----------

const loginForm = $("loginForm");

function openLogin(passwordSet) {
  loginForm.reset();
  const err = $("loginError");
  err.hidden = passwordSet;
  err.textContent = passwordSet ? "" :
    "Ainda não foi definida a palavra-passe de administrador. Defina-a no PC: corra o instalador novamente ou execute \"simple-rtsp-recorder.exe set-admin-password\".";
  loginForm.querySelector("[type=submit]").disabled = !passwordSet;
  $("loginDlg").showModal();
}

loginForm.addEventListener("submit", async (ev) => {
  ev.preventDefault();
  try {
    await api("POST", "/api/admin/login", { password: loginForm.elements.password.value });
    $("loginDlg").close();
    openSettings();
  } catch (e) {
    $("loginError").textContent = e.message;
    $("loginError").hidden = false;
    loginForm.elements.password.select();
  }
});

$("logoutBtn").addEventListener("click", async () => {
  try {
    await api("POST", "/api/admin/logout");
  } catch { /* the cookie is cleared either way */ }
  $("settingsDlg").close();
  showAdminLinks(false);
  toast("Sessão terminada");
});

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

// ---------- Logs & updates ----------

// The logs are admin-only, so their link only shows while logged in.
function showAdminLinks(loggedIn) {
  $("logsBtn").hidden = !loggedIn;
}

async function refreshAdmin() {
  try {
    showAdminLinks((await api("GET", "/api/admin")).loggedIn);
  } catch { /* keep the current state */ }
}

$("logsBtn").addEventListener("click", async () => {
  try {
    const lines = await api("GET", "/api/logs");
    $("logs").textContent = (lines || []).join("\n") || "(vazio)";
    $("logsDlg").showModal();
    $("logs").scrollTop = $("logs").scrollHeight;
  } catch (e) {
    if (e.status === 401) {
      // Session expired (or the app restarted after an update).
      showAdminLinks(false);
      openLogin(true);
      return;
    }
    toast(e.message, true);
  }
});

$("updateBtn").addEventListener("click", async () => {
  try {
    await api("POST", "/api/update/check");
    toast("A verificar atualizações…");
    setTimeout(refreshStatus, 1500);
  } catch (e) {
    toast(e.message, true);
  }
});

// ---------- Wiring ----------

$("settingsBtn").addEventListener("click", openSettings);
document.querySelectorAll("[data-open-settings]").forEach((b) => b.addEventListener("click", openSettings));
document.querySelectorAll("dialog [data-close]").forEach((b) =>
  b.addEventListener("click", () => b.closest("dialog").close()));

document.addEventListener("visibilitychange", () => {
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
refreshStatus().then(refreshSnapshot);
refreshAdmin();

// 小桔云盘前端：纯静态页面，通过 CORS 调用 xiaoju-cloud 服务端 API。
"use strict";

const $ = (sel) => document.querySelector(sel);
const store = {
  get(key) { try { return localStorage.getItem(key) || ""; } catch { return ""; } },
  set(key, v) { try { localStorage.setItem(key, v); } catch { /* 隐私模式下忽略 */ } },
};

// 内置了服务端地址时只需输入令牌；令牌只放在内存里，刷新或关闭页面即失效。
const defaultAPI = (window.XIAOJU_API_URL || "").replace(/\/+$/, "");
const state = {
  api: defaultAPI || store.get("xiaoju.api"),
  token: "",
  folderId: null,
};
try { localStorage.removeItem("xiaoju.token"); } catch { /* 清理旧版本保存的令牌 */ }

class HttpError extends Error {
  constructor(status, message) { super(message); this.status = status; }
}

async function request(method, path, body) {
  let resp;
  try {
    resp = await fetch(state.api + path, {
      method,
      headers: { "X-Xiaoju-Token": state.token, ...(body ? { "Content-Type": "application/json" } : {}) },
      body: body ? JSON.stringify(body) : undefined,
    });
  } catch {
    throw new HttpError(0, "无法连接服务端（地址错误、服务休眠中或未允许跨域）");
  }
  if (resp.status === 204) return null;
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new HttpError(resp.status, errorText(resp.status, data.error));
  return data;
}

function errorText(status, msg) {
  if (status === 401) return "令牌错误";
  if (status === 404) return "不存在或已被删除";
  if (status === 409) return "同名文件已存在";
  return msg || `请求失败（${status}）`;
}

function toast(msg) {
  const el = $("#toast");
  el.textContent = msg;
  el.hidden = false;
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => { el.hidden = true; }, 3500);
}

function fail(err) {
  if (err.status === 401) { logout("令牌错误，请重新输入"); return; }
  toast(err.message);
}

// ---------- 登录 ----------

function openLogin(message) {
  const form = $("#login-form");
  $("#api-field").hidden = !!defaultAPI;
  form.api.disabled = !!defaultAPI; // 隐藏的必填项会阻止表单提交
  form.api.value = state.api;
  form.token.value = "";
  const errEl = $("#login-error");
  errEl.textContent = message || "";
  errEl.hidden = !message;
  if (!$("#login").open) $("#login").showModal();
  (defaultAPI ? form.token : form.api).focus();
}

function logout(message) {
  state.token = "";
  stopBackupPolling();
  $("#app").hidden = true;
  $("#logout-btn").hidden = true;
  $("#rows").replaceChildren();
  $("#preview").close();
  openLogin(message);
}

$("#logout-btn").addEventListener("click", () => logout());
// 未登录时不允许用 Esc 关掉登录框
$("#login").addEventListener("cancel", (e) => { if (!state.token) e.preventDefault(); });
$("#login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const form = e.target;
  const api = defaultAPI || form.api.value.trim().replace(/\/+$/, "");
  const token = form.token.value.trim();
  const prev = { api: state.api, token: state.token };
  Object.assign(state, { api, token });
  try {
    await request("GET", "/api/folders/root/children");
  } catch (err) {
    Object.assign(state, prev);
    const errEl = $("#login-error");
    errEl.textContent = err.message;
    errEl.hidden = false;
    return;
  }
  if (!defaultAPI) store.set("xiaoju.api", api);
  $("#login").close();
  $("#logout-btn").hidden = false;
  startBackupPolling();
  load();
});

// ---------- 备份进度 ----------

const BACKUP_POLL_MS = 30_000;
let backupTimer = null;

function startBackupPolling() {
  stopBackupPolling();
  refreshBackup();
  backupTimer = setInterval(() => { if (!document.hidden) refreshBackup(); }, BACKUP_POLL_MS);
}

function stopBackupPolling() {
  clearInterval(backupTimer);
  backupTimer = null;
  $("#backup-badge").hidden = true;
}

async function refreshBackup() {
  if (!state.token) return;
  const el = $("#backup-badge");
  let st;
  try {
    st = await request("GET", "/api/backup/status");
  } catch {
    el.hidden = true; // 旧版服务端没有这个接口，或网络暂时不通：不显示
    return;
  }
  if (!state.token) return; // 请求期间已退出
  renderBackup(el, st);
  el.hidden = false;
}

// 手机上没有悬停提示，点一下显示详情
$("#backup-badge").addEventListener("click", (e) => { if (e.currentTarget.title) toast(e.currentTarget.title); });

function renderBackup(el, st) {
  el.className = "backup-badge";
  el.replaceChildren();
  if (!st.enabled) {
    el.classList.add("warn");
    el.textContent = st.error ? "⚠️ 备份异常" : "⚠️ 未启用备份";
    el.title = st.error
      ? `备份频道连接失败，新文件暂时只存了一份：\n${st.error}`
      : "服务端没有配置备份频道，文件只存了一份";
    return;
  }
  const detail = `已备份 ${st.backed_up} / ${st.parts} 个分块（${formatSize(st.backed_up_bytes)} / ${formatSize(st.bytes)}）`;
  if (st.backed_up >= st.parts) {
    el.classList.add("done");
    el.textContent = "✅ 已双备份";
    el.title = `${detail}\n所有文件都在两个 Telegram 账号的频道里各存了一份`;
    return;
  }
  // 按字节算进度；没完成时最多显示 99%
  const pct = st.bytes > 0 ? Math.min(99, Math.floor((st.backed_up_bytes / st.bytes) * 100)) : 0;
  const meter = document.createElement("span");
  meter.className = "meter";
  const fill = document.createElement("i");
  fill.style.width = `${pct}%`;
  meter.append(fill);
  el.append(meter, `备份中 ${pct}%`);
  el.title = `${detail}\n后台正在自动复制到备份频道，关掉页面也会继续`;
}

// ---------- 目录浏览 ----------

function currentFolderFromHash() {
  const m = location.hash.match(/^#\/f\/([0-9a-f-]{36})$/i);
  return m ? m[1] : null;
}

async function crumbsFor(id) {
  const chain = [];
  for (let cur = id, guard = 0; cur && guard < 64; guard++) {
    const f = await request("GET", `/api/files/${cur}`);
    chain.unshift({ id: cur, name: f.name });
    cur = f.parent_id;
  }
  return chain;
}

function renderCrumbs(chain) {
  const nav = $("#crumbs");
  nav.replaceChildren();
  const items = [{ id: null, name: "全部文件" }, ...chain];
  items.forEach((c, i) => {
    if (i > 0) nav.append(Object.assign(document.createElement("span"), { className: "sep", textContent: "/" }));
    const a = document.createElement("a");
    a.href = c.id ? `#/f/${c.id}` : "#/";
    a.textContent = c.name;
    nav.append(a);
  });
}

async function load() {
  if (!state.api || !state.token) { openLogin(); return; }
  $("#app").hidden = false;
  state.folderId = currentFolderFromHash();
  $("#loading").hidden = false;
  $("#empty").hidden = true;
  $("#rows").replaceChildren();
  try {
    const [chain, list] = await Promise.all([
      state.folderId ? crumbsFor(state.folderId) : [],
      request("GET", `/api/folders/${state.folderId || "root"}/children`),
    ]);
    renderCrumbs(chain);
    renderRows(list.items);
  } catch (err) {
    if (err.status === 404 && state.folderId) { location.hash = "#/"; return; }
    fail(err);
  } finally {
    $("#loading").hidden = true;
  }
}

function renderRows(items) {
  const rows = $("#rows");
  rows.replaceChildren(...items.map(rowFor));
  $("#empty").hidden = items.length > 0;
}

function rowFor(f) {
  const tr = document.createElement("tr");
  const isFolder = f.type === "folder";

  const name = document.createElement("td");
  name.className = "name";
  name.title = f.name;
  const icon = Object.assign(document.createElement("span"), { className: "icon", textContent: iconFor(f) });
  const label = document.createElement(isFolder ? "a" : "span");
  label.textContent = f.name;
  if (isFolder) {
    label.href = `#/f/${f.id}`;
  } else {
    label.addEventListener("click", () => (previewKind(f.mime) ? preview(f) : download(f)));
  }
  name.append(icon, label);

  const size = Object.assign(document.createElement("td"), { className: "size", textContent: isFolder ? "" : formatSize(f.size) });
  const date = Object.assign(document.createElement("td"), { className: "date", textContent: formatDate(f.updated_at) });

  const ops = document.createElement("td");
  ops.className = "ops";
  const btn = (text, fn, cls = "") => {
    const b = Object.assign(document.createElement("button"), { type: "button", textContent: text, className: `link ${cls}` });
    b.addEventListener("click", fn);
    return b;
  };
  if (!isFolder) {
    if (previewKind(f.mime)) ops.append(btn("预览", () => preview(f), "wide"));
    ops.append(btn("下载", () => download(f)));
  }
  ops.append(btn("删除", () => remove(f), "danger"));

  tr.append(name, size, date, ops);
  return tr;
}

function iconFor(f) {
  if (f.type === "folder") return "📁";
  const m = f.mime || "";
  if (m.startsWith("image/")) return "🖼️";
  if (m.startsWith("video/")) return "🎬";
  if (m.startsWith("audio/")) return "🎵";
  if (m === "application/pdf") return "📕";
  if (/zip|rar|7z|tar|gzip/.test(m)) return "🗜️";
  if (m.startsWith("text/")) return "📄";
  return "📦";
}

function formatSize(n) {
  if (n < 1024) return `${n} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let v = n;
  let i = -1;
  do { v /= 1024; i++; } while (v >= 1024 && i < units.length - 1);
  return `${v.toFixed(v < 10 ? 1 : 0)} ${units[i]}`;
}

function formatDate(s) {
  const d = new Date(s);
  const pad = (x) => String(x).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

// ---------- 操作 ----------

$("#mkdir-btn").addEventListener("click", async () => {
  const name = prompt("文件夹名称");
  if (!name || !name.trim()) return;
  try {
    await request("POST", "/api/folders", { name: name.trim(), parent_id: state.folderId });
    load();
  } catch (err) { fail(err); }
});

async function signedURL(f, inline) {
  const { url } = await request("POST", `/api/files/${f.id}/link`);
  return state.api + url + (inline ? "&inline=1" : "");
}

async function download(f) {
  try {
    const a = Object.assign(document.createElement("a"), { href: await signedURL(f, false), rel: "noopener" });
    document.body.append(a);
    a.click();
    a.remove();
  } catch (err) { fail(err); }
}

function previewKind(mime = "") {
  if (/^image\/(png|jpe?g|gif|webp|avif|bmp|svg\+xml)$/.test(mime)) return "img";
  if (/^video\/(mp4|webm|ogg|quicktime)$/.test(mime)) return "video";
  if (/^audio\//.test(mime)) return "audio";
  if (mime === "application/pdf" || mime.startsWith("text/")) return "tab";
  return null;
}

async function preview(f) {
  const kind = previewKind(f.mime);
  // 新标签页要在点击的同步阶段打开，否则会被浏览器拦截
  const tab = kind === "tab" ? window.open("about:blank", "_blank") : null;
  let url;
  try {
    url = await signedURL(f, true);
  } catch (err) {
    tab?.close();
    fail(err);
    return;
  }
  if (tab) { tab.opener = null; tab.location = url; return; }
  const el = document.createElement(kind);
  el.src = url;
  if (kind !== "img") { el.controls = true; el.autoplay = true; }
  else el.alt = f.name;
  $("#preview-title").textContent = f.name;
  $("#preview-body").replaceChildren(el);
  $("#preview").showModal();
}

$("#preview-close").addEventListener("click", () => $("#preview").close());
// 关闭时清空内容以停止视频播放（Esc 关闭也会触发）
$("#preview").addEventListener("close", () => $("#preview-body").replaceChildren());

async function remove(f) {
  const isFolder = f.type === "folder";
  let permanent = false;
  if (isFolder) {
    if (!confirm(`把文件夹“${f.name}”移入回收站？`)) return;
  } else {
    if (!confirm(`删除“${f.name}”？`)) return;
    permanent = confirm("是否永久删除？\n\n确定：立即永久删除（同时删除 Telegram 中的数据，无法恢复）\n取消：仅移入回收站");
  }
  try {
    await request("DELETE", `/api/files/${f.id}${permanent ? "?permanent=1" : ""}`);
    toast(permanent ? "已永久删除" : "已移入回收站");
    load();
    if (permanent) refreshBackup();
  } catch (err) { fail(err); }
}

// ---------- 上传 ----------

const CONCURRENCY = 2;
// 网络中断、服务端出错（多为 Telegram 限流）时的重试间隔（秒）
const RETRY_DELAYS = [5, 20, 60];
const ALBUM = "相册";

const queue = [];
let active = 0;
let stats = null; // 本轮上传的统计：{ uploaded, skipped, failed, sync }

const sleep = (s) => new Promise((r) => setTimeout(r, s * 1000));

// items: [{ file, name }]；sync=true 表示来自“同步相册”，同名已存在视为已同步
function enqueue(items, parent = state.folderId, sync = false) {
  stats ??= { uploaded: 0, skipped: 0, failed: 0, sync: false };
  stats.sync ||= sync;
  for (const { file, name } of items) queue.push({ file, name: name || file.name, parent, sync });
  pump();
}

function pump() {
  while (active < CONCURRENCY && queue.length) {
    active++;
    runJob(queue.shift()).finally(() => {
      active--;
      if (active || queue.length) pump();
      else finishBatch();
    });
  }
  updateSummary();
}

function updateSummary() {
  if (!stats) return;
  const s = stats;
  const parts = [`已上传 ${s.uploaded}`];
  if (queue.length + active) parts.push(`剩余 ${queue.length + active}`);
  if (s.skipped) parts.push(`跳过 ${s.skipped}（已存在）`);
  if (s.failed) parts.push(`失败 ${s.failed}`);
  $("#upload-summary").textContent = (s.sync ? "同步相册：" : "上传：") + parts.join(" · ");
  $("#uploads").hidden = false;
}

function finishBatch() {
  const s = stats;
  stats = null;
  if (!s) return;
  let msg = `${s.sync ? "同步" : "上传"}完成：上传 ${s.uploaded} 个`;
  if (s.skipped) msg += `，跳过 ${s.skipped} 个（已存在）`;
  if (s.failed) msg += `，失败 ${s.failed} 个（见上方列表）`;
  toast(msg);
  $("#upload-summary").textContent = msg;
  refreshBackup();
  if (!s.failed) setTimeout(() => { if (!stats) $("#uploads").hidden = true; }, 4000);
}

let reloadTimer;
function scheduleReload() {
  clearTimeout(reloadTimer);
  reloadTimer = setTimeout(load, 1500);
}

async function runJob(job) {
  const row = document.createElement("div");
  row.className = "upload";
  const name = Object.assign(document.createElement("span"), { className: "uname", textContent: job.name });
  const status = Object.assign(document.createElement("span"), { className: "ustatus", textContent: "上传中" });
  const bar = Object.assign(document.createElement("progress"), { max: 1, value: 0 });
  row.append(name, status, bar);
  $("#upload-rows").append(row);

  for (let attempt = 0; ; attempt++) {
    try {
      await uploadOne(job, status, bar);
      stats.uploaded++;
      status.textContent = "完成";
      bar.value = 1;
      setTimeout(() => row.remove(), 1500);
      if (job.parent === state.folderId) scheduleReload();
      return;
    } catch (err) {
      if (err.status === 401) {
        queue.length = 0;
        row.remove();
        logout("令牌错误，请重新输入");
        return;
      }
      // 同步时，或重试时（上次其实已成功但响应丢失），同名已存在都算已同步
      if (err.status === 409 && (job.sync || attempt > 0)) {
        stats.skipped++;
        row.remove();
        return;
      }
      const retryable = err.status === 0 || err.status >= 500;
      if (retryable && attempt < RETRY_DELAYS.length) {
        status.textContent = `${err.message}，${RETRY_DELAYS[attempt]} 秒后重试`;
        bar.value = 0;
        await sleep(RETRY_DELAYS[attempt]);
        if (!state.token) { row.remove(); return; } // 已退出登录
        continue;
      }
      stats.failed++;
      status.textContent = err.message;
      status.classList.add("error");
      return;
    }
  }
}

function uploadOne({ file, name, parent }, status, bar) {
  return new Promise((resolve, reject) => {
    const q = new URLSearchParams({ name });
    if (parent) q.set("parent_id", parent);
    const xhr = new XMLHttpRequest();
    xhr.open("POST", `${state.api}/api/files?${q}`);
    xhr.setRequestHeader("X-Xiaoju-Token", state.token);
    xhr.setRequestHeader("Content-Type", file.type || "application/octet-stream");
    xhr.upload.onprogress = (e) => {
      if (!e.lengthComputable) return;
      bar.value = e.loaded / e.total;
      status.textContent = e.loaded < e.total
        ? `${formatSize(e.loaded)} / ${formatSize(e.total)}`
        : "加密并写入 Telegram…";
    };
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) return resolve();
      let msg;
      try { msg = JSON.parse(xhr.responseText).error; } catch { /* 非 JSON */ }
      reject(new HttpError(xhr.status, errorText(xhr.status, msg)));
    };
    xhr.onerror = () => reject(new HttpError(0, "上传中断（网络错误或连接超时）"));
    xhr.send(file);
  });
}

$("#file-input").addEventListener("change", (e) => {
  enqueue([...e.target.files].map((file) => ({ file })));
  e.target.value = "";
});

// ---------- 同步相册 ----------

// 去掉自动加的序号：“IMG_1 (2).jpg” → “IMG_1.jpg”
const baseName = (n) => n.replace(/ \(\d+\)(?=\.[^.]*$|$)/, "");
const withSuffix = (n, i) => n.replace(/(\.[^.]*)?$/, (ext) => ` (${i})${ext}`);

// 把选中的照片 / 视频传到根目录的“相册”文件夹。已传过的（文件名 + 大小相同）自动跳过，
// 所以每次多选一些也不会重复上传；同名但内容不同的照片自动改名为“名字 (2)”。
async function syncAlbum(files) {
  if (!files.length) return;
  toast(`正在检查 ${files.length} 个文件是否已同步…`);
  try {
    const root = await request("GET", "/api/folders/root/children");
    let album = root.items.find((f) => f.type === "folder" && f.name === ALBUM);
    album ??= await request("POST", "/api/folders", { name: ALBUM, parent_id: null });
    const { items } = await request("GET", `/api/folders/${album.id}/children`);
    const synced = new Set(items.filter((f) => f.type === "file").map((f) => `${baseName(f.name)}\0${f.size}`));
    const taken = new Set(items.map((f) => f.name));

    const todo = [];
    let skipped = 0;
    for (const file of files) {
      const key = `${baseName(file.name)}\0${file.size}`;
      if (synced.has(key)) { skipped++; continue; }
      synced.add(key); // 同一批里选了两次同一张
      let name = file.name;
      for (let i = 2; taken.has(name); i++) name = withSuffix(file.name, i);
      taken.add(name);
      todo.push({ file, name });
    }
    if (!todo.length) {
      toast(`没有新的照片：选中的 ${skipped} 个都已经同步过了`);
      return;
    }
    if (state.folderId !== album.id) location.hash = `#/f/${album.id}`;
    enqueue(todo, album.id, true);
    stats.skipped += skipped;
    updateSummary();
  } catch (err) {
    fail(err);
  }
}

$("#album-input").addEventListener("change", (e) => {
  const files = [...e.target.files];
  e.target.value = "";
  syncAlbum(files);
});

const drop = $("#drop");
drop.addEventListener("dragover", (e) => { e.preventDefault(); drop.classList.add("dragover"); });
drop.addEventListener("dragleave", () => drop.classList.remove("dragover"));
drop.addEventListener("drop", (e) => {
  e.preventDefault();
  drop.classList.remove("dragover");
  const files = [...e.dataTransfer.files];
  if (files.length) enqueue(files.map((file) => ({ file })));
});

window.addEventListener("hashchange", load);
load();

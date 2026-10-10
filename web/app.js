// 小桔云盘前端：纯静态页面，通过 CORS 调用 xiaoju-cloud 服务端 API。
"use strict";

const $ = (sel) => document.querySelector(sel);
const store = {
  get(key) { try { return localStorage.getItem(key) || ""; } catch { return ""; } },
  set(key, v) { try { localStorage.setItem(key, v); } catch { /* 隐私模式下忽略 */ } },
};

const state = {
  api: (store.get("xiaoju.api") || window.XIAOJU_API_URL || "").replace(/\/+$/, ""),
  token: store.get("xiaoju.token"),
  folderId: null,
};

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
  if (err.status === 401) { openSettings("令牌错误，请重新填写"); return; }
  toast(err.message);
}

// ---------- 设置 ----------

function openSettings(message) {
  const form = $("#settings-form");
  form.api.value = state.api;
  form.token.value = state.token;
  const errEl = $("#settings-error");
  errEl.textContent = message || "";
  errEl.hidden = !message;
  $("#settings").showModal();
}

$("#settings-btn").addEventListener("click", () => openSettings());
$("#settings-form").addEventListener("submit", async (e) => {
  if (e.submitter?.value !== "save") return;
  e.preventDefault();
  const form = e.target;
  const prev = { api: state.api, token: state.token };
  state.api = form.api.value.trim().replace(/\/+$/, "");
  state.token = form.token.value.trim();
  try {
    await request("GET", "/api/folders/root/children");
  } catch (err) {
    Object.assign(state, prev);
    const errEl = $("#settings-error");
    errEl.textContent = err.message;
    errEl.hidden = false;
    return;
  }
  store.set("xiaoju.api", state.api);
  store.set("xiaoju.token", state.token);
  $("#settings").close();
  load();
});

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
  if (!state.api || !state.token) { openSettings(); return; }
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
  } catch (err) { fail(err); }
}

// ---------- 上传 ----------

const queue = [];
let uploading = false;

function enqueue(files) {
  const parent = state.folderId;
  for (const file of files) {
    const row = document.createElement("div");
    row.className = "upload";
    const name = Object.assign(document.createElement("span"), { className: "uname", textContent: file.name });
    const status = Object.assign(document.createElement("span"), { className: "ustatus", textContent: "等待中" });
    const bar = Object.assign(document.createElement("progress"), { max: 1, value: 0 });
    row.append(name, status, bar);
    $("#uploads").append(row);
    queue.push({ file, parent, status, bar, row });
  }
  $("#uploads").hidden = false;
  if (!uploading) drain();
}

async function drain() {
  uploading = true;
  while (queue.length) {
    const job = queue.shift();
    try {
      await uploadOne(job);
      job.status.textContent = "完成";
      job.bar.value = 1;
      setTimeout(() => {
        job.row.remove();
        if (!$("#uploads").children.length) $("#uploads").hidden = true;
      }, 2500);
      if (job.parent === state.folderId) load();
    } catch (err) {
      job.status.textContent = err.message;
      job.status.classList.add("error");
      if (err.status === 401) openSettings("令牌错误，请重新填写");
    }
  }
  uploading = false;
}

function uploadOne({ file, parent, status, bar }) {
  return new Promise((resolve, reject) => {
    const q = new URLSearchParams({ name: file.name });
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
    status.textContent = "上传中";
    xhr.send(file);
  });
}

$("#file-input").addEventListener("change", (e) => {
  enqueue(e.target.files);
  e.target.value = "";
});

const drop = $("#drop");
drop.addEventListener("dragover", (e) => { e.preventDefault(); drop.classList.add("dragover"); });
drop.addEventListener("dragleave", () => drop.classList.remove("dragover"));
drop.addEventListener("drop", (e) => {
  e.preventDefault();
  drop.classList.remove("dragover");
  const files = [...e.dataTransfer.files];
  if (files.length) enqueue(files);
});

window.addEventListener("hashchange", load);
load();

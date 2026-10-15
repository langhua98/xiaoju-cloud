// 小桔云盘前端：纯静态页面，通过 CORS 调用 xiaoju-cloud 服务端 API。
"use strict";

const $ = (sel) => document.querySelector(sel);
const store = {
  get(key) { try { return localStorage.getItem(key) || ""; } catch { return ""; } },
  set(key, v) { try { localStorage.setItem(key, v); } catch { /* 隐私模式下忽略 */ } },
};

// 内置了服务端地址时只需输入令牌；令牌和会话只放在内存里，刷新或关闭页面即失效。
// 登录分两步：令牌正确后，如果已注册 Face ID / 指纹设备，还要刷脸，通过后服务端签发会话（12 小时）。
const defaultAPI = (window.XIAOJU_API_URL || "").replace(/\/+$/, "");
const state = {
  api: defaultAPI || store.get("xiaoju.api"),
  token: "",   // 访问令牌（登录接口要用）
  session: "", // 刷脸后签发的会话；有值即表示已登录
  folderId: null,
};
try { localStorage.removeItem("xiaoju.token"); } catch { /* 清理旧版本保存的令牌 */ }

class HttpError extends Error {
  constructor(status, message, code) { super(message); this.status = status; this.code = code; }
}

function authHeaders() {
  const h = {};
  if (state.token) h["X-Xiaoju-Token"] = state.token;
  if (state.session) h["X-Xiaoju-Session"] = state.session;
  return h;
}

async function request(method, path, body) {
  let resp;
  try {
    resp = await fetch(state.api + path, {
      method,
      headers: { ...authHeaders(), ...(body ? { "Content-Type": "application/json" } : {}) },
      body: body ? JSON.stringify(body) : undefined,
    });
  } catch {
    throw new HttpError(0, "无法连接服务端（地址错误、服务休眠中或未允许跨域）");
  }
  if (resp.status === 204) return null;
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new HttpError(resp.status, errorText(resp.status, data.error, data.code), data.code);
  return data;
}

function errorText(status, msg, code) {
  if (code === "passkey_required") return "需要刷脸验证，请重新登录";
  if (status === 401) return state.session ? "登录已过期，请重新验证" : "令牌错误";
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
  if (err.status === 401) { logout(err.message); return; }
  toast(err.message);
}

// ---------- 过渡动画 ----------

const sleepMs = (ms) => new Promise((r) => setTimeout(r, ms));

// 给元素加一个带动画的 class，动画结束（或超时兜底）后移除并 resolve
function playClass(el, cls, timeout = 500) {
  return new Promise((resolve) => {
    let done = false;
    const finish = () => {
      if (done) return;
      done = true;
      el.removeEventListener("animationend", onEnd);
      el.classList.remove(cls);
      resolve();
    };
    const onEnd = (e) => { if (e.target === el) finish(); };
    el.classList.remove(cls);
    void el.offsetWidth; // 重新触发同名动画
    el.classList.add(cls);
    el.addEventListener("animationend", onEnd);
    setTimeout(finish, timeout);
  });
}

async function closeDialog(dialog) {
  if (!dialog.open) return;
  await playClass(dialog, "closing", 300);
  dialog.close();
}

// 页面顶部细进度条：start() 后先快后慢地涨到 90%，done() 时补满再淡出。支持并发调用。
const progress = (() => {
  const wrap = $("#page-progress");
  const bar = wrap.querySelector("i");
  let active = 0;
  let width = 0;
  let timer = null;
  let hideTimer = null;
  const set = (w) => { width = w; bar.style.width = `${w}%`; };
  return {
    start() {
      if (active++ > 0) return;
      clearTimeout(hideTimer);
      wrap.hidden = false;
      bar.style.transition = "none";
      bar.style.opacity = "1";
      set(0);
      void bar.offsetWidth;
      bar.style.transition = "";
      set(15);
      timer = setInterval(() => set(width + (90 - width) * 0.12), 300);
    },
    done() {
      if (active === 0 || --active > 0) return;
      clearInterval(timer);
      set(100);
      bar.style.opacity = "0";
      hideTimer = setTimeout(() => { wrap.hidden = true; }, 500);
    },
  };
})();

// ---------- 登录 ----------

const LOGIN_HINT = "令牌不会保存，刷新或关闭页面后需要重新输入。";
// 免费 HF Space 闲置后会休眠，第一次请求要等它唤醒
const WAKE_HINT = "服务端可能在休眠，正在唤醒，首次访问约需 30 秒…";
const WAKE_HINT_DELAY_MS = 4000;

function setButtonBusy(btn, busy, busyLabel, idleLabel) {
  btn.classList.toggle("loading", busy);
  btn.disabled = busy;
  if (busyLabel) btn.querySelector(".label").textContent = busy ? busyLabel : idleLabel;
}

function setLoginBusy(busy) {
  const form = $("#login-form");
  $("#login").classList.toggle("busy", busy);
  setButtonBusy($("#login-btn"), busy, "登录中…", "登录");
  form.token.readOnly = busy;
  if (!defaultAPI) form.api.readOnly = busy;
}

function setLoginHint(text) {
  const hint = $("#login-hint");
  if (hint.textContent === text) return;
  hint.style.opacity = "0";
  setTimeout(() => { hint.textContent = text; hint.style.opacity = ""; }, 150);
}

function showLoginStep(step) {
  const form = $("#login-form");
  const face = $("#face-step");
  form.hidden = step !== "token";
  face.hidden = step !== "face";
  const shown = step === "token" ? form : face;
  playClass(shown, "step-in", 400);
}

function openLogin(message) {
  const form = $("#login-form");
  const btn = $("#login-btn");
  $("#api-field").hidden = !!defaultAPI;
  form.api.disabled = !!defaultAPI; // 隐藏的必填项会阻止表单提交
  form.api.value = state.api;
  form.token.value = "";
  setLoginBusy(false);
  btn.classList.remove("done");
  $("#login-hint").textContent = LOGIN_HINT;
  const errEl = $("#login-error");
  errEl.textContent = message || "";
  errEl.hidden = !message;
  form.hidden = false;
  $("#face-step").hidden = true;
  if (!$("#login").open) $("#login").showModal();
  (defaultAPI ? form.token : form.api).focus();
}

async function logout(message) {
  state.token = "";
  state.session = "";
  faceOptions = null;
  regOptions = null;
  stopBackupPolling();
  $("#logout-btn").hidden = true;
  $("#security-btn").hidden = true;
  $("#preview").close();
  $("#security").close();
  // 主界面淡出后再弹出登录框
  const app = $("#app");
  if (!app.hidden) {
    app.style.transition = "opacity 0.2s ease";
    app.style.opacity = "0";
    await sleepMs(200);
    app.style.transition = app.style.opacity = "";
  }
  app.hidden = true;
  $("#rows").replaceChildren();
  openLogin(message);
}

$("#logout-btn").addEventListener("click", () => logout());
// 未登录时不允许用 Esc 关掉登录框
$("#login").addEventListener("cancel", (e) => { if (!state.session) e.preventDefault(); });

// 第一步：校验令牌。已注册设备时进入刷脸，否则直接拿到会话
$("#login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const form = e.target;
  const api = defaultAPI || form.api.value.trim().replace(/\/+$/, "");
  const token = form.token.value.trim();
  if ($("#login-btn").disabled) return;
  const prev = { api: state.api, token: state.token };
  Object.assign(state, { api, token, session: "" });

  $("#login-error").hidden = true;
  setLoginBusy(true);
  const wakeTimer = setTimeout(() => setLoginHint(WAKE_HINT), WAKE_HINT_DELAY_MS);
  let res;
  try {
    res = await request("POST", "/api/auth/session");
  } catch (err) {
    Object.assign(state, prev);
    setLoginBusy(false);
    setLoginHint(LOGIN_HINT);
    const errEl = $("#login-error");
    errEl.textContent = err.message;
    errEl.hidden = false;
    playClass(form, "shake", 450);
    form.token.select();
    return;
  } finally {
    clearTimeout(wakeTimer);
  }
  if (!defaultAPI) store.set("xiaoju.api", api);
  setLoginBusy(false);

  if (res.passkey_required) {
    enterFaceStep();
    return;
  }
  state.session = res.session;
  await finishLogin($("#login-btn"));
  // 还没开启刷脸：提示开启（只在服务端支持时）
  if (res.passkeys_available && webauthnSupported()) {
    setTimeout(() => openSecurity("建议开启：开启后登录需要「令牌 + 刷脸」，别人拿到令牌也打不开你的网盘。"), 700);
  }
});

// 登录成功：按钮显示“✓ 登录成功”，稍停后登录框淡出、主界面淡入
async function finishLogin(btn) {
  btn.disabled = true;
  btn.classList.add("done");
  const label = btn.querySelector(".label");
  const idle = label.textContent;
  label.textContent = "✓ 登录成功";
  await sleepMs(350);
  await closeDialog($("#login"));
  btn.disabled = false;
  btn.classList.remove("done");
  label.textContent = idle;
  $("#face-step").classList.remove("ok", "verifying");
  $("#logout-btn").hidden = false;
  $("#security-btn").hidden = false;
  startBackupPolling();
  load();
}

// ---------- 刷脸 / 指纹（通行密钥）----------
// 人脸比对在设备上由系统完成，服务端只保存公钥、校验签名，不接触任何人脸数据。

const webauthnSupported = () => !!window.PublicKeyCredential && !!navigator.credentials;

const b64u = {
  enc(buf) {
    let s = "";
    for (const b of new Uint8Array(buf)) s += String.fromCharCode(b);
    return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  },
  dec(str) {
    const s = str.replace(/-/g, "+").replace(/_/g, "/");
    return Uint8Array.from(atob(s + "=".repeat((4 - (s.length % 4)) % 4)), (c) => c.charCodeAt(0));
  },
};

const decodeDescriptors = (list) => (list || []).map((c) => ({ ...c, id: b64u.dec(c.id) }));

function decodeRequestOptions(pk) {
  return { ...pk, challenge: b64u.dec(pk.challenge), allowCredentials: decodeDescriptors(pk.allowCredentials) };
}

function decodeCreationOptions(pk) {
  return {
    ...pk,
    challenge: b64u.dec(pk.challenge),
    user: { ...pk.user, id: b64u.dec(pk.user.id) },
    excludeCredentials: decodeDescriptors(pk.excludeCredentials),
  };
}

function encodeCredential(c) {
  const r = c.response;
  const out = {
    id: c.id,
    rawId: b64u.enc(c.rawId),
    type: c.type,
    authenticatorAttachment: c.authenticatorAttachment || undefined,
    clientExtensionResults: c.getClientExtensionResults ? c.getClientExtensionResults() : {},
    response: { clientDataJSON: b64u.enc(r.clientDataJSON) },
  };
  if (r.attestationObject) {
    out.response.attestationObject = b64u.enc(r.attestationObject);
    out.response.transports = r.getTransports ? r.getTransports() : [];
  } else {
    out.response.authenticatorData = b64u.enc(r.authenticatorData);
    out.response.signature = b64u.enc(r.signature);
    if (r.userHandle) out.response.userHandle = b64u.enc(r.userHandle);
  }
  return out;
}

function webauthnError(err) {
  if (err.name === "NotAllowedError") return "已取消，或验证没有通过";
  if (err.name === "InvalidStateError") return "这台设备已经注册过了";
  if (err.name === "SecurityError") return "网址不对：请从网盘的正式网址打开";
  if (err.code === "verification_failed") return "验证失败：这台设备可能还没注册，或已被移除";
  if (err.code === "ceremony_expired") return "验证超时，请再试一次";
  if (err.code === "add_code_required") return "添加码不对或已过期";
  if (err.code === "passkeys_unavailable") return "服务端没有开启刷脸功能";
  return err.message || "出错了";
}

function deviceName() {
  const ua = navigator.userAgent;
  const os = /iPhone/.test(ua) ? "iPhone" : /iPad/.test(ua) ? "iPad" : /Android/.test(ua) ? "Android 手机"
    : /Macintosh/.test(ua) ? "Mac" : /Windows/.test(ua) ? "Windows 电脑" : /Linux/.test(ua) ? "Linux 电脑" : "设备";
  const browser = /Edg\//.test(ua) ? "Edge" : /Chrome\//.test(ua) ? "Chrome" : /Firefox\//.test(ua) ? "Firefox"
    : /Safari\//.test(ua) ? "Safari" : "";
  return browser ? `${os} · ${browser}` : os;
}

// 预取的刷脸参数：进入刷脸步骤时就请求好，点按钮时直接调起系统验证
// （Safari 要求验证由点击直接触发，中间不能先等网络请求太久）
let faceOptions = null;

async function prefetchFaceOptions() {
  faceOptions = null;
  try {
    const res = await request("POST", "/api/auth/passkey/login/begin");
    faceOptions = { ...res, at: Date.now() };
  } catch (err) {
    showFaceError(webauthnError(err));
  }
}

function enterFaceStep() {
  $("#face-error").hidden = true;
  $("#face-step").classList.remove("ok", "verifying");
  $("#face-add").open = false;
  $("#add-code").value = "";
  if (!webauthnSupported()) {
    showLoginStep("face");
    showFaceError("这个浏览器不支持 Face ID / 指纹登录，请用 Safari 或 Chrome 打开（不要在微信里打开）");
    $("#face-btn").disabled = true;
    return;
  }
  $("#face-btn").disabled = false;
  showLoginStep("face");
  prefetchFaceOptions();
  $("#face-btn").focus();
}

function showFaceError(msg) {
  const el = $("#face-error");
  el.textContent = msg;
  el.hidden = false;
  playClass($("#face-step"), "shake", 450);
}

// 第二步：刷脸解锁
$("#face-btn").addEventListener("click", async () => {
  const btn = $("#face-btn");
  if (btn.disabled) return;
  $("#face-error").hidden = true;
  const step = $("#face-step");
  setButtonBusy(btn, true, "正在验证…", "使用 Face ID / 指纹解锁");
  step.classList.add("verifying");
  try {
    let opts = faceOptions;
    faceOptions = null;
    if (!opts || Date.now() - opts.at > 120_000) opts = await request("POST", "/api/auth/passkey/login/begin");
    const cred = await navigator.credentials.get({ publicKey: decodeRequestOptions(opts.options.publicKey) });
    const res = await request("POST", `/api/auth/passkey/login/finish?ceremony=${encodeURIComponent(opts.ceremony)}`, encodeCredential(cred));
    state.session = res.session;
    step.classList.remove("verifying");
    step.classList.add("ok");
    setButtonBusy(btn, false, "正在验证…", "使用 Face ID / 指纹解锁");
    await finishLogin(btn);
  } catch (err) {
    step.classList.remove("verifying");
    setButtonBusy(btn, false, "正在验证…", "使用 Face ID / 指纹解锁");
    if (err.status === 401 && !err.code) { logout("令牌已失效，请重新输入"); return; }
    showFaceError(webauthnError(err));
    prefetchFaceOptions();
  }
});

// 没注册过的设备：输入另一台已登录设备生成的添加码，注册本机后直接登录
$("#face-register-btn").addEventListener("click", async () => {
  const btn = $("#face-register-btn");
  const code = $("#add-code").value.trim();
  if (btn.disabled) return;
  if (!code) { $("#add-code").focus(); return; }
  $("#face-error").hidden = true;
  setButtonBusy(btn, true, "注册中…", "注册本机");
  try {
    state.session = await registerThisDevice(code);
    $("#face-step").classList.add("ok");
    setButtonBusy(btn, false, "注册中…", "注册本机");
    toast("本机已开启刷脸登录");
    await finishLogin($("#face-btn"));
  } catch (err) {
    setButtonBusy(btn, false, "注册中…", "注册本机");
    showFaceError(webauthnError(err));
  }
});
$("#add-code").addEventListener("keydown", (e) => { if (e.key === "Enter") $("#face-register-btn").click(); });
$("#add-code").addEventListener("input", async (e) => {
  const code = e.target.value.trim();
  if (code.length !== 8 || regOptions?.code === code) return;
  $("#face-error").hidden = true;
  try {
    await prefetchRegistration(code);
  } catch (err) {
    showFaceError(webauthnError(err));
  }
});

$("#face-back").addEventListener("click", () => {
  faceOptions = null;
  state.token = "";
  $("#login-form").token.value = "";
  $("#login-error").hidden = true;
  showLoginStep("token");
  $("#login-form").token.focus();
});

// 预取的注册参数（和刷脸一样，让点击能直接调起系统验证）
let regOptions = null;

async function prefetchRegistration(code = "") {
  regOptions = null;
  const res = await request("POST", "/api/auth/passkey/register/begin", { name: deviceName(), code });
  regOptions = { ...res, code, at: Date.now() };
}

// 在本机注册 Face ID / 指纹，成功后返回新的会话
async function registerThisDevice(code = "") {
  let begin = regOptions;
  regOptions = null;
  if (!begin || begin.code !== code || Date.now() - begin.at > 150_000) {
    begin = await request("POST", "/api/auth/passkey/register/begin", { name: deviceName(), code });
  }
  const cred = await navigator.credentials.create({ publicKey: decodeCreationOptions(begin.options.publicKey) });
  const res = await request("POST", `/api/auth/passkey/register/finish?ceremony=${encodeURIComponent(begin.ceremony)}`, encodeCredential(cred));
  return res.session;
}

// ---------- 安全设置：管理刷脸设备 ----------

$("#security-btn").addEventListener("click", () => openSecurity());
$("#sec-close").addEventListener("click", () => closeDialog($("#security")));

async function openSecurity(hint) {
  const dlg = $("#security");
  $("#sec-code-box").hidden = true;
  $("#sec-hint").textContent = hint || "";
  if (!dlg.open) dlg.showModal();
  await renderDevices(hint);
}

async function renderDevices(hint) {
  const list = $("#sec-list");
  let res;
  try {
    res = await request("GET", "/api/auth/passkeys");
  } catch (err) {
    fail(err);
    return;
  }
  const items = res.items || [];
  if (!hint) {
    $("#sec-hint").textContent = !res.available ? "服务端没有开启刷脸功能（需要设置 XIAOJU_CORS_ORIGINS）。"
      : items.length ? "登录需要「令牌 + 刷脸」。手机丢了？在这里删掉那台设备即可。"
      : "还没有开启。开启后登录需要「令牌 + 刷脸」，别人拿到令牌也打不开你的网盘。";
  }
  list.replaceChildren(...items.map((d) => {
    const li = document.createElement("li");
    const info = document.createElement("div");
    info.append(d.name);
    const metaEl = Object.assign(document.createElement("div"), { className: "meta" });
    metaEl.textContent = `添加于 ${formatDate(d.created_at)}` + (d.last_used_at ? ` · 最近使用 ${formatDate(d.last_used_at)}` : "");
    info.append(metaEl);
    const del = Object.assign(document.createElement("button"), { type: "button", className: "link danger", textContent: "移除" });
    del.addEventListener("click", () => removeDevice(d, items.length));
    li.append(info, del);
    return li;
  }));
  $("#sec-register").hidden = !res.available || !webauthnSupported();
  $("#sec-code").hidden = !res.available || items.length === 0;
  if (!$("#sec-register").hidden && !regOptions) prefetchRegistration().catch(() => { /* 点击时再请求 */ });
}

async function removeDevice(d, total) {
  const warn = total === 1 ? "\n\n这是最后一台设备：移除后，仅凭令牌就能登录。" : "";
  if (!confirm(`移除「${d.name}」？移除后这台设备不能再刷脸登录。${warn}`)) return;
  try {
    await request("DELETE", `/api/auth/passkeys/${encodeURIComponent(d.id)}`);
    toast("已移除");
    renderDevices();
  } catch (err) { fail(err); }
}

$("#sec-register").addEventListener("click", async () => {
  const btn = $("#sec-register");
  if (btn.disabled) return;
  setButtonBusy(btn, true, "请完成 Face ID / 指纹…", "在本机开启 Face ID / 指纹");
  try {
    state.session = await registerThisDevice();
    toast("✓ 本机已开启刷脸登录");
    $("#sec-hint").textContent = "";
    renderDevices();
  } catch (err) {
    toast(webauthnError(err));
    prefetchRegistration().catch(() => { /* 点击时再请求 */ });
  } finally {
    setButtonBusy(btn, false, "请完成 Face ID / 指纹…", "在本机开启 Face ID / 指纹");
  }
});

$("#sec-code").addEventListener("click", async () => {
  try {
    const { code, expires_at: exp } = await request("POST", "/api/auth/passkeys/add-code");
    const box = $("#sec-code-box");
    box.replaceChildren("在新设备的登录页点「这台设备还没注册？」，输入添加码：", document.createElement("br"),
      Object.assign(document.createElement("b"), { textContent: code }),
      document.createElement("br"), `${formatDate(exp)} 前有效，只能用一次。`);
    box.hidden = false;
    playClass(box, "fade-in", 400);
  } catch (err) { fail(err); }
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
  $("#stats").hidden = true;
}

// 同时刷新网盘统计和备份进度（备份提示里的文件数来自统计）
async function refreshBackup() {
  if (!state.session) return;
  const [stats, backup] = await Promise.all([
    request("GET", "/api/stats").catch(() => null), // 旧版服务端没有这些接口，或网络暂时不通：不显示
    request("GET", "/api/backup/status").catch(() => null),
  ]);
  if (!state.session) return; // 请求期间已退出
  const statsEl = $("#stats");
  if (stats) renderStats(statsEl, stats);
  statsEl.hidden = !stats;
  const badge = $("#backup-badge");
  if (backup) renderBackup(badge, backup, stats);
  badge.hidden = !backup;
}

// 例：共 120 个文件 · 照片 80 张 · 视频 30 个 · 其他 10 个 · 12.3 GB
function renderStats(el, st) {
  const part = (label, n, unit, bytes) => {
    const span = document.createElement("span");
    span.append(`${label} `, Object.assign(document.createElement("b"), { textContent: n }), ` ${unit}`);
    if (bytes) span.title = formatSize(bytes);
    return span;
  };
  const total = part("共", st.files, "个文件");
  total.className = "total";
  const items = [total];
  if (st.photos) items.push(part("🖼️ 照片", st.photos, "张", st.photo_bytes));
  if (st.videos) items.push(part("🎬 视频", st.videos, "个", st.video_bytes));
  if (st.others) items.push(part("📦 其他", st.others, "个"));
  if (st.folders) items.push(part("📁", st.folders, "个文件夹"));
  items.push(Object.assign(document.createElement("span"), { textContent: `已用 ${formatSize(st.bytes)}` }));
  el.replaceChildren(...items);
}

// 手机上没有悬停提示，点一下显示详情
$("#backup-badge").addEventListener("click", (e) => { if (e.currentTarget.title) toast(e.currentTarget.title); });

function renderBackup(el, st, stats) {
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
  // 有统计时按文件数说明，否则退回按分块
  const count = stats
    ? `已备份 ${stats.backed_up_files} / ${stats.files} 个文件`
    : `已备份 ${st.backed_up} / ${st.parts} 个分块`;
  const detail = `${count}（${formatSize(st.backed_up_bytes)} / ${formatSize(st.bytes)}）`;
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

let loadSeq = 0;

// prefetchedRoot：登录时已经拿到的根目录列表，避免重复请求
async function load(prefetchedRoot = null) {
  if (!state.api || !state.session) { openLogin(); return; }
  const seq = ++loadSeq;
  const app = $("#app");
  if (app.hidden) {
    app.hidden = false;
    playClass(app, "fade-in", 500);
  }
  state.folderId = currentFolderFromHash();
  const rows = $("#rows");
  $("#empty").hidden = true;
  // 已有内容时先变淡（切换目录、刷新），没有内容时显示骨架占位
  if (rows.querySelector("tr:not(.skeleton)")) rows.classList.add("stale");
  else if (!prefetchedRoot) showSkeleton();
  progress.start();
  try {
    const [chain, list] = await Promise.all([
      state.folderId ? crumbsFor(state.folderId) : [],
      prefetchedRoot && !state.folderId
        ? prefetchedRoot
        : request("GET", `/api/folders/${state.folderId || "root"}/children`),
    ]);
    if (seq !== loadSeq) return; // 期间又切换了目录，丢弃旧结果
    renderCrumbs(chain);
    renderRows(list.items);
  } catch (err) {
    if (seq !== loadSeq) return;
    rows.querySelectorAll(".skeleton").forEach((tr) => tr.remove());
    if (err.status === 404 && state.folderId) { location.hash = "#/"; return; }
    fail(err);
  } finally {
    progress.done();
    if (seq === loadSeq) rows.classList.remove("stale");
  }
}

function showSkeleton() {
  const widths = [[46, 60, 70, 55], [62, 50, 70, 55], [38, 65, 70, 55], [54, 45, 70, 55]];
  $("#rows").replaceChildren(...widths.map((w) => {
    const tr = document.createElement("tr");
    tr.className = "skeleton";
    ["name", "size", "date", "ops"].forEach((cls, i) => {
      const td = document.createElement("td");
      td.className = cls;
      const bar = document.createElement("span");
      bar.style.width = `${w[i]}%`;
      if (cls === "size" || cls === "ops") bar.style.marginLeft = "auto";
      td.append(bar);
      tr.append(td);
    });
    return tr;
  }));
}

function renderRows(items) {
  const rows = $("#rows");
  // 只给新出现的条目做淡入，刷新时已有的行保持不动
  const before = new Set([...rows.querySelectorAll("tr[data-id]")].map((tr) => tr.dataset.id));
  let n = 0;
  rows.replaceChildren(...items.map((f) => {
    const tr = rowFor(f);
    if (!before.has(f.id)) {
      tr.classList.add("appear");
      tr.style.setProperty("--i", Math.min(n++, 12));
    }
    return tr;
  }));
  $("#empty").hidden = items.length > 0;
  if (!items.length) playClass($("#empty"), "fade-in", 500);
}

function rowFor(f) {
  const tr = document.createElement("tr");
  tr.dataset.id = f.id;
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
    refreshBackup();
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
  reloadTimer = setTimeout(() => load(), 1500);
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
        logout(err.message);
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
        if (!state.session) { row.remove(); return; } // 已退出登录
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
    for (const [k, v] of Object.entries(authHeaders())) xhr.setRequestHeader(k, v);
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
      let data = {};
      try { data = JSON.parse(xhr.responseText) || {}; } catch { /* 非 JSON */ }
      reject(new HttpError(xhr.status, errorText(xhr.status, data.error, data.code), data.code));
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

window.addEventListener("hashchange", () => load());
load();

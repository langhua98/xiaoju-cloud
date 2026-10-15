// 小桔云盘前端：纯静态页面，通过 CORS 调用 xiaoju-cloud 服务端 API。
"use strict";

const $ = (sel) => document.querySelector(sel);
const store = {
  get(key) { try { return localStorage.getItem(key) || ""; } catch { return ""; } },
  set(key, v) { try { localStorage.setItem(key, v); } catch { /* 隐私模式下忽略 */ } },
};

// 内置了服务端地址时只需输入令牌。令牌只放在内存里；登录后的会话（12 小时）存在当前标签页（sessionStorage），
// iPhone 因为内存紧张把页面重新加载、或者网页自动更新后不用重新登录，关闭标签页即失效。
// 登录：令牌 → Face ID / 指纹（注册了设备时），通过后服务端签发会话（12 小时）。
// 开启了格子口令时，看照片、视频每随机 3–7 张弹出一道限时格子题，两次机会都没答对就退出登录。
const defaultAPI = (window.XIAOJU_API_URL || "").replace(/\/+$/, "");
const state = {
  api: defaultAPI || store.get("xiaoju.api"),
  token: "",   // 访问令牌（登录接口要用）
  session: tabSession("get"), // 全部验证通过后签发的会话；有值即表示已登录
  loginSteps: [], // 令牌之后还要过的步骤，如 ["passkey"]
  recoveryAvailable: false, // 可以用恢复码代替刷脸
  folderId: null,
};
try { localStorage.removeItem("xiaoju.token"); } catch { /* 清理旧版本保存的令牌 */ }

// 当前标签页里的会话：tabSession("get") / tabSession("set", v) / tabSession("clear")
function tabSession(op, v) {
  try {
    if (op === "get") return sessionStorage.getItem("xiaoju.session") || "";
    if (op === "set") sessionStorage.setItem("xiaoju.session", v);
    else sessionStorage.removeItem("xiaoju.session");
  } catch { /* 隐私模式下只放内存 */ }
  return "";
}

class HttpError extends Error {
  constructor(status, message, code, data = {}) { super(message); this.status = status; this.code = code; this.data = data; }
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
  if (!resp.ok) throw new HttpError(resp.status, errorText(resp.status, data.error, data.code, data), data.code, data);
  return data;
}

const CODE_TEXT = {
  passkey_required: "需要刷脸验证，请重新登录",
  second_factor_required: "需要完成登录验证，请重新登录",
  wrong_answer: "答错了 · 最后一次机会，多 5 秒",
  timeout: "超时了 · 最后一次机会，多 5 秒",
  grid_logout: "格子口令两次都没答对，已退出登录",
  grid_required: "需要先答一道格子题",
  challenge_expired: "这道题已过期，已换一道新题",
  recovery_invalid: "恢复码不对，或者已经用过了",
  recovery_used: "这次登录已经用过一个恢复码了，这一步请答题",
  grid_disabled: "格子口令没有开启",
  grid_enabled: "格子口令已经开启了",
  invalid_rule: "规则无效：要选两个不同的格子，周一加的数在 0–99 之间",
};

function errorText(status, msg, code, data = {}) {
  if (code === "locked") {
    const min = Math.max(1, Math.ceil((data.retry_after || 0) / 60));
    return `错误次数太多，已锁定，请 ${min} 分钟后再试`;
  }
  if (CODE_TEXT[code]) return CODE_TEXT[code];
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
  if (err.code === "grid_cancelled") return; // 格子口令没通过，已经退出登录
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

const STEP_LABEL = { token: "令牌", passkey: "刷脸" };

// 顶部步骤条：令牌 ✓ — 刷脸 ●（只有一步时不显示）
function renderLoginSteps(step) {
  const el = $("#login-steps");
  const all = ["token", ...state.loginSteps];
  const cur = all.indexOf(step);
  el.hidden = state.loginSteps.length === 0 || cur <= 0;
  el.replaceChildren(...all.map((s, i) => {
    const li = document.createElement("li");
    li.textContent = (i < cur ? "✓ " : "") + STEP_LABEL[s];
    if (i < cur) li.className = "done";
    if (i === cur) { li.className = "current"; li.setAttribute("aria-current", "step"); }
    return li;
  }));
}

function showLoginStep(step) {
  const panels = { token: $("#login-form"), passkey: $("#face-step") };
  for (const [k, el] of Object.entries(panels)) el.hidden = k !== step;
  renderLoginSteps(step);
  playClass(panels[step], "step-in", 400);
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
  $("#login-steps").hidden = true;
  if (!$("#login").open) $("#login").showModal();
  (defaultAPI ? form.token : form.api).focus();
}

async function logout(message) {
  state.token = "";
  state.session = "";
  tabSession("clear");
  state.loginSteps = [];
  faceOptions = null;
  regOptions = null;
  closeGridGate();
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

// 第一步：校验令牌。已注册刷脸设备时进入刷脸，否则直接拿到会话
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

  state.recoveryAvailable = !!res.recovery_available;
  if (!res.session) {
    state.loginSteps = res.steps || ["passkey"];
    enterFaceStep();
    return;
  }
  state.loginSteps = [];
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
  enterApp();
}

// 已登录：保存会话，显示主界面
function enterApp() {
  tabSession("set", state.session);
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

function enterFaceStep(message) {
  $("#face-error").hidden = true;
  $("#face-step").classList.remove("ok", "verifying");
  $("#face-add").open = false;
  $("#add-code").value = "";
  $("#face-recovery").hidden = !state.recoveryAvailable;
  $("#face-recovery").open = false;
  $("#face-recovery-code").value = "";
  if (!webauthnSupported()) {
    showLoginStep("passkey");
    showFaceError("这个浏览器不支持 Face ID / 指纹登录，请用 Safari 或 Chrome 打开（不要在微信里打开）");
    $("#face-btn").disabled = true;
    return;
  }
  $("#face-btn").disabled = false;
  showLoginStep("passkey");
  prefetchFaceOptions();
  $("#face-btn").focus();
  if (message) showFaceError(message);
}

// 刷脸（或恢复码）通过后登录
async function afterFace(res, btn) {
  state.session = res.session;
  await finishLogin(btn);
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
    step.classList.remove("verifying");
    step.classList.add("ok");
    setButtonBusy(btn, false, "正在验证…", "使用 Face ID / 指纹解锁");
    await afterFace(res, btn);
  } catch (err) {
    step.classList.remove("verifying");
    setButtonBusy(btn, false, "正在验证…", "使用 Face ID / 指纹解锁");
    if (err.status === 401 && !err.code) { logout("令牌已失效，请重新输入"); return; }
    showFaceError(webauthnError(err));
    prefetchFaceOptions();
  }
});

// 没注册过的设备：输入另一台已登录设备生成的添加码，注册本机后继续登录
$("#face-register-btn").addEventListener("click", async () => {
  const btn = $("#face-register-btn");
  const code = $("#add-code").value.trim();
  if (btn.disabled) return;
  if (!code) { $("#add-code").focus(); return; }
  $("#face-error").hidden = true;
  setButtonBusy(btn, true, "注册中…", "注册本机");
  try {
    const res = await registerThisDevice(code);
    $("#face-step").classList.add("ok");
    setButtonBusy(btn, false, "注册中…", "注册本机");
    toast("本机已开启刷脸登录");
    await afterFace(res, $("#face-btn"));
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

// 刷脸设备丢了：用一个恢复码代替刷脸（恢复码在开启格子口令时生成）
$("#face-recovery-btn").addEventListener("click", async () => {
  const btn = $("#face-recovery-btn");
  const code = $("#face-recovery-code").value.trim();
  if (btn.disabled) return;
  if (!code) { $("#face-recovery-code").focus(); return; }
  $("#face-error").hidden = true;
  setButtonBusy(btn, true, "验证中…", "使用恢复码");
  try {
    const res = await request("POST", "/api/auth/recovery/passkey", { code });
    setButtonBusy(btn, false, "验证中…", "使用恢复码");
    toast("已用掉一个恢复码。登录后记得在「🔐 安全」里给本机重新开启刷脸");
    await afterFace(res, btn);
  } catch (err) {
    setButtonBusy(btn, false, "验证中…", "使用恢复码");
    if (err.status === 401 && !err.code) { logout("令牌已失效，请重新输入"); return; }
    showFaceError(err.message);
  }
});
$("#face-recovery-code").addEventListener("keydown", (e) => { if (e.key === "Enter") $("#face-recovery-btn").click(); });

function backToToken() {
  faceOptions = null;
  state.token = "";
  state.loginSteps = [];
  $("#login-form").token.value = "";
  $("#login-error").hidden = true;
  showLoginStep("token");
  $("#login-form").token.focus();
}
$("#face-back").addEventListener("click", backToToken);

// 预取的注册参数（和刷脸一样，让点击能直接调起系统验证）
let regOptions = null;

async function prefetchRegistration(code = "") {
  regOptions = null;
  const res = await request("POST", "/api/auth/passkey/register/begin", { name: deviceName(), code });
  regOptions = { ...res, code, at: Date.now() };
}

// 在本机注册 Face ID / 指纹。返回 {session}，或开启了格子口令且未登录时返回 {next: "grid", ticket}
async function registerThisDevice(code = "") {
  let begin = regOptions;
  regOptions = null;
  if (!begin || begin.code !== code || Date.now() - begin.at > 150_000) {
    begin = await request("POST", "/api/auth/passkey/register/begin", { name: deviceName(), code });
  }
  const cred = await navigator.credentials.create({ publicKey: decodeCreationOptions(begin.options.publicKey) });
  return request("POST", `/api/auth/passkey/register/finish?ceremony=${encodeURIComponent(begin.ceremony)}`, encodeCredential(cred));
}

// ---------- 格子口令 ----------
// 页面出一张随机 4×4 数字格，主人按自己设定的规则心算答案。规则只在服务端（加密保存），这里不知道。

const WEEKDAY = ["", "周一", "周二", "周三", "周四", "周五", "周六", "周日"];

function mk(tag, props = {}, ...children) {
  const el = Object.assign(document.createElement(tag), props);
  el.append(...children);
  return el;
}

function busyButton(label, className = "") {
  return mk("button", { type: "submit", className },
    mk("span", { className: "spinner", ariaHidden: "true" }), mk("span", { className: "label", textContent: label }));
}

const prefersReducedMotion = () => window.matchMedia?.("(prefers-reduced-motion: reduce)").matches;
const RING_C = 2 * Math.PI * 19; // 倒计时圆环周长（r = 19）

// 答题组件（深色卡片）：点格子里的数字依次凑出答案，点满 ch.digits 位自动提交，没有确认按钮。
//   label                  → 左上角标题
//   timed                  → 抽查题：圆环倒计时，到点自动交“超时”；答错 / 超时自动换新题（第二次机会）
//   recovery               → 允许用恢复码代替
//   load()                 → 出一道题
//   submit(payload)        → payload 为 {id, answer}、{id, timeout: true} 或 {recovery}
//   onError(err) → true    表示已处理
function mountGridQuiz(root, { load, submit, label = "安全验证", timed = false, recovery = false, onError }) {
  const svg = (markup) => { const t = document.createElement("template"); t.innerHTML = markup.trim(); return t.content.firstChild; };
  const day = mk("em", { className: "gq-day" });
  const head = mk("div", { className: "gq-head" },
    mk("div", { className: "gq-title" },
      svg('<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 3l7 3v5c0 4.5-3 8.3-7 10-4-1.7-7-5.5-7-10V6z"/><path d="M9 12l2 2 4-4"/></svg>'),
      mk("span", { textContent: label }), day));
  const ring = svg(`<div class="gq-ring" role="timer"><svg viewBox="0 0 44 44" aria-hidden="true"><circle class="track" cx="22" cy="22" r="19"/><circle class="bar" cx="22" cy="22" r="19" stroke-dasharray="${RING_C}" stroke-dashoffset="0"/></svg><b></b></div>`);
  if (timed) head.append(ring);
  const grid = mk("div", { className: "gq-grid loading", role: "group", ariaLabel: "数字格：依次点出答案" });
  const cells = Array.from({ length: 16 }, () => {
    const b = mk("button", { type: "button", textContent: "·" });
    b.addEventListener("click", () => tap(b));
    grid.append(b);
    return b;
  });
  const slots = mk("div", { className: "gq-slots", ariaLive: "polite" });
  const status = mk("p", { className: "gq-status" });
  root.className = "gq" + (timed ? " timed" : "");
  root.replaceChildren(head, grid, slots, status);

  let recInput, recBtn;
  if (recovery) {
    recInput = mk("input", { maxLength: 9, autocomplete: "off", autocapitalize: "off", spellcheck: false, placeholder: "xxxx-xxxx", ariaLabel: "恢复码" });
    recBtn = busyButton("确定");
    recBtn.type = "button";
    root.append(mk("details", { className: "gq-rec" },
      mk("summary", { textContent: "规则忘了？用恢复码" }),
      mk("div", { className: "row" }, recInput, recBtn)));
  }

  let ch = null;
  let digits = "";
  let busy = false;
  let locked = false;
  let ticker = null;
  const ready = () => ch && !busy && !locked && !grid.classList.contains("loading");

  const setStatus = (text, kind = "") => { status.textContent = text; status.className = "gq-status" + (kind ? " " + kind : ""); };
  const paintSlots = () => {
    const n = ch?.digits || 0;
    slots.replaceChildren(...Array.from({ length: n }, (_, i) => mk("i", { className: i < digits.length ? "on" : "" })));
    slots.ariaLabel = n ? `已输入 ${digits.length} 位，共 ${n} 位` : "";
  };
  const flag = (cls, ms = 450) => playClass(root, cls, ms);

  function stopTimer() { clearInterval(ticker); ticker = null; }
  // 倒计时到点：把“超时”交给服务端（算用掉一次机会）
  function startTimer(seconds) {
    stopTimer();
    const bar = ring.querySelector(".bar");
    const num = ring.querySelector("b");
    const total = seconds * 1000;
    const end = Date.now() + total;
    const tick = () => {
      if (!root.isConnected) { stopTimer(); return; }
      const left = Math.max(0, end - Date.now());
      bar.setAttribute("stroke-dashoffset", String(RING_C * (1 - left / total)));
      num.textContent = String(Math.ceil(left / 1000));
      ring.classList.toggle("urgent", left <= 5000);
      if (left === 0) {
        stopTimer();
        if (ch && !locked && !busy) send({ id: ch.id, timeout: true });
      }
    };
    tick();
    ticker = setInterval(tick, 200);
  }

  function tap(b) {
    if (!ready() || digits.length >= ch.digits) return;
    digits += b.textContent;
    playClass(b, "hit", 350);
    paintSlots();
    if (digits.length === ch.digits) send({ id: ch.id, answer: Number(digits) });
  }

  // 新题出来时数字先滚动一下再定格（“解码”效果），期间不能点
  function reveal(values) {
    return new Promise((resolve) => {
      if (prefersReducedMotion()) { cells.forEach((b, i) => { b.textContent = values[i]; }); resolve(); return; }
      let n = 0;
      const t = setInterval(() => {
        n++;
        cells.forEach((b, i) => {
          b.textContent = n >= 6 + (i % 4) ? values[i] : String(Math.floor(Math.random() * 10));
        });
        if (n >= 10) { clearInterval(t); resolve(); }
      }, 45);
    });
  }

  async function next() {
    ch = null;
    digits = "";
    stopTimer();
    grid.classList.add("loading");
    paintSlots();
    let c;
    try {
      c = await load();
    } catch (err) {
      handle(err);
      return;
    }
    ch = c;
    day.textContent = WEEKDAY[c.weekday];
    day.title = `出题时间 ${c.issued_local}（${c.timezone}），星期按这个算`;
    root.classList.toggle("last", c.attempt > 1);
    if (c.attempt > 1 && !status.textContent) setStatus("最后一次机会", "warn");
    paintSlots();
    await reveal(c.grid.map(String));
    if (ch !== c || !root.isConnected) return;
    grid.classList.remove("loading");
    if (timed) startTimer(c.seconds);
    cells[0].focus({ preventScroll: true });
  }

  function handle(err) {
    if (onError && onError(err)) return;
    if (err.code === "locked") {
      locked = true;
      stopTimer();
      grid.classList.add("loading");
      if (recBtn) recBtn.disabled = true;
      setStatus(err.message, "bad");
      flag("wrong");
      return;
    }
    setStatus(err.message, ["wrong_answer", "timeout"].includes(err.code) ? "warn" : "bad");
    flag("wrong");
    if (["wrong_answer", "timeout", "challenge_expired"].includes(err.code)) next();
  }

  async function send(payload, b = null) {
    if (locked || busy) return;
    stopTimer();
    busy = true;
    root.classList.add("busy");
    if (b) setButtonBusy(b, true);
    try {
      await submit(payload);
      root.classList.add("ok");
      setStatus("");
    } catch (err) {
      digits = "";
      paintSlots();
      handle(err);
    } finally {
      busy = false;
      root.classList.remove("busy");
      if (b) setButtonBusy(b, false);
    }
  }

  if (recBtn) {
    recBtn.addEventListener("click", () => {
      const code = recInput.value.trim();
      if (!code) { recInput.focus(); return; }
      send({ recovery: code }, recBtn);
    });
    recInput.addEventListener("keydown", (e) => { if (e.key === "Enter") recBtn.click(); });
  }
  next();
}

// 看照片、视频时的抽查弹窗。答对返回 true；两次都没答对时服务端已作废会话，这里退出登录并返回 false。
// 弹窗不能关掉跳过（题出了不答，服务端也会算用掉一次机会）。
let gridGatePending = null;

function gridGate() {
  if (gridGatePending) return gridGatePending.promise;
  const dlg = $("#grid-gate");
  let resolve;
  const promise = new Promise((r) => { resolve = r; });
  gridGatePending = { promise, resolve };
  const done = async (ok) => {
    if (gridGatePending?.promise !== promise) return;
    gridGatePending = null;
    if (ok) await closeDialog(dlg);
    else dlg.close();
    $("#grid-gate-quiz").replaceChildren();
    resolve(ok);
  };
  mountGridQuiz($("#grid-gate-quiz"), {
    timed: true,
    load: () => request("POST", "/api/auth/grid/challenge"),
    async submit(payload) {
      await request("POST", "/api/auth/grid/verify", payload);
      $("#grid-gate-quiz").classList.add("ok"); // 亮一下绿色再关
      await sleepMs(420);
      await done(true);
    },
    onError(err) {
      if (err.status === 401) { // grid_logout：两次都没答对；或会话已过期
        done(false);
        logout(err.message);
        return true;
      }
      return false;
    },
  });
  if (!dlg.open) dlg.showModal();
  return promise;
}

function closeGridGate() {
  const pending = gridGatePending;
  gridGatePending = null;
  $("#grid-gate").close();
  $("#grid-gate-quiz").replaceChildren();
  pending?.resolve(false);
}

$("#grid-gate").addEventListener("cancel", (e) => e.preventDefault()); // 不能用 Esc 跳过

// ---------- 安全设置：管理刷脸设备 ----------

$("#security-btn").addEventListener("click", () => openSecurity());
$("#sec-close").addEventListener("click", () => closeSecurity());
$("#security").addEventListener("cancel", (e) => { e.preventDefault(); closeSecurity(); });

function closeSecurity() {
  if (gridCodesPending && !confirm("恢复码还没确认保存。关闭后就再也看不到这组恢复码了，确定关闭？")) return;
  gridCodesPending = false;
  closeDialog($("#security"));
}

async function openSecurity(hint) {
  const dlg = $("#security");
  $("#sec-code-box").hidden = true;
  $("#sec-hint").textContent = hint || "";
  // 格子口令的状态加载完之前不显示旧内容和按钮
  if (!gridCodesPending) {
    $("#grid-sec-hint").textContent = "";
    $("#grid-sec-body").replaceChildren();
    $("#grid-sec-actions").hidden = true;
  }
  if (!dlg.open) dlg.showModal();
  await Promise.all([renderDevices(hint), gridCodesPending ? null : renderGridSec(), renderShortcutSec()]);
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
  const warn = total !== 1 ? "" : gridStatus?.enabled
    ? "\n\n这是最后一台设备：移除后，登录只需要令牌 + 格子口令。"
    : "\n\n这是最后一台设备：移除后，仅凭令牌就能登录。";
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
    const res = await registerThisDevice();
    if (res.session) { state.session = res.session; tabSession("set", res.session); }
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

// ---------- 安全设置：格子口令 ----------

let gridStatus = null;
let gridCodesPending = false; // 刚生成的恢复码还没确认保存

function gridSecActions(show) {
  $("#grid-sec-actions").hidden = !show;
  $("#grid-sec-enable").hidden = !!gridStatus?.enabled;
  $("#grid-sec-disable").hidden = !gridStatus?.enabled;
}

async function renderGridSec() {
  try {
    gridStatus = await request("GET", "/api/auth/grid");
  } catch (err) {
    fail(err);
    return;
  }
  $("#grid-sec").hidden = !gridStatus.available;
  $("#grid-sec-body").replaceChildren();
  const hint = $("#grid-sec-hint");
  const st = gridStatus;
  if (st.enabled) {
    hint.textContent = `已开启：看照片、视频时每 ${st.views_min}–${st.views_max} 张随机弹出一道格子题，`
      + `限时 ${st.seconds_first} 秒；答错再给一次机会（${st.seconds_second} 秒），两次都没答对就退出登录。`
      + `剩余恢复码 ${st.recovery_left} 个`
      + (st.recovery_left <= 3 ? "，不多了：关闭后重新开启会生成新的一组。" : "。");
  } else {
    hint.textContent = "开启后，看照片、视频时会随机弹出一张 4×4 数字格，你按自己设定的规则心算出答案、点数字输入。"
      + "规则只有你知道（加密保存在服务端），答不对就退出登录，别人拿着你已登录的手机也翻不了相册。";
  }
  gridSecActions(true);
}

$("#grid-sec-enable").addEventListener("click", () => gridSetupPick());
$("#grid-sec-disable").addEventListener("click", () => gridDisable());

// 开启第 1 步：点选乘数格、加数格，填周一额外加的数
function gridSetupPick(rule = { mul: -1, add: -1, monday_bonus: "" }) {
  gridSecActions(false);
  const body = $("#grid-sec-body");
  const board = mk("div", { className: "grid-board" });
  const bonus = mk("input", { type: "number", min: 0, max: 99, inputMode: "numeric", value: rule.monday_bonus });
  const errEl = mk("p", { className: "error", hidden: true });
  const paint = () => {
    [...board.children].forEach((b, i) => {
      b.className = i === rule.mul ? "mul" : i === rule.add ? "add" : "";
      b.textContent = i === rule.mul ? "×" : i === rule.add ? "+" : `${Math.floor(i / 4) + 1}-${(i % 4) + 1}`;
      b.setAttribute("aria-pressed", String(i === rule.mul || i === rule.add));
    });
  };
  for (let i = 0; i < 16; i++) {
    const b = mk("button", { type: "button", ariaLabel: `第 ${Math.floor(i / 4) + 1} 行第 ${(i % 4) + 1} 列` });
    b.addEventListener("click", () => {
      if (i === rule.mul) rule.mul = -1;
      else if (i === rule.add) rule.add = -1;
      else if (rule.mul < 0) rule.mul = i;
      else if (rule.add < 0) rule.add = i;
      else { rule.mul = i; rule.add = -1; } // 两个都选了：重新从乘数格开始
      paint();
    });
    board.append(b);
  }
  paint();
  const nextBtn = mk("button", { type: "button", className: "primary", textContent: "下一步：做一道练习题" });
  const cancel = mk("button", { type: "button", className: "ghost", textContent: "取消" });
  nextBtn.addEventListener("click", () => {
    rule.monday_bonus = bonus.value;
    const n = Number(bonus.value);
    if (rule.mul < 0 || rule.add < 0) { errEl.textContent = "请在格子上依次点选乘数格和加数格"; errEl.hidden = false; return; }
    if (bonus.value === "" || !Number.isInteger(n) || n < 0 || n > 99) { errEl.textContent = "周一额外加的数要在 0–99 之间"; errEl.hidden = false; bonus.focus(); return; }
    gridSetupPractice({ mul: rule.mul, add: rule.add, monday_bonus: n });
  });
  cancel.addEventListener("click", () => renderGridSec());
  body.replaceChildren(mk("div", { className: "grid-pick" },
    mk("p", { className: "grid-legend" }, "依次点选 ", mk("b", { className: "mul", textContent: "× 乘数格" }),
      "（乘以星期几，周日算 7）和 ", mk("b", { className: "add", textContent: "+ 加数格" })),
    board,
    mk("label", {}, "周一额外加", bonus)),
  errEl,
  mk("p", { className: "hint", textContent: "答案 = 乘数格 × 星期 + 加数格，周一再加上面这个数。星期按出题那一刻算。设置时注意别让旁人看到。" }),
  mk("div", { className: "dialog-actions" }, cancel, nextBtn));
  playClass(body, "fade-in", 400);
}

// 开启第 2 步：按刚设定的规则答对一道练习题才真正开启，防止设错把自己锁在外面
function gridSetupPractice(rule) {
  const body = $("#grid-sec-body");
  const quiz = mk("div");
  const back = mk("button", { type: "button", className: "link", textContent: "← 重新选格子" });
  back.addEventListener("click", () => gridSetupPick({ ...rule }));
  body.replaceChildren(mk("p", { className: "hint", textContent: "按刚设定的规则做一道练习题（不限时），答对就开启。" }), quiz, back);
  mountGridQuiz(quiz, {
    label: "练习",
    load: () => request("POST", "/api/auth/grid/setup/begin", { rule }),
    async submit({ id, answer }) {
      const res = await request("POST", "/api/auth/grid/setup/finish", { id, answer });
      await sleepMs(300);
      gridShowCodes(res.recovery_codes);
    },
    onError(err) {
      if (err.code === "wrong_answer") err.message = "和设定的规则对不上，换一题再试";
      if (err.status === 401) { fail(err); return true; }
      return false;
    },
  });
}

// 开启第 3 步：显示恢复码（只显示这一次）
function gridShowCodes(codes) {
  gridCodesPending = true;
  const body = $("#grid-sec-body");
  const text = `小桔云盘恢复码（每个只能用一次，可以代替“刷脸”或“格子口令”中的一步）\n生成于 ${new Date().toLocaleString()}\n\n${codes.join("\n")}\n`;
  const copy = mk("button", { type: "button", textContent: "复制" });
  copy.addEventListener("click", async () => {
    try { await navigator.clipboard.writeText(text); toast("已复制"); } catch { toast("复制失败，请手动抄下"); }
  });
  const dl = mk("button", { type: "button", textContent: "下载 .txt" });
  dl.addEventListener("click", () => {
    const a = mk("a", { href: URL.createObjectURL(new Blob([text], { type: "text/plain" })), download: "xiaoju-recovery-codes.txt" });
    a.click();
    setTimeout(() => URL.revokeObjectURL(a.href), 1000);
  });
  const saved = mk("input", { type: "checkbox" });
  const done = mk("button", { type: "button", className: "primary", textContent: "完成", disabled: true });
  saved.addEventListener("change", () => { done.disabled = !saved.checked; });
  done.addEventListener("click", () => {
    gridCodesPending = false;
    toast("✓ 格子口令已开启");
    renderGridSec();
  });
  body.replaceChildren(
    mk("p", { className: "hint", textContent: "✓ 已开启。下面是 10 个恢复码，只显示这一次，每个只能用一次：刷脸设备丢了时可以代替刷脸登录，规则忘了时可以用来关闭格子口令。请存到手机以外的地方。" }),
    mk("ul", { className: "recovery-codes" }, ...codes.map((c) => mk("li", { textContent: c }))),
    mk("div", { className: "dialog-actions wrap" }, copy, dl),
    mk("label", { className: "check" }, saved, "我已妥善保存这些恢复码"),
    mk("div", { className: "dialog-actions" }, done));
  playClass(body, "fade-in", 400);
}

// 关闭：要当场重新刷脸（注册了设备时）并答对一道题（或用恢复码），防止被盗的会话拆掉验证
async function gridDisable() {
  gridSecActions(false);
  const body = $("#grid-sec-body");
  let ticket = "";
  if (gridStatus.passkey_required) {
    ticket = await reverifyFace(body);
    if (!ticket) { renderGridSec(); return; }
  }
  // 题出了就要答（和抽查一样两次机会、两次都没答对退出登录），所以这里不再提供取消
  const quiz = mk("div");
  body.replaceChildren(mk("p", { className: "hint", textContent: "答对这道题就关闭（关闭后恢复码全部作废）。" }), quiz);
  mountGridQuiz(quiz, {
    label: "确认关闭",
    timed: true,
    recovery: true,
    load: () => request("POST", "/api/auth/grid/challenge"),
    async submit(payload) {
      await request("DELETE", "/api/auth/grid", { ticket, ...payload });
      toast("已关闭格子口令");
      renderGridSec();
    },
    onError(err) {
      if (err.code === "passkey_required") { toast("刷脸验证已超时，请重新操作"); renderGridSec(); return true; }
      if (err.status === 401) { fail(err); return true; }
      return false;
    },
  });
}

// 在安全设置里重新刷脸，成功后返回“刚刚刷过脸”的凭证；取消返回 ""
function reverifyFace(body) {
  return new Promise((resolve) => {
    let opts = null;
    const begin = () => request("POST", "/api/auth/passkey/login/begin").then((r) => { opts = { ...r, at: Date.now() }; });
    begin().catch(() => { /* 点击时再请求 */ });
    const btn = mk("button", { type: "button", className: "primary" },
      mk("span", { className: "spinner", ariaHidden: "true" }), mk("span", { className: "label", textContent: "使用 Face ID / 指纹验证" }));
    const cancel = mk("button", { type: "button", className: "ghost", textContent: "取消" });
    const errEl = mk("p", { className: "error", hidden: true });
    cancel.addEventListener("click", () => resolve(""));
    btn.addEventListener("click", async () => {
      errEl.hidden = true;
      setButtonBusy(btn, true, "正在验证…", "使用 Face ID / 指纹验证");
      try {
        if (!opts || Date.now() - opts.at > 120_000) await begin();
        const o = opts;
        opts = null;
        const cred = await navigator.credentials.get({ publicKey: decodeRequestOptions(o.options.publicKey) });
        const res = await request("POST", `/api/auth/passkey/login/finish?ceremony=${encodeURIComponent(o.ceremony)}`, encodeCredential(cred));
        resolve(res.ticket || "");
      } catch (err) {
        setButtonBusy(btn, false, "正在验证…", "使用 Face ID / 指纹验证");
        errEl.textContent = webauthnError(err);
        errEl.hidden = false;
        begin().catch(() => { /* 点击时再请求 */ });
      }
    });
    body.replaceChildren(mk("p", { className: "hint", textContent: "第 1 步：重新刷脸，确认是你本人在操作。" }), errEl,
      mk("div", { className: "dialog-actions" }, cancel, btn));
  });
}

// ---------- 安全设置：快捷指令批量上传 ----------
// 上传接口只进不出（不需要密钥，只能往“相册”里添加），快捷指令装上就能用。

const SHORTCUT_FILE = "xiaoju-backup.shortcut";

// 网页旁边有没有签好名的快捷指令（GitHub Pages 发布时生成）
async function shortcutAvailable() {
  try {
    const resp = await fetch(SHORTCUT_FILE, { method: "HEAD", cache: "no-store" });
    return resp.ok;
  } catch { return false; }
}

function copyButton(text, label = "复制") {
  const b = mk("button", { type: "button", textContent: label });
  b.addEventListener("click", async () => {
    try { await navigator.clipboard.writeText(text); toast("已复制"); } catch { toast("复制失败，请长按选中后手动复制"); }
  });
  return b;
}

async function renderShortcutSec() {
  const box = $("#shortcut-box");
  const url = `${state.api}/api/ingest`;
  const code = (t) => mk("code", { textContent: t });
  const install = mk("div", { className: "key-install", hidden: true },
    mk("a", { className: "button primary", href: SHORTCUT_FILE, textContent: "安装快捷指令" }),
    mk("p", { className: "hint", textContent: "点了之后 Safari 问是否下载就选「下载」，再在下载列表里点开，在快捷指令 App 里点「添加快捷指令」。"
      + "以后打开「备份照片到小桔云盘」选照片就行，传完会通知上传了几张、跳过了几张；第一次会问能否连接网盘，选「始终允许」。" }));
  const manual = mk("details", { className: "key-manual" },
    mk("summary", { textContent: "自己动手搭建快捷指令" }),
    mk("div", { className: "key-field" }, mk("span", { textContent: "地址" }), mk("code", { textContent: url }), copyButton(url)),
    mk("ol", { className: "key-steps" },
      mk("li", {}, "「快捷指令」App 点「＋」新建，添加「选择照片」，展开后打开「选择多张」。"),
      mk("li", {}, "添加「重复每一项」，在里面添加「获取 URL 的内容」：URL 粘贴上面的地址；方法 ", code("POST"),
        "；头部加一项 ", code("X-File-Name"), "，值选变量「重复项目」后改成「名称」；请求体选「文件」= 「重复项目」。"),
      mk("li", {}, "运行。中途出错就再运行一次，传过的会自动跳过。")));
  box.replaceChildren(install, manual,
    mk("p", { className: "hint", textContent: "想每天自动备份：在快捷指令「自动化」里新建“特定时间”运行它，并把里面的「选择照片」换成「查找照片」（添加日期 是 今天）。" }));
  const ok = await shortcutAvailable();
  install.hidden = !ok;
  if (!ok) manual.open = true;
}

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

// 开启了格子口令时，看照片、视频每随机几张服务端会要求先答一道格子题（grid_required）
async function signedURL(f, inline) {
  const path = `/api/files/${f.id}/link`;
  let res;
  try {
    res = await request("POST", path);
  } catch (err) {
    if (err.code !== "grid_required") throw err;
    if (!(await gridGate())) throw new HttpError(0, "", "grid_cancelled"); // 已退出登录
    res = await request("POST", path);
  }
  return state.api + res.url + (inline ? "&inline=1" : "");
}

async function download(f) {
  try {
    const a = Object.assign(document.createElement("a"), { href: await signedURL(f, false), rel: "noopener" });
    document.body.append(a);
    a.click();
    a.remove();
  } catch (err) { fail(err); }
}

// Safari（iPhone / Mac）能直接显示 HEIC 照片，其他浏览器不行
const isSafari = /Safari\//.test(navigator.userAgent) && !/Chrome|Chromium|CriOS|Edg|Firefox|FxiOS/.test(navigator.userAgent);

function previewKind(mime = "") {
  if (/^image\/(png|jpe?g|gif|webp|avif|bmp|svg\+xml)$/.test(mime)) return "img";
  if (isSafari && /^image\/hei[cf]$/.test(mime)) return "img";
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
// 上传卡住的判定：传文件的过程中这么久没有任何进度（网络断了、手机锁屏或切到后台），中断后重试
const STALL_MS = 60_000;
// 文件传完后，服务端加密并写入 Telegram（含限流等待）最多等这么久
const SERVER_WAIT_MS = 6 * 60_000;
const ALBUM = "相册";

const queue = [];
let active = 0;
let stats = null; // 本轮上传的统计：{ uploaded, skipped, failed, sync, started, received }

// 上传期间保持屏幕常亮：iPhone 锁屏后网页会被暂停，几百张照片要传十几分钟
let wakeLock = null;
let wakeLockPending = false;
async function keepAwake(on) {
  if (!("wakeLock" in navigator)) return;
  try {
    if (on && !wakeLock && !wakeLockPending && document.visibilityState === "visible") {
      wakeLockPending = true;
      wakeLock = await navigator.wakeLock.request("screen");
      wakeLock.addEventListener("release", () => { wakeLock = null; });
    } else if (!on && wakeLock) {
      const l = wakeLock;
      wakeLock = null;
      await l.release();
    }
  } catch { /* 不支持、低电量模式或被系统拒绝 */ } finally {
    wakeLockPending = false;
  }
}
// 切到后台时系统会自动释放，回到前台后重新申请
document.addEventListener("visibilitychange", () => { if (document.visibilityState === "visible" && (active || queue.length)) keepAwake(true); });

function formatDuration(ms) {
  const s = Math.max(0, Math.round(ms / 1000));
  if (s < 60) return `${s} 秒`;
  const m = Math.floor(s / 60);
  return m < 60 ? `${m} 分 ${s % 60} 秒` : `${Math.floor(m / 60)} 小时 ${m % 60} 分`;
}

const sleep = (s) => new Promise((r) => setTimeout(r, s * 1000));

// items: [{ file, name }]；sync=true 表示来自“同步相册”，同名已存在视为已同步
function enqueue(items, parent = state.folderId, sync = false) {
  stats ??= { uploaded: 0, skipped: 0, failed: 0, sync: false, started: Date.now() };
  stats.sync ||= sync;
  for (const { file, name } of items) queue.push({ file, name: name || file.name, parent, sync });
  $("#upload-more").hidden = true;
  keepAwake(true);
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
  const left = queue.length + active;
  const parts = [`已上传 ${s.uploaded}`];
  if (left) parts.push(`剩余 ${left}`);
  if (s.skipped) parts.push(`跳过 ${s.skipped}（已存在）`);
  if (s.failed) parts.push(`失败 ${s.failed}`);
  // 传了几个之后按平均速度估算剩余时间
  if (left && s.uploaded >= 3) {
    const eta = ((Date.now() - s.started) / s.uploaded) * left;
    parts.push(eta < 60_000 ? "预计不到 1 分钟" : `预计还要 ${formatDuration(eta)}`);
  }
  if (left) parts.push(wakeLock ? "屏幕会保持常亮，请别切走" : "请保持这个页面在前台、不要锁屏");
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
  keepAwake(false);
  refreshBackup();
  // 同步相册：一次选太多 iPhone 准备得很慢，分批选更顺；已传过的会自动跳过
  if (s.sync) { $("#upload-more").hidden = false; return; }
  if (!s.failed) setTimeout(() => { if (!stats) $("#uploads").hidden = true; }, 4000);
}

$("#upload-more").addEventListener("click", () => $("#album-input").click());

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
    let settled = false;
    let lastProgress = Date.now();
    let bodySent = false;
    const settle = (fn, arg) => {
      if (settled) return;
      settled = true;
      clearInterval(watchdog);
      fn(arg);
    };
    // 看门狗：上传挂住（既不报错也不结束）时主动中断，交给重试，避免整个队列被堵死。
    // 手机切到后台时计时器会暂停，回到前台后会立刻检查。
    const watchdog = setInterval(() => {
      const idle = Date.now() - lastProgress;
      if (!bodySent && idle > STALL_MS) {
        xhr.abort();
        settle(reject, new HttpError(0, "上传停滞（网络中断或页面进入了后台）"));
      } else if (bodySent && idle > SERVER_WAIT_MS) {
        xhr.abort();
        settle(reject, new HttpError(0, "服务端长时间没有响应"));
      }
    }, 5000);

    xhr.open("POST", `${state.api}/api/files?${q}`);
    for (const [k, v] of Object.entries(authHeaders())) xhr.setRequestHeader(k, v);
    xhr.setRequestHeader("Content-Type", file.type || "application/octet-stream");
    xhr.upload.onprogress = (e) => {
      lastProgress = Date.now();
      if (!e.lengthComputable) return;
      bar.value = e.loaded / e.total;
      status.textContent = e.loaded < e.total
        ? `${formatSize(e.loaded)} / ${formatSize(e.total)}`
        : "加密并写入 Telegram…";
    };
    xhr.upload.onload = () => {
      bodySent = true;
      lastProgress = Date.now();
      status.textContent = "加密并写入 Telegram…";
    };
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) return settle(resolve);
      let data = {};
      try { data = JSON.parse(xhr.responseText) || {}; } catch { /* 非 JSON */ }
      settle(reject, new HttpError(xhr.status, errorText(xhr.status, data.error, data.code), data.code));
    };
    xhr.onerror = () => settle(reject, new HttpError(0, "上传中断（网络错误或连接超时）"));
    xhr.onabort = () => settle(reject, new HttpError(0, "上传已中断"));
    xhr.send(file);
  });
}

// ---------- 选择文件时的等待提示 ----------
// iPhone 选很多照片时，系统要先把每张照片准备好（必要时转换格式、从 iCloud 下载原图），全部完成才把文件交给网页，
// 期间网页拿不到任何进度，看起来像卡住了。点了选择后显示提示和已等待的时间，收到文件或取消选择时隐藏。

const PICK_HINT_DELAY_MS = 1200;
const PICK_HINT_MAX_MS = 30 * 60_000;
let pickTimer;
let pickTick;
let pickStarted = 0;

function showPicking(on) {
  clearTimeout(pickTimer);
  clearInterval(pickTick);
  $("#picking").hidden = !on;
  if (!on) return;
  const el = $("#picking-time");
  const tick = () => {
    const ms = Date.now() - pickStarted;
    el.textContent = `已等 ${formatDuration(ms)}`;
    if (ms > PICK_HINT_MAX_MS) showPicking(false);
  };
  tick();
  pickTick = setInterval(tick, 1000);
}
$("#picking").addEventListener("click", () => showPicking(false)); // 点一下可以先收起

for (const sel of ["#file-input", "#album-input"]) {
  const input = $(sel);
  // 选择器打开后稍等再显示（桌面上很快选完就不会闪一下）
  input.addEventListener("click", () => {
    pickStarted = Date.now();
    clearTimeout(pickTimer);
    pickTimer = setTimeout(() => showPicking(true), PICK_HINT_DELAY_MS);
  });
  input.addEventListener("cancel", () => showPicking(false));
}

// 收到文件时说一下 iPhone 准备了多久、是什么格式（JPEG 多说明照片被转换过，HEIC 是原图）
function pickedNote(files) {
  const waited = pickStarted ? Date.now() - pickStarted : 0;
  pickStarted = 0;
  const kinds = {};
  let bytes = 0;
  for (const f of files) {
    const ext = (f.name.match(/\.([^.]+)$/)?.[1] || "其他").toUpperCase().replace("JPG", "JPEG");
    kinds[ext] = (kinds[ext] || 0) + 1;
    bytes += f.size;
  }
  const kindText = Object.entries(kinds).sort((a, b) => b[1] - a[1]).map(([k, n]) => `${k} ${n}`).join(" · ");
  return `收到 ${files.length} 个文件（${formatSize(bytes)}，${kindText}）`
    + (waited > 3000 ? `，iPhone 准备用了 ${formatDuration(waited)}` : "");
}

$("#file-input").addEventListener("change", (e) => {
  showPicking(false);
  const files = [...e.target.files];
  e.target.value = "";
  if (!files.length) return;
  toast(pickedNote(files));
  enqueue(files.map((file) => ({ file })));
});

// ---------- 同步相册 ----------

// 去掉自动加的序号：“IMG_1 (2).jpg” → “IMG_1.jpg”
const baseName = (n) => n.replace(/ \(\d+\)(?=\.[^.]*$|$)/, "");
const withSuffix = (n, i) => n.replace(/(\.[^.]*)?$/, (ext) => ` (${i})${ext}`);

// 把选中的照片 / 视频传到根目录的“相册”文件夹。已传过的（文件名 + 大小相同）自动跳过，
// 所以每次多选一些也不会重复上传；同名但内容不同的照片自动改名为“名字 (2)”。
async function syncAlbum(files) {
  if (!files.length) return;
  toast(`${pickedNote(files)}，正在检查哪些已经同步过…`);
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
  showPicking(false);
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
// 当前标签页里有会话（页面被重新加载）：直接进入；会话过期或被作废时第一个请求会退回登录页
if (state.session) enterApp();
else load();

// ---------- 网页版本检查 ----------
// 部署后已经打开的旧页面不认识服务端的新要求（比如看照片时的格子口令），要换成新版。
// WEB_VERSION 由 .github/workflows/pages.yml 换成提交号；本地开发是 "dev"，不检查。
const WEB_VERSION = "dev";

async function checkWebVersion() {
  if (WEB_VERSION === "dev") return;
  let latest;
  try {
    const resp = await fetch(`version.json?t=${Date.now()}`, { cache: "no-store" });
    if (!resp.ok) return;
    latest = (await resp.json()).version;
  } catch { return; } // 离线等情况：下次再查
  if (!latest || latest === WEB_VERSION) return;
  // 没在上传、没在答题或看照片时直接换新版（会话存在当前标签页，刷新不用重新登录），否则只提示
  const busy = active || queue.length || gridGatePending || $("#preview").open || $("#security").open;
  if (!busy) reloadTo(latest);
  else showUpdateBar(latest);
}

function reloadTo(version) {
  // 换个网址绕开 GitHub Pages 对页面的缓存；同一个版本只自动刷新一次，防止缓存没过期时来回刷新
  let tried = "";
  try { tried = sessionStorage.getItem("xiaoju.reloadedFor") || ""; } catch { /* 隐私模式 */ }
  if (tried === version) { showUpdateBar(version); return; }
  try { sessionStorage.setItem("xiaoju.reloadedFor", version); } catch { /* 隐私模式 */ }
  location.replace(`${location.pathname}?v=${encodeURIComponent(version)}${location.hash}`);
}

// 顶部提示条；登录框是模态的，打开时顶部点不到，所以登录框里也显示一个刷新入口
function showUpdateBar(version) {
  const bar = $("#update-bar");
  bar.dataset.version = version;
  bar.hidden = false;
  $("#login-update").hidden = false;
}

function updateNow() {
  if ((active || queue.length) && !confirm("还有文件在上传，刷新会中断上传。确定刷新？")) return;
  try { sessionStorage.removeItem("xiaoju.reloadedFor"); } catch { /* 隐私模式 */ }
  reloadTo($("#update-bar").dataset.version);
}
$("#update-btn").addEventListener("click", updateNow);
$("#login-update-btn").addEventListener("click", updateNow);

checkWebVersion();
// iPhone 上切到别的 App 再回来，页面不会重新加载：回到前台时再查一次
document.addEventListener("visibilitychange", () => { if (document.visibilityState === "visible") checkWebVersion(); });

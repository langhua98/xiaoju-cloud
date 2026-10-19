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
  chain: [],   // 从根目录下第一级到当前文件夹的完整条目
  items: [],   // 当前目录的条目（含缩略图地址）
  files: [],   // 其中的文件（不含文件夹）
  view: "list", // 当前目录的文件怎么显示：list / grid
  privateHere: false, // 当前文件夹（或上级）是私密的
  unlocked: false,    // 私密相册已验证解锁（切到后台后重新遮住）
  selecting: false,   // 多选中
  selected: new Map(), // 选中的条目：id → 条目
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

function toast(msg, { action = "", onAction = null } = {}) {
  const el = $("#toast");
  const host = [...document.querySelectorAll("dialog[open]")].pop() || document.body;
  if (el.parentElement !== host) host.append(el);
  el.querySelector(".msg").textContent = msg;
  const act = el.querySelector(".toast-act");
  act.hidden = !action;
  act.textContent = action;
  act.onclick = action ? () => { el.hidden = true; onAction(); } : null;
  el.hidden = false;
  playClass(el, "pop", 250);
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => { el.hidden = true; }, action ? 6000 : 3500);
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
  exitSelection();
  closeSheet();
  toggleMenu(false);
  state.unlocked = false;
  passkeyCount = null;
  $("#menu-btn").hidden = true;
  $("#fab").hidden = true;
  $("#scrubber").hidden = true;
  document.querySelectorAll("dialog[open]").forEach((d) => { if (d.id !== "login") d.close(); });
  // 主界面淡出后再弹出登录框
  const app = $("#app");
  if (!app.hidden) {
    app.style.transition = "opacity 0.2s ease";
    app.style.opacity = "0";
    await sleepMs(200);
    app.style.transition = app.style.opacity = "";
  }
  app.hidden = true;
  state.items = [];
  $("#rows").replaceChildren();
  $("#grid").replaceChildren();
  $("#albums").replaceChildren();
  openLogin(message);
}

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
  $("#menu-btn").hidden = false;
  $("#fab").hidden = false;
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
    toast("已用掉一个恢复码。登录后记得在右上角头像菜单的「登录安全」里给本机重新开启刷脸");
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
  await Promise.all([renderDevices(hint), gridCodesPending ? null : renderGridSec()]);
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
      : items.length ? "登录需要「令牌 + 刷脸」，私密相册也用刷脸解锁。手机丢了？在这里删掉那台设备即可。"
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
    passkeyCount = null;
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
    passkeyCount = null;
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

const SHORTCUT_FILE = "xiaoju-backup.shortcut"; // 备份照片：上传「文件」里的一个文件夹
const NEW_PHOTOS_SHORTCUT_FILE = "xiaoju-newphotos.shortcut"; // 自动备份新照片：直接从相册传最新的
const SHARE_SHORTCUT_FILE = "xiaoju-share.shortcut"; // 分享到小桔云盘：分享菜单里传几张

// 网页旁边有没有签好名的快捷指令（GitHub Pages 发布时生成）
async function shortcutAvailable(file) {
  try {
    const resp = await fetch(file, { method: "HEAD", cache: "no-store" });
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
    mk("a", { className: "button primary", href: SHORTCUT_FILE, textContent: "安装「备份照片」" }),
    mk("p", { className: "hint", textContent: "几百上千张都行（隐藏的照片、整个相册）：① 照片 App 打开要传的相簿（比如“已隐藏”）→ 选择 → 全选 → 分享 → "
      + "存储到“文件”，存进「我的 iPhone」里的一个新文件夹；② 运行「备份照片到小桔云盘」，选放进网盘的「隐藏相册」还是「相册」，"
      + "再选刚才那个文件夹。传的时候有通知报进度；传过的（名字、大小都一样）一开始就认出来直接跳过，中途停了再运行一次很快就接着传。"
      + "都传好后可以删掉那个文件夹（原照片还在相册里）。" }));
  const newPhotos = mk("div", { className: "key-install", hidden: true },
    mk("a", { className: "button", href: NEW_PHOTOS_SHORTCUT_FILE, textContent: "安装「自动备份新照片」" }),
    mk("p", { className: "hint", textContent: "不用选、不用先存到“文件”：直接从相册看最新的 300 张照片、视频，把还没传过的传进网盘的「相册」"
      + "（同名的照片还比拍摄日期，换过手机编号重复的也不会漏）。全程不弹窗，只发通知。隐藏的照片读不到，用上面的「备份照片」。" }),
    mk("p", { className: "hint", textContent: "每天自动备份：先手动运行一次（允许访问照片选「完全访问」，连接网盘、共享照片选「始终允许」，"
      + "第一次会把最新的 300 张里没传过的都传上去）；再在快捷指令 App 的「自动化」→「＋」→ 选「充电器」（接上电源时）"
      + "或「特定时间」（比如每天凌晨 3 点），选「立即运行」、关掉「运行时通知」，动作选「自动备份新照片」。" }));
  const share = mk("div", { className: "key-install", hidden: true },
    mk("a", { className: "button", href: SHARE_SHORTCUT_FILE, textContent: "安装「分享到小桔云盘」" }),
    mk("p", { className: "hint", textContent: "传几张、几十张：在照片 App 里选好 → 分享 → 「分享到小桔云盘」（第一次在分享菜单最下面「编辑操作」里加上），"
      + "选放进「相册」还是「隐藏相册」。传过的会自动跳过。几百张以上用「备份照片」，分享菜单一次给太多 iOS 会卡住。" }));
  const howto = mk("p", { className: "hint", hidden: true, textContent: "安装：点按钮后 Safari 问是否下载就选「下载」，再在下载列表里点开，"
    + "在快捷指令 App 里点「添加快捷指令」。第一次运行会问能否连接网盘、共享照片，选「始终允许」；问的时候别锁屏、别切走。" });
  const manual = mk("details", { className: "key-manual" },
    mk("summary", { textContent: "自己动手搭建快捷指令" }),
    mk("div", { className: "key-field" }, mk("span", { textContent: "地址" }), mk("code", { textContent: url }), copyButton(url)),
    mk("ol", { className: "key-steps" },
      mk("li", {}, "「快捷指令」App 点「＋」新建，添加「选择照片」，展开后打开「选择多张」（一次几十张以内，选太多会卡住）。"),
      mk("li", {}, "添加「重复每一项」，在里面添加「获取 URL 的内容」：URL 粘贴上面的地址；方法 ", code("POST"),
        "；头部加一项 ", code("X-File-Name"), "，值选变量「重复项目」后改成「名称」；请求体选「文件」= 「重复项目」。"),
      mk("li", {}, "运行。中途出错就再运行一次，传过的会自动跳过。")));
  box.replaceChildren(install, newPhotos, share, howto, manual);
  const [ok, okNew, okShare] = await Promise.all([SHORTCUT_FILE, NEW_PHOTOS_SHORTCUT_FILE, SHARE_SHORTCUT_FILE].map(shortcutAvailable));
  install.hidden = !ok;
  newPhotos.hidden = !okNew;
  share.hidden = !okShare;
  howto.hidden = !(ok || okNew || okShare);
  if (howto.hidden) manual.open = true;
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
  $("#backup-dot").hidden = true;
  $("#stats").hidden = true;
}

// 同时刷新网盘统计和备份进度（备份提示里的文件数来自整个网盘的统计）
async function refreshBackup() {
  if (!state.session) return;
  const [stats, backup] = await Promise.all([
    request("GET", "/api/stats").catch(() => null), // 旧版服务端没有这些接口，或网络暂时不通：不显示
    request("GET", "/api/backup/status").catch(() => null),
  ]);
  if (!state.session) return; // 请求期间已退出
  if (backup) renderBackupDot(backup, stats);
  else $("#backup-dot").hidden = true;
  refreshStats(stats);
}

// 统计栏：在根目录显示整个网盘的，进了文件夹只算这个文件夹里的（连同子文件夹）。
// drive：刚取到的整个网盘的统计（在根目录时直接用，不用再请求）
async function refreshStats(drive) {
  const folder = state.folderId;
  let st = folder ? null : drive;
  if (!st) st = await request("GET", folder ? `/api/stats?folder=${folder}` : "/api/stats").catch(() => null);
  if (!state.session || folder !== state.folderId) return; // 请求期间退出或换了目录
  const statsEl = $("#stats");
  if (st) renderStats(statsEl, st);
  statsEl.hidden = !st;
}

// 例：共 509 个文件 · 361 张照片 · 148 个视频 · 12 GB（在文件夹里只算这个文件夹的）
function renderStats(el, st) {
  const part = (n, unit, bytes) => {
    const span = mk("span", {}, mk("b", { textContent: n }), ` ${unit}`);
    if (bytes) span.title = formatSize(bytes);
    return span;
  };
  const items = [part(st.files, "个文件")];
  if (st.photos) items.push(part(st.photos, "张照片", st.photo_bytes));
  if (st.videos) items.push(part(st.videos, "个视频", st.video_bytes));
  if (st.others) items.push(part(st.others, "个其他文件"));
  if (st.folders) items.push(part(st.folders, "个文件夹"));
  items.push(mk("span", {}, mk("b", { textContent: formatSize(st.bytes) })));
  el.replaceChildren(...items.flatMap((x, i) => (i ? [mk("i", { className: "dot", textContent: "·" }), x] : [x])));
}

// ---------- 图标 ----------
// 线条图标（24×24，描边用当前文字颜色）。HTML 里写 data-icon="名字"，fillIcons() 把图标填进去。

const ICONS = {
  folder: '<path d="M3 7.5A2.5 2.5 0 0 1 5.5 5H9l2 2.5h7.5A2.5 2.5 0 0 1 21 10v7.5a2.5 2.5 0 0 1-2.5 2.5h-13A2.5 2.5 0 0 1 3 17.5z"/>',
  "folder-plus": '<path d="M3 7.5A2.5 2.5 0 0 1 5.5 5H9l2 2.5h7.5A2.5 2.5 0 0 1 21 10v7.5a2.5 2.5 0 0 1-2.5 2.5h-13A2.5 2.5 0 0 1 3 17.5z"/><path d="M12 11v6M9 14h6"/>',
  move: '<path d="M3 7.5A2.5 2.5 0 0 1 5.5 5H9l2 2.5h7.5A2.5 2.5 0 0 1 21 10v7.5a2.5 2.5 0 0 1-2.5 2.5h-13A2.5 2.5 0 0 1 3 17.5z"/><path d="M8.5 14h7M13 11.5l2.5 2.5-2.5 2.5"/>',
  image: '<rect x="3" y="4" width="18" height="16" rx="2.5"/><circle cx="9" cy="9.5" r="1.8"/><path d="M21 15.5l-5-5L5.5 20"/>',
  video: '<rect x="2.5" y="5" width="14" height="14" rx="2.5"/><path d="M16.5 10l5-3v10l-5-3"/>',
  audio: '<path d="M9 18V6l11-2v12"/><circle cx="6.5" cy="18" r="2.5"/><circle cx="17.5" cy="16" r="2.5"/>',
  doc: '<path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z"/><path d="M14 3v5h5M9 13h6M9 17h4"/>',
  archive: '<rect x="3" y="4" width="18" height="5" rx="1.5"/><path d="M5 9v9a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V9M10 13h4"/>',
  file: '<path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z"/><path d="M14 3v5h5"/>',
  lock: '<rect x="5" y="11" width="14" height="10" rx="2.5"/><path d="M8 11V8a4 4 0 0 1 8 0v3"/>',
  unlock: '<rect x="5" y="11" width="14" height="10" rx="2.5"/><path d="M8 11V8a4 4 0 0 1 7.6-1.7"/>',
  plus: '<path d="M12 5v14M5 12h14"/>',
  upload: '<path d="M12 16V4M7 9l5-5 5 5M4 20h16"/>',
  download: '<path d="M12 4v12M7 11l5 5 5-5M4 20h16"/>',
  trash: '<path d="M4 7h16M10 3.5h4M6 7l1 13h10l1-13M10 11v5.5M14 11v5.5"/>',
  check: '<path d="M5 12.5l4.5 4.5L19 7.5"/>',
  user: '<circle cx="12" cy="8.5" r="3.6"/><path d="M5 20a7 7 0 0 1 14 0"/>',
  shield: '<path d="M12 3l7 3v5c0 4.5-3 8.3-7 10-4-1.7-7-5.5-7-10V6z"/><path d="M9 12l2 2 4-4"/>',
  cloud: '<path d="M7 18.5a4.5 4.5 0 0 1-.6-9A6 6 0 0 1 18 9a4.5 4.5 0 0 1-.5 9.5z"/><path d="M9.5 13.5l2 2 3.5-3.5"/>',
  bolt: '<path d="M13 3L5 14h6l-1 7 8-11h-6z"/>',
  logout: '<path d="M9 4H6a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h3M16 17l5-5-5-5M21 12H9"/>',
  more: '<circle cx="5" cy="12" r="1.6" fill="currentColor" stroke="none"/><circle cx="12" cy="12" r="1.6" fill="currentColor" stroke="none"/><circle cx="19" cy="12" r="1.6" fill="currentColor" stroke="none"/>',
  edit: '<path d="M4 20h4L19 9l-4-4L4 16z"/><path d="M13.5 6.5l4 4"/>',
  restore: '<path d="M4 4v6h6"/><path d="M5 15a8 8 0 1 0 1.9-8.4L4 10"/>',
  chevron: '<path d="M9 6l6 6-6 6"/>',
  back: '<path d="M15 6l-6 6 6 6"/>',
  phone: '<rect x="6.5" y="2.5" width="11" height="19" rx="2.5"/><path d="M10.5 18.5h3"/>',
};

function icon(name, cls = "ic") {
  const t = document.createElement("template");
  t.innerHTML = `<svg class="${cls}" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${ICONS[name] || ""}</svg>`;
  return t.content.firstChild;
}

function fillIcons(root = document) {
  root.querySelectorAll("[data-icon]").forEach((el) => {
    if (!el.querySelector(":scope > svg.ic")) el.prepend(icon(el.dataset.icon));
  });
}

function fileIcon(f) {
  if (f.type === "folder") return "folder";
  const m = f.mime || "";
  if (m.startsWith("image/")) return "image";
  if (m.startsWith("video/")) return "video";
  if (m.startsWith("audio/")) return "audio";
  if (m === "application/pdf" || m.startsWith("text/")) return "doc";
  if (/zip|rar|7z|tar|gzip/.test(m)) return "archive";
  return "file";
}

// ---------- 目录浏览 ----------
// 根目录：相册（文件夹）是带封面的卡片，散落的文件放在下面的“其他文件”里。
// 进了文件夹：子文件夹也是卡片，照片、视频按拍摄日期分组排成网格（也可以切成列表）。

function currentFolderFromHash() {
  const m = location.hash.match(/^#\/f\/([0-9a-f-]{36})$/i);
  return m ? m[1] : null;
}

// 从当前文件夹一路往上，得到 [根目录下的第一级, …, 当前文件夹]（每一级是完整的条目，带 private 等）
async function crumbsFor(id) {
  const chain = [];
  for (let cur = id, guard = 0; cur && guard < 64; guard++) {
    const f = await request("GET", `/api/files/${cur}`);
    chain.unshift(f);
    cur = f.parent_id;
  }
  return chain;
}

// 上级路径（当前这一级是大标题，不重复）：‹ 全部文件 › 照片
function renderCrumbs(chain) {
  const nav = $("#crumbs");
  nav.replaceChildren();
  nav.hidden = !chain.length;
  if (!chain.length) return;
  const items = [{ id: null, name: "全部文件" }, ...chain.slice(0, -1)];
  const up = items[items.length - 1];
  nav.append(mk("a", { className: "up", href: up.id ? `#/f/${up.id}` : "#/", ariaLabel: "返回上一级" }, icon("back")));
  items.forEach((c, i) => {
    if (i > 0) nav.append(mk("span", { className: "sep", textContent: "›" }));
    nav.append(mk("a", { href: c.id ? `#/f/${c.id}` : "#/", textContent: c.name }));
  });
}

function renderHead() {
  const cur = state.chain[state.chain.length - 1];
  $("#album-title").textContent = cur ? cur.name : "全部文件";
  document.title = cur ? `${cur.name} · 小桔云盘` : "小桔云盘";
  $("#folder-menu-btn").hidden = !cur;
  $("#sheet-media-hint").textContent = cur ? "放进这个文件夹，传过的自动跳过" : `放进「${ALBUM}」，传过的自动跳过`;
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
  const prevFolder = state.folderId;
  state.folderId = currentFolderFromHash();
  if (state.folderId !== prevFolder) {
    exitSelection();
    scrollTo(0, 0);
  }
  const drop = $("#drop");
  $("#empty").hidden = true;
  // 已有内容时先变淡（切换目录、刷新），没有内容时显示骨架占位
  if (state.items.length || $("#albums").childElementCount) drop.classList.add("stale");
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
    state.chain = chain;
    state.privateHere = chain.some((c) => c.private);
    renderCrumbs(chain);
    renderHead();
    renderItems(list.items);
    refreshStats();
  } catch (err) {
    if (seq !== loadSeq) return;
    $("#albums").querySelectorAll(".skeleton").forEach((el) => el.remove());
    if (err.status === 404 && state.folderId) { location.hash = "#/"; return; }
    fail(err);
  } finally {
    progress.done();
    if (seq === loadSeq) drop.classList.remove("stale");
  }
}

function showSkeleton() {
  const box = $("#albums");
  box.hidden = false;
  box.replaceChildren(...Array.from({ length: 4 }, () => mk("div", { className: "album skeleton" },
    mk("span", { className: "cover" }), mk("span", { className: "aname" }), mk("span", { className: "acount" }))));
}

// 条目内容没变时沿用原来的元素（上传期间列表会反复刷新，缩略图不用重新加载、不闪）
const itemKey = (f) => [f.name, f.size, f.thumb || "", f.duration || 0, f.counts ? countsText(f.counts) : "", f.cover || "", f.private ? 1 : 0].join("|");

function renderItems(items) {
  state.items = items;
  const folders = items.filter((f) => f.type === "folder");
  const files = items.filter((f) => f.type !== "folder");
  state.files = files;
  const atRoot = !state.folderId;
  renderAlbums(folders);
  // 根目录的文件放在相册下面，用列表；文件夹里照片、视频多就用网格（可以切换）
  state.view = atRoot ? "list" : chooseView(files);
  const grid = state.view === "grid" && files.length > 0;
  $("#view-toggle").hidden = atRoot || !files.length;
  document.querySelectorAll("#view-toggle button").forEach((b) => b.setAttribute("aria-pressed", String(b.dataset.view === state.view)));
  const others = $("#others-title");
  others.hidden = !(atRoot && files.length && folders.length);
  others.textContent = `其他文件 · ${files.length}`;
  $("#list-table").hidden = grid || !files.length;
  $("#grid").hidden = !grid;
  if (grid) {
    $("#rows").replaceChildren();
    renderGrid(files);
  } else {
    $("#grid").replaceChildren();
    renderRows(files);
  }
  $("#select-btn").hidden = !items.length;
  $("#empty").hidden = items.length > 0;
  if (!items.length) playClass($("#empty"), "fade-in", 500);
  applyLock();
  updateScrubber();
}

const viewKey = () => `xiaoju.view.${state.folderId || "root"}`;

// 没手动选过时：照片、视频占多数的文件夹（比如“相册”）用网格，其他用列表
function chooseView(files) {
  const saved = store.get(viewKey());
  if (saved === "grid" || saved === "list") return saved;
  const media = files.filter(isMedia).length;
  return media >= 4 && media >= files.length * 0.6 ? "grid" : "list";
}

$("#view-toggle").addEventListener("click", (e) => {
  const b = e.target.closest("button[data-view]");
  if (!b || b.dataset.view === state.view) return;
  store.set(viewKey(), b.dataset.view);
  renderItems(state.items);
});

// 文件夹里（连同子文件夹）有几张照片、几个视频：“323 张照片 · 119 个视频”
function countsText(c) {
  const parts = [];
  if (c.photos) parts.push(`${c.photos} 张照片`);
  if (c.videos) parts.push(`${c.videos} 个视频`);
  if (c.others) parts.push(`${c.others} 个文件`);
  return parts.join(" · ") || "空文件夹";
}

// ---------- 相册卡片 ----------

function renderAlbums(folders) {
  const box = $("#albums");
  box.hidden = !folders.length;
  const old = new Map([...box.children].map((el) => [el.dataset.id, el]));
  let n = 0;
  box.replaceChildren(...folders.map((f) => {
    const prev = old.get(f.id);
    if (prev && prev.dataset.key === itemKey(f)) return prev;
    const card = cardFor(f);
    if (!prev) {
      card.classList.add("appear");
      card.style.setProperty("--i", Math.min(n++, 12));
    }
    return card;
  }));
}

function cardFor(f) {
  const card = mk("button", { type: "button", className: "album" + (f.private ? " private" : "") });
  card.dataset.id = f.id;
  card.dataset.key = itemKey(f);
  const cover = mk("span", { className: "cover" });
  const placeholder = icon("folder", "ic big");
  if (f.cover) {
    const img = thumbImg({ thumb: f.cover }, "thumb", () => img.replaceWith(placeholder));
    cover.append(img);
  } else {
    cover.append(placeholder);
  }
  if (f.private) cover.append(mk("span", { className: "lock-badge" }, icon("lock")));
  cover.append(mk("span", { className: "check" }, icon("check")));
  card.append(cover,
    mk("span", { className: "aname", textContent: f.name }),
    mk("span", { className: "acount", textContent: f.counts ? countsText(f.counts) : "" }));
  if (state.selected.has(f.id)) card.classList.add("is-selected");
  bindItem(card, f, () => { location.hash = `#/f/${f.id}`; });
  return card;
}

// ---------- 列表 ----------

function renderRows(files) {
  const rows = $("#rows");
  // 只给新出现的条目做淡入，刷新时没变的行直接沿用
  const old = new Map([...rows.querySelectorAll("tr[data-id]")].map((tr) => [tr.dataset.id, tr]));
  let n = 0;
  rows.replaceChildren(...files.map((f) => {
    const prev = old.get(f.id);
    if (prev && prev.dataset.key === itemKey(f)) {
      prev.classList.remove("appear"); // 重新插入时不要再播一遍淡入
      return prev;
    }
    const tr = rowFor(f);
    if (!prev) {
      tr.classList.add("appear");
      tr.style.setProperty("--i", Math.min(n++, 12));
    }
    return tr;
  }));
}

function rowFor(f) {
  const tr = document.createElement("tr");
  tr.dataset.id = f.id;
  tr.dataset.key = itemKey(f);
  const name = mk("td", { className: "name", title: f.name });
  const fallback = mk("span", { className: "ficon" }, icon(fileIcon(f)));
  let lead = fallback;
  if (f.thumb) {
    const img = thumbImg(f, "row-thumb", () => img.replaceWith(fallback));
    lead = img;
  }
  // 照片、视频显示拍摄时间（列表也按它排），其他显示修改时间
  const when = f.taken_at || f.updated_at;
  name.append(mk("div", { className: "cell" }, mk("span", { className: "check" }, icon("check")), lead,
    mk("span", { className: "fname" }, mk("span", { textContent: f.name }),
      mk("small", { textContent: `${formatSize(f.size)} · ${formatDate(when)}` }))));
  const size = mk("td", { className: "size", textContent: formatSize(f.size) });
  const date = mk("td", { className: "date", textContent: formatDate(when), title: (f.taken_at ? "拍摄于 " : "修改于 ") + formatDate(when) });
  tr.append(name, size, date);
  if (state.selected.has(f.id)) tr.classList.add("is-selected");
  bindItem(tr, f, () => openItem(f));
  return tr;
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

// “刚刚”“5 分钟前”“今天 21:05”“昨天 21:05”“10月18日 21:05”“2025年10月18日”
function relTime(s) {
  const d = new Date(s);
  const ms = Date.now() - d.getTime();
  if (ms < 60_000) return "刚刚";
  if (ms < 3600_000) return `${Math.floor(ms / 60_000)} 分钟前`;
  const pad = (x) => String(x).padStart(2, "0");
  const hm = `${pad(d.getHours())}:${pad(d.getMinutes())}`;
  const days = dayDiff(d, new Date());
  if (days === 0) return `今天 ${hm}`;
  if (days === 1) return `昨天 ${hm}`;
  if (d.getFullYear() === new Date().getFullYear()) return `${d.getMonth() + 1}月${d.getDate()}日 ${hm}`;
  return `${d.getFullYear()}年${d.getMonth() + 1}月${d.getDate()}日`;
}

// 两个时间相差几个自然日（按本地日期）
const dayDiff = (a, b) => Math.round((new Date(b.getFullYear(), b.getMonth(), b.getDate()) - new Date(a.getFullYear(), a.getMonth(), a.getDate())) / 86400_000);

// ---------- 操作 ----------

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
    clickLink(await signedURL(f, false));
  } catch (err) { fail(err); }
}

// 弹窗打开时页面其他部分点不到，链接放在最上层的弹窗里
function clickLink(href) {
  const host = [...document.querySelectorAll("dialog[open]")].pop() || document.body;
  const a = Object.assign(document.createElement("a"), { href, rel: "noopener" });
  host.append(a);
  a.click();
  a.remove();
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

// ---------- 网格与缩略图 ----------

const isMedia = (f) => f.type === "file" && /^(image|video)\//.test(f.mime || "");
// 能在大图查看里看的：有缩略图，或者浏览器能直接显示的照片、视频
const inGallery = (f) => isMedia(f) && (!!f.thumb || ["img", "video"].includes(previewKind(f.mime)));

function openItem(f) {
  // 私密相册还遮着：先验证
  if (isLocked()) { unlockPrivate().then((ok) => { if (ok) openItem(f); }); return; }
  if (inGallery(f)) openViewer(f);
  else if (previewKind(f.mime)) preview(f);
  else download(f);
}

const WEEKDAY_SHORT = ["周日", "周一", "周二", "周三", "周四", "周五", "周六"];
const takenOf = (f) => new Date(f.taken_at || f.created_at);
const dayKey = (d) => `${d.getFullYear()}-${d.getMonth() + 1}-${d.getDate()}`;
const monthLabel = (d) => `${d.getFullYear()}年${d.getMonth() + 1}月`;

// 日期分组的标题：“今天”“昨天”“10月18日 周六”，不是今年的带上年份
function dayLabel(d) {
  const days = dayDiff(d, new Date());
  if (days === 0) return "今天";
  if (days === 1) return "昨天";
  const md = `${d.getMonth() + 1}月${d.getDate()}日 ${WEEKDAY_SHORT[d.getDay()]}`;
  return d.getFullYear() === new Date().getFullYear() ? md : `${d.getFullYear()}年${md}`;
}

// 照片墙：按拍摄日期分组（服务端已经按拍摄时间从新到旧排好），每组前面一个日期标题。
// 照片稀疏（平均一天不到 4 张、跨了很多天，比如截图、导出的旧照片）时按月分组，免得满屏都是只有一张的日期
function renderGrid(files) {
  const grid = $("#grid");
  const old = new Map([...grid.children].map((el) => [el.dataset.id || el.dataset.day, el]));
  const days = new Set(files.map((f) => dayKey(takenOf(f)))).size;
  const byMonth = days > 12 && files.length / days < 4;
  const out = [];
  let lastDay = "";
  let n = 0;
  for (const f of files) {
    const when = takenOf(f);
    const day = byMonth ? `m${when.getFullYear()}-${when.getMonth() + 1}` : "d" + dayKey(when);
    if (day !== lastDay) {
      lastDay = day;
      const label = byMonth ? monthLabel(when) : dayLabel(when);
      let h = old.get(day);
      if (!h || h.textContent !== label) h = mk("h3", { className: "day", textContent: label });
      h.dataset.day = day;
      h.dataset.month = monthLabel(when);
      out.push(h);
    }
    const prev = old.get(f.id);
    if (prev && prev.dataset.key === itemKey(f)) {
      prev.classList.remove("appear"); // 重新插入时不要再播一遍淡入
      out.push(prev);
      continue;
    }
    const tile = tileFor(f);
    if (!prev) {
      tile.classList.add("appear");
      tile.style.setProperty("--i", Math.min(n++, 24));
    }
    out.push(tile);
  }
  grid.replaceChildren(...out);
}

function tileFor(f) {
  const tile = mk("button", { type: "button", className: "tile" });
  tile.dataset.id = f.id;
  tile.dataset.key = itemKey(f);
  tile.title = f.name;
  const check = mk("span", { className: "check" }, icon("check"));
  // 普通文件、缩略图加载失败的：图标 + 名字
  const plain = () => {
    tile.classList.remove("media");
    tile.replaceChildren(icon(fileIcon(f), "ic ticon"), mk("span", { className: "tname", textContent: f.name }), check);
  };
  if (state.selected.has(f.id)) tile.classList.add("is-selected");
  bindItem(tile, f, () => openItem(f));
  if (!f.thumb) {
    plain();
    return tile;
  }
  tile.classList.add("media");
  tile.setAttribute("aria-label", f.name);
  tile.append(thumbImg(f, "thumb", plain));
  if (f.mime.startsWith("video/")) tile.append(videoBadge(f));
  tile.append(check);
  return tile;
}

function thumbImg(f, className, onFail) {
  const img = document.createElement("img");
  img.className = className;
  img.alt = "";
  img.decoding = "async";
  img.draggable = false;
  img.dataset.src = state.api + f.thumb;
  img.addEventListener("thumbfail", onFail, { once: true });
  thumbQueue.observe(img);
  return img;
}

function videoBadge(f) {
  const b = document.createElement("span");
  b.className = "badge";
  b.innerHTML = '<svg viewBox="0 0 10 10" aria-hidden="true"><path d="M2.2 1.2v7.6L8.6 5z"/></svg>';
  if (f.duration) b.append(clipLength(f.duration));
  return b;
}

function clipLength(sec) {
  const s = Math.max(1, Math.round(sec));
  const pad = (x) => String(x).padStart(2, "0");
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  return h ? `${h}:${pad(m)}:${pad(s % 60)}` : `${m}:${pad(s % 60)}`;
}

// 缩略图按需加载：滚到附近才请求，最多同时 4 个（服务端第一次要现做，也给点开原图等请求留出连接）
const thumbQueue = (() => {
  const MAX = 4;
  let inFlight = 0;
  const waiting = [];
  const pump = () => {
    while (inFlight < MAX && waiting.length) {
      const img = waiting.shift();
      if (!img.isConnected || img.src) continue;
      inFlight++;
      let freed = false;
      const free = () => {
        if (freed) return;
        freed = true;
        clearTimeout(timer);
        inFlight--;
        pump();
      };
      const timer = setTimeout(free, 20000); // 太久没回来先让出名额，回来了照样显示
      img.onload = () => { img.classList.add("loaded"); img.parentElement?.classList.add("ready"); free(); };
      img.onerror = () => { img.dispatchEvent(new Event("thumbfail")); free(); };
      img.src = img.dataset.src;
    }
  };
  const io = "IntersectionObserver" in window
    ? new IntersectionObserver((entries) => {
      for (const e of entries) {
        if (!e.isIntersecting) continue;
        io.unobserve(e.target);
        waiting.push(e.target);
      }
      pump();
    }, { rootMargin: "600px 0px" })
    : null;
  return {
    observe(img) {
      if (io) io.observe(img);
      else { waiting.push(img); setTimeout(pump); }
    },
  };
})();

// ---------- 点按、长按、多选 ----------
// 点一下打开；长按（或点“选择”）进入多选，之后点一下是选中 / 取消。底部是“下载 / 移动 / 删除”。

const LONG_PRESS_MS = 450;

function bindItem(el, f, open) {
  let timer = 0;
  let start = null;
  let pressed = false; // 这次是长按，接下来的 click 不算
  const cancel = () => { clearTimeout(timer); start = null; };
  const longPress = () => {
    pressed = true;
    if (!state.selecting) enterSelection();
    toggleSelect(f, true);
    navigator.vibrate?.(10);
  };
  el.addEventListener("pointerdown", (e) => {
    if (e.pointerType === "mouse" && e.button !== 0) return;
    pressed = false;
    start = { x: e.clientX, y: e.clientY };
    clearTimeout(timer);
    timer = setTimeout(longPress, LONG_PRESS_MS);
  });
  el.addEventListener("pointermove", (e) => {
    if (start && Math.hypot(e.clientX - start.x, e.clientY - start.y) > 10) cancel();
  });
  for (const ev of ["pointerup", "pointercancel", "pointerleave"]) el.addEventListener(ev, cancel);
  // 长按不弹系统菜单；电脑上右键也是选中
  el.addEventListener("contextmenu", (e) => {
    e.preventDefault();
    if (!pressed) { cancel(); longPress(); }
  });
  el.addEventListener("click", (e) => {
    if (pressed) { pressed = false; e.preventDefault(); return; }
    if (state.selecting) { toggleSelect(f); return; }
    open();
  });
}

function enterSelection() {
  state.selecting = true;
  document.body.classList.add("selecting");
  $("#selbar").hidden = false;
  closeSheet();
  updateSelbar();
}

function exitSelection() {
  if (!state.selecting) return;
  state.selecting = false;
  state.selected.clear();
  document.body.classList.remove("selecting");
  $("#selbar").hidden = true;
  document.querySelectorAll(".is-selected").forEach((el) => el.classList.remove("is-selected"));
}

function toggleSelect(f, on) {
  const now = on ?? !state.selected.has(f.id);
  if (now) state.selected.set(f.id, f);
  else state.selected.delete(f.id);
  document.querySelectorAll(`[data-id="${f.id}"]`).forEach((el) => el.classList.toggle("is-selected", now));
  updateSelbar();
}

function updateSelbar() {
  const picked = [...state.selected.values()];
  const n = picked.length;
  $("#sel-count").textContent = n ? `已选 ${n} 项` : "选择项目";
  $("#sel-all").textContent = n && n === state.items.length ? "全不选" : "全选";
  $("#sel-download").disabled = !picked.some((f) => f.type === "file");
  $("#sel-move").disabled = !n;
  $("#sel-delete").disabled = !n;
}

$("#select-btn").addEventListener("click", () => (state.selecting ? exitSelection() : enterSelection()));
$("#sel-cancel").addEventListener("click", exitSelection);
$("#sel-all").addEventListener("click", () => {
  const all = state.selected.size !== state.items.length;
  for (const f of state.items) toggleSelect(f, all);
});
$("#sel-download").addEventListener("click", async () => {
  const files = [...state.selected.values()].filter((f) => f.type === "file");
  if (files.length > 10 && !confirm(`一次下载 ${files.length} 个文件？浏览器可能会问是否允许下载多个文件。`)) return;
  if (isLocked() && !(await unlockPrivate())) return;
  for (const [i, f] of files.entries()) {
    if (i) await sleepMs(700); // 连着触发太快，浏览器会丢掉后面的
    await download(f);
  }
  exitSelection();
});
$("#sel-move").addEventListener("click", () => moveItems([...state.selected.values()]));
$("#sel-delete").addEventListener("click", () => trashItems([...state.selected.values()]));

const itemsLabel = (list) => (list.length === 1 ? `“${list[0].name}”` : `${list.length} 项`);

// 移入回收站，提示里可以撤销；文件夹先确认一下（里面的照片会跟着一起进回收站）
async function trashItems(list) {
  if (!list.length) return [];
  const folders = list.filter((f) => f.type === "folder");
  if (folders.length) {
    const inside = folders.reduce((n, f) => n + (f.counts ? f.counts.photos + f.counts.videos + f.counts.others : 0), 0);
    const what = folders.length === 1 ? `文件夹“${folders[0].name}”` : `${folders.length} 个文件夹`;
    if (!confirm(`把${what}移入回收站？` + (inside ? `里面的 ${inside} 个文件会跟着一起移入。` : "") + "之后可以在回收站里恢复。")) return [];
  }
  const done = [];
  for (const f of list) {
    try {
      await request("DELETE", `/api/files/${f.id}`);
      done.push(f);
    } catch (err) {
      if (err.status === 401) { fail(err); return done; }
    }
  }
  exitSelection();
  load();
  refreshBackup();
  if (!done.length) { toast("删除失败，请稍后再试"); return done; }
  const failed = list.length - done.length;
  toast(`已移入回收站：${itemsLabel(done)}` + (failed ? `（${failed} 项没删掉）` : ""), {
    action: "撤销",
    async onAction() {
      let back = 0;
      for (const f of done) {
        try { await request("POST", `/api/files/${f.id}/restore`); back++; } catch { /* 原处已有同名的：留在回收站 */ }
      }
      toast(back === done.length ? "已恢复" : `已恢复 ${back} 项，其余的原处已有同名文件，留在回收站里`);
      load();
      refreshBackup();
    },
  });
  return done;
}

// 移动：选目标文件夹，一项项移过去；目标里有同名的跳过
async function moveItems(list) {
  if (!list.length) return;
  const target = await pickFolder(list);
  if (target === undefined) return;
  let moved = 0;
  let clash = 0;
  let other = 0;
  for (const f of list) {
    if ((f.parent_id || null) === target) continue;
    try {
      await request("PATCH", `/api/files/${f.id}`, { parent_id: target });
      moved++;
    } catch (err) {
      if (err.status === 401) { fail(err); return; }
      if (err.status === 409) clash++;
      else other++;
    }
  }
  exitSelection();
  load();
  let msg = `已移动 ${moved} 项`;
  if (clash) msg += `，${clash} 项因为目标文件夹里有同名的没移动`;
  if (other) msg += `，${other} 项移动失败`;
  toast(msg);
}

// 选一个文件夹（null 是根目录）；取消返回 undefined。正在移动的文件夹本身不能选（不能移进自己里面）
function pickFolder(moving) {
  return new Promise((resolve) => {
    const dlg = $("#move-dlg");
    const excluded = new Set(moving.filter((f) => f.type === "folder").map((f) => f.id));
    const path = []; // 进到哪一级了：[{id, name}]
    let finished = false;
    const finish = (v) => {
      if (finished) return;
      finished = true;
      closeDialog(dlg);
      resolve(v);
    };
    const cur = () => (path.length ? path[path.length - 1].id : null);
    async function show() {
      const list = $("#move-list");
      list.replaceChildren(mk("li", { className: "hint", textContent: "正在读取…" }));
      const crumbs = $("#move-crumbs");
      crumbs.replaceChildren();
      [{ id: null, name: "全部文件" }, ...path].forEach((c, i, all) => {
        if (i) crumbs.append(mk("span", { className: "sep", textContent: "›" }));
        const a = mk("a", { href: "#", textContent: c.name });
        a.addEventListener("click", (e) => { e.preventDefault(); path.length = i; show(); });
        if (i === all.length - 1) a.setAttribute("aria-current", "page");
        crumbs.append(a);
      });
      $("#move-here").disabled = cur() === (state.folderId || null) && moving.every((f) => (f.parent_id || null) === cur());
      let res;
      try {
        res = await request("GET", `/api/folders/${cur() || "root"}/children`);
      } catch (err) {
        list.replaceChildren(mk("li", { className: "error", textContent: err.message }));
        return;
      }
      const folders = res.items.filter((f) => f.type === "folder" && !excluded.has(f.id));
      list.replaceChildren(...(folders.length ? folders.map((f) => {
        const b = mk("button", { type: "button" }, icon(f.private ? "lock" : "folder"), mk("span", { textContent: f.name }), icon("chevron", "ic go"));
        b.addEventListener("click", () => { path.push({ id: f.id, name: f.name }); show(); });
        return mk("li", {}, b);
      }) : [mk("li", { className: "hint", textContent: "这里没有子文件夹" })]));
    }
    $("#move-title").textContent = `把 ${itemsLabel(moving)} 移动到`;
    $("#move-here").onclick = () => finish(cur());
    dlg.onclose = () => finish(undefined);
    dlg.querySelector("[data-close]").onclick = () => finish(undefined);
    if (!dlg.open) dlg.showModal();
    show();
  });
}

// ---------- 当前文件夹的操作（⋯） ----------

$("#folder-menu-btn").addEventListener("click", () => {
  const f = state.chain[state.chain.length - 1];
  if (!f) return;
  const dlg = $("#folder-dlg");
  $("#folder-dlg-title").textContent = f.name;
  const priv = dlg.querySelector('[data-act="private"]');
  priv.querySelector(".label").textContent = f.private ? "取消私密" : "设为私密";
  priv.querySelector("svg")?.replaceWith(icon(f.private ? "unlock" : "lock"));
  dlg.showModal();
});

$("#folder-dlg").addEventListener("click", async (e) => {
  const b = e.target.closest("button[data-act]");
  if (!b) return;
  const f = state.chain[state.chain.length - 1];
  await closeDialog($("#folder-dlg"));
  if (!f) return;
  const act = b.dataset.act;
  try {
    if (act === "rename") {
      const name = prompt("新名字", f.name);
      if (!name || !name.trim() || name.trim() === f.name) return;
      await request("PATCH", `/api/files/${f.id}`, { name: name.trim() });
      toast("已重命名");
      load();
    } else if (act === "private") {
      // 取消私密要先验证（不然拿着已登录手机的人点一下就能看了）
      if (f.private && !(await unlockPrivate())) return;
      await request("PATCH", `/api/files/${f.id}`, { private: !f.private });
      toast(f.private ? "已取消私密" : "已设为私密：照片默认遮住，验证后才显示");
      load();
    } else if (act === "move") {
      await moveItems([f]);
    } else if (act === "delete") {
      const done = await trashItems([f]);
      if (done.length) location.hash = f.parent_id ? `#/f/${f.parent_id}` : "#/";
    }
  } catch (err) { fail(err); }
});

// ---------- 私密相册 ----------
// 私密文件夹（或它的上级是私密的）里的缩略图、根目录上私密相册的封面默认遮住。验证后显示，
// 切到别的 App、锁屏后重新遮住。只是防旁人瞄到：原图本来就要刷脸登录、答格子题才能看。

const isLocked = () => state.privateHere && !state.unlocked;
let passkeyCount = null; // 注册了几台刷脸设备：有就用刷脸解锁，没有就用格子口令（都没开时点一下就显示）

function applyLock() {
  const drop = $("#drop");
  drop.classList.toggle("locked", isLocked());
  drop.classList.toggle("unlocked", state.unlocked);
  const bar = $("#private-bar");
  bar.hidden = !state.privateHere;
  bar.classList.toggle("open", state.unlocked);
  bar.querySelector(".text").textContent = state.unlocked ? "已解锁，切到别的 App 或锁屏后会重新遮住" : "私密相册，照片已遮住";
  $("#unlock-btn").textContent = state.unlocked ? "现在遮住" : "解锁查看";
  $("#unlock-btn").className = state.unlocked ? "small" : "primary small";
  bar.querySelector("svg")?.replaceWith(icon(state.unlocked ? "unlock" : "lock"));
}

function relock() {
  if (!state.unlocked) return;
  state.unlocked = false;
  applyLock();
  if (state.privateHere && vwDialog.open) vwDialog.close();
}

$("#unlock-btn").addEventListener("click", () => (state.unlocked ? relock() : unlockPrivate()));
document.addEventListener("visibilitychange", () => { if (document.visibilityState === "hidden") relock(); });
window.addEventListener("pagehide", relock);

async function unlockPrivate() {
  if (state.unlocked) return true;
  if (passkeyCount === null) {
    try { passkeyCount = ((await request("GET", "/api/auth/passkeys")).items || []).length; } catch { passkeyCount = 0; }
  }
  let ok;
  if (passkeyCount > 0 && webauthnSupported()) {
    ok = await unlockWithFace();
  } else {
    if (!gridStatus) {
      try { gridStatus = await request("GET", "/api/auth/grid"); } catch { /* 没开 */ }
    }
    ok = gridStatus?.enabled ? await gridGate() : true;
  }
  if (ok) {
    state.unlocked = true;
    applyLock();
  }
  return ok;
}

// 刷脸解锁：和登录一样调起系统的 Face ID / 指纹
function unlockWithFace() {
  return new Promise((resolve) => {
    const dlg = $("#unlock-dlg");
    const btn = $("#unlock-face");
    const errEl = $("#unlock-error");
    let opts = null;
    let finished = false;
    const begin = () => request("POST", "/api/auth/passkey/login/begin").then((r) => { opts = { ...r, at: Date.now() }; });
    const finish = (ok) => {
      if (finished) return;
      finished = true;
      closeDialog(dlg);
      resolve(ok);
    };
    errEl.hidden = true;
    setButtonBusy(btn, false, "正在验证…", "使用 Face ID / 指纹");
    begin().catch(() => { /* 点击时再请求 */ });
    btn.onclick = async () => {
      errEl.hidden = true;
      setButtonBusy(btn, true, "正在验证…", "使用 Face ID / 指纹");
      try {
        if (!opts || Date.now() - opts.at > 120_000) await begin();
        const o = opts;
        opts = null;
        const cred = await navigator.credentials.get({ publicKey: decodeRequestOptions(o.options.publicKey) });
        await request("POST", `/api/auth/passkey/login/finish?ceremony=${encodeURIComponent(o.ceremony)}`, encodeCredential(cred));
        setButtonBusy(btn, false, "正在验证…", "使用 Face ID / 指纹");
        finish(true);
      } catch (err) {
        setButtonBusy(btn, false, "正在验证…", "使用 Face ID / 指纹");
        errEl.textContent = webauthnError(err);
        errEl.hidden = false;
        begin().catch(() => { /* 点击时再请求 */ });
      }
    };
    dlg.onclose = () => finish(false);
    dlg.querySelector("[data-close]").onclick = () => finish(false);
    dlg.showModal();
  });
}

// ---------- 时间滑条 ----------
// 照片多时右边出现一个滑块：滚动时显示，按住上下拖可以快速跳到某个月，旁边气泡显示是哪年哪月。

const scrub = { el: $("#scrubber"), heads: [], hideTimer: 0, drag: null };
const scrubHandle = scrub.el.querySelector(".handle");
const scrubBubble = scrub.el.querySelector(".bubble");
const SCRUB_MIN_ITEMS = 60;

function updateScrubber() {
  const grid = $("#grid");
  const on = !grid.hidden && state.files.length >= SCRUB_MIN_ITEMS;
  scrub.el.hidden = !on;
  if (on) measureHeads();
}

function measureHeads() {
  scrub.heads = [...$("#grid").querySelectorAll(".day")].map((h) => ({ y: h.getBoundingClientRect().top + scrollY, label: h.dataset.month }));
}

const maxScroll = () => Math.max(1, document.documentElement.scrollHeight - innerHeight);
const scrubTrack = () => scrub.el.clientHeight - scrubHandle.offsetHeight;

function monthAt(y) {
  let label = scrub.heads[0]?.label || "";
  for (const h of scrub.heads) {
    if (h.y > y + innerHeight * 0.3) break;
    label = h.label;
  }
  return label;
}

function placeHandle() {
  const top = Math.min(1, scrollY / maxScroll()) * scrubTrack();
  scrubHandle.style.transform = `translateY(${top}px)`;
  scrubBubble.style.transform = `translateY(${top}px)`;
  scrubBubble.textContent = monthAt(scrollY);
}

function showScrubber() {
  scrub.el.classList.add("active");
  clearTimeout(scrub.hideTimer);
  scrub.hideTimer = setTimeout(() => { if (!scrub.drag) scrub.el.classList.remove("active"); }, 1500);
}

window.addEventListener("scroll", () => {
  if (scrub.el.hidden) return;
  placeHandle();
  if (scrollY > 240 || scrub.drag) showScrubber();
  else if (!scrub.drag) scrub.el.classList.remove("active");
}, { passive: true });
window.addEventListener("resize", () => { if (!scrub.el.hidden) measureHeads(); });

scrubHandle.addEventListener("pointerdown", (e) => {
  e.preventDefault();
  measureHeads();
  scrub.drag = { y0: e.clientY, top0: Math.min(1, scrollY / maxScroll()) * scrubTrack() };
  scrub.el.classList.add("dragging", "active");
  try { scrubHandle.setPointerCapture(e.pointerId); } catch { /* 指针已经抬起 */ }
});
scrubHandle.addEventListener("pointermove", (e) => {
  if (!scrub.drag) return;
  const top = Math.min(scrubTrack(), Math.max(0, scrub.drag.top0 + e.clientY - scrub.drag.y0));
  scrollTo(0, (top / Math.max(1, scrubTrack())) * maxScroll());
});
const scrubEnd = () => {
  if (!scrub.drag) return;
  scrub.drag = null;
  scrub.el.classList.remove("dragging");
  showScrubber();
};
scrubHandle.addEventListener("pointerup", scrubEnd);
scrubHandle.addEventListener("pointercancel", scrubEnd);

// ---------- 大图查看 ----------
// 当前目录的照片、视频排成一串：左右滑动（电脑上 ← → 或两侧箭头）切换；照片双指或双击放大，放大后拖动查看；
// 往下拖关闭；单击隐藏 / 显示顶栏；手机的返回手势也是关闭。只有停在某一张上才请求原图
// （算格子口令的“看了一张”），旁边的只用缩略图垫着，不算。

const vw = { items: [], index: 0, zoom: { s: 1, x: 0, y: 0 }, pushed: false, animating: false };
const vwDialog = $("#viewer");
const vwStage = $("#vw-stage");
const vwTrack = $("#vw-track");

function openViewer(f) {
  vw.items = state.files.filter(inGallery);
  vw.index = Math.max(0, vw.items.findIndex((x) => x.id === f.id));
  vw.animating = false;
  vwDialog.classList.remove("ui-hidden");
  vwDialog.style.setProperty("--vw-fade", "1");
  vwTrack.style.transform = "";
  vwTrack.replaceChildren(...[0, 1, 2].map(() => Object.assign(document.createElement("div"), { className: "vw-slide" })));
  if (!vwDialog.open) {
    vwDialog.showModal();
    try {
      history.pushState({ xiaojuViewer: true }, "");
      vw.pushed = true;
    } catch { vw.pushed = false; }
  }
  renderViewer();
}

function closeViewer() { closeDialog(vwDialog); }

vwDialog.addEventListener("close", () => {
  vwTrack.replaceChildren(); // 停止视频
  vw.items = [];
  if (vw.pushed) { // 关掉时把打开时压的那条历史记录退掉
    vw.pushed = false;
    history.back();
  }
});
vwDialog.addEventListener("cancel", (e) => { e.preventDefault(); closeViewer(); });
window.addEventListener("popstate", () => {
  if (!vwDialog.open) return;
  vw.pushed = false; // 返回手势已经退掉了
  closeViewer();
});

const curItem = () => vw.items[vw.index];
const curSlide = () => vwTrack.children[1];
const mediaEl = () => curSlide()?.querySelector(".vw-media");
const canZoom = () => !!curItem() && !curItem().mime.startsWith("video/");

function renderViewer() {
  const f = curItem();
  if (!f) { closeViewer(); return; }
  vw.zoom = { s: 1, x: 0, y: 0 };
  vwDialog.classList.remove("zoomed");
  [...vwTrack.children].forEach((slide, k) => fillSlide(slide, vw.items[vw.index + k - 1], k === 1));
  $("#vw-name").textContent = f.name;
  $("#vw-count").textContent = `${vw.index + 1} / ${vw.items.length}` + (f.taken_at ? ` · ${formatDate(f.taken_at)}` : "");
  $("#vw-prev").disabled = vw.index === 0;
  $("#vw-next").disabled = vw.index === vw.items.length - 1;
  vwDialog.classList.toggle("is-video", f.mime.startsWith("video/"));
}

function fillSlide(slide, f, current) {
  slide.replaceChildren();
  slide.dataset.id = f ? f.id : "";
  if (!f) return;
  const bg = document.createElement("div");
  bg.className = "vw-bg";
  if (f.thumb) bg.style.backgroundImage = `url("${state.api + f.thumb}")`;
  const media = document.createElement("div");
  media.className = "vw-media";
  if (f.thumb) media.append(Object.assign(document.createElement("img"), { className: "vw-small", src: state.api + f.thumb, alt: "", draggable: false }));
  slide.append(bg, media);
  if (current) showFull(slide, media, f);
}

const vwNote = (text) => Object.assign(document.createElement("p"), { className: "vw-note", textContent: text });

// 原图的签名链接：同一张只申请一次（只算一次“看了一张”），下载也用它
async function viewerURL(f) {
  if (f._url && Date.now() < f._urlUntil) return f._url;
  const url = await signedURL(f, false);
  Object.assign(f, { _url: url, _urlUntil: Date.now() + 5 * 3600e3 });
  return url;
}

async function showFull(slide, media, f) {
  const kind = previewKind(f.mime);
  if (kind !== "img" && kind !== "video") {
    slide.append(vwNote(f.mime.startsWith("video/")
      ? "这个浏览器播放不了这种视频，可以下载后查看"
      : "这个浏览器显示不了 HEIC 原图（iPhone、Mac 的 Safari 可以），可以下载后查看"));
    return;
  }
  const spin = Object.assign(document.createElement("div"), { className: "vw-spin" });
  slide.append(spin);
  const still = () => slide.isConnected && slide === curSlide() && slide.dataset.id === f.id;
  let url;
  try {
    url = await viewerURL(f);
  } catch (err) {
    if (err.code === "grid_cancelled") return; // 格子口令没过，已经退出登录
    spin.remove();
    if (still()) slide.append(vwNote(err.message));
    return;
  }
  if (!still()) return;
  const src = `${url}&inline=1`;
  if (kind === "video") {
    spin.remove();
    showVideo(slide, media, f, src);
    return;
  }
  const img = Object.assign(new Image(), { className: "vw-full", alt: f.name, draggable: false });
  img.decoding = "async";
  const ok = await new Promise((resolve) => {
    img.onload = () => resolve(true);
    img.onerror = () => resolve(false);
    img.src = src;
  });
  if (!still()) return;
  spin.remove();
  if (!ok) { slide.append(vwNote("原图加载失败，可以下载后查看")); return; }
  media.replaceChildren(img);
}

// 视频：不用浏览器自带的控件——iPhone 上它会接管视频上的所有手势（左右滑不动），没点播放前也什么都不显示。
// 自己画：中间一个播放键（垫着缩略图），底部播放 / 暂停、进度条、时间、静音、全屏。
// iPhone 不允许没点过就带声音自动播放：先试着播放，不行就等点播放键（点的那一下是允许的）。
const VW_ICON = {
  play: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M8 5.5v13l11-6.5z" fill="currentColor"/></svg>',
  pause: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M7 5h3.5v14H7zM13.5 5H17v14h-3.5z" fill="currentColor"/></svg>',
  sound: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M4 9.5h3.5L12 5.5v13l-4.5-4H4z"/><path d="M15.5 9a4 4 0 0 1 0 6M18 6.5a7.5 7.5 0 0 1 0 11"/></svg>',
  muted: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M4 9.5h3.5L12 5.5v13l-4.5-4H4z"/><path d="M16 9.5l5 5M21 9.5l-5 5"/></svg>',
  full: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M4 9V4h5M20 9V4h-5M4 15v5h5M20 15v5h-5"/></svg>',
};

const playClock = (sec) => {
  if (!Number.isFinite(sec) || sec < 0) return "0:00";
  const s = Math.floor(sec);
  const pad = (x) => String(x).padStart(2, "0");
  return s >= 3600 ? `${Math.floor(s / 3600)}:${pad(Math.floor((s % 3600) / 60))}:${pad(s % 60)}` : `${Math.floor(s / 60)}:${pad(s % 60)}`;
};

// MediaError 的编号 → 说明（MEDIA_ERR_NETWORK 2、DECODE 3、SRC_NOT_SUPPORTED 4）
function videoErrorText(v) {
  const code = v.error?.code;
  if (code === 2) return "视频加载中断了（网络不稳），往回滑一下再回来试试，或者下载后查看";
  if (code === 3 || code === 4) return `这个视频的编码这台设备放不了（错误 ${code}），可以点右上角下载后用相册打开`;
  return "视频加载失败，可以点右上角下载后查看";
}

function showVideo(slide, media, f, src) {
  const v = document.createElement("video");
  v.className = "vw-video loading";
  v.playsInline = true;
  v.setAttribute("playsinline", "");
  v.setAttribute("webkit-playsinline", "");
  v.preload = "metadata";
  const html = (markup) => { const t = document.createElement("template"); t.innerHTML = markup; return t.content.firstChild; };
  const big = mk("button", { type: "button", className: "vw-play", ariaLabel: "播放" }, html(VW_ICON.play));
  const toggle = mk("button", { type: "button", className: "vw-cbtn", ariaLabel: "播放" }, html(VW_ICON.play));
  const cur = mk("span", { className: "vw-time", textContent: "0:00" });
  const seek = mk("input", { type: "range", className: "vw-seek", min: 0, max: 1000, step: 1, value: 0, ariaLabel: "播放进度" });
  const total = mk("span", { className: "vw-time", textContent: f.duration ? playClock(f.duration) : "--:--" });
  const mute = mk("button", { type: "button", className: "vw-cbtn", ariaLabel: "静音" }, html(VW_ICON.sound));
  const full = mk("button", { type: "button", className: "vw-cbtn", ariaLabel: "全屏" }, html(VW_ICON.full));
  const controls = mk("div", { className: "vw-controls" }, toggle, cur, seek, total, mute, full);
  const spin = mk("div", { className: "vw-spin", hidden: true });
  let seeking = false;
  let failed = false;

  const setIcon = (btn, name, label) => { btn.replaceChildren(html(VW_ICON[name])); btn.ariaLabel = label; };
  const paint = () => {
    const playing = !v.paused && !v.ended;
    slide.classList.toggle("playing", playing);
    big.hidden = playing || failed;
    setIcon(toggle, playing ? "pause" : "play", playing ? "暂停" : "播放");
  };
  const fail = () => {
    if (failed) return;
    failed = true;
    spin.hidden = true;
    big.hidden = true;
    controls.hidden = true;
    slide.append(vwNote(videoErrorText(v)));
  };
  const start = () => {
    if (failed) return;
    if (v.ended) v.currentTime = 0;
    const p = v.play();
    if (p) p.catch((err) => { if (err.name !== "NotAllowedError" && err.name !== "AbortError") fail(); paint(); });
  };

  v.addEventListener("loadedmetadata", () => { total.textContent = playClock(v.duration); });
  v.addEventListener("playing", () => {
    spin.hidden = true;
    v.classList.remove("loading");
    media.querySelector(".vw-small")?.remove();
    paint();
  });
  v.addEventListener("waiting", () => { if (!v.paused) spin.hidden = false; });
  v.addEventListener("canplay", () => { spin.hidden = true; });
  v.addEventListener("play", () => { if (v.readyState < 3) spin.hidden = false; paint(); });
  v.addEventListener("pause", paint);
  v.addEventListener("ended", paint);
  v.addEventListener("timeupdate", () => {
    cur.textContent = playClock(v.currentTime);
    if (!seeking && v.duration) seek.value = String(Math.round((v.currentTime / v.duration) * 1000));
  });
  v.addEventListener("error", fail);
  big.addEventListener("click", start);
  toggle.addEventListener("click", () => (v.paused || v.ended ? start() : v.pause()));
  mute.addEventListener("click", () => {
    v.muted = !v.muted;
    setIcon(mute, v.muted ? "muted" : "sound", v.muted ? "取消静音" : "静音");
  });
  full.addEventListener("click", () => {
    if (v.webkitEnterFullscreen) v.webkitEnterFullscreen(); // iPhone：交给系统播放器全屏
    else v.requestFullscreen?.();
  });
  seek.addEventListener("input", () => {
    seeking = true;
    if (v.duration) {
      v.currentTime = (Number(seek.value) / 1000) * v.duration;
      cur.textContent = playClock(v.currentTime);
    }
  });
  seek.addEventListener("change", () => { seeking = false; });

  v.src = src;
  media.append(v);
  slide.append(spin, big, controls);
  paint();
  start(); // 电脑上多半能直接播；iPhone 上会被拒绝，停在播放键
}

// 切到上一张（-1）/ 下一张（1）
function go(delta) {
  if (vw.animating || !vwDialog.open) return;
  const target = vw.index + delta;
  if (target < 0 || target >= vw.items.length) { snapTrack(); return; }
  vw.animating = true;
  vwTrack.classList.add("anim");
  vwTrack.style.transform = `translateX(${-100 * (1 + delta)}%)`;
  let timer;
  const finish = (e) => {
    if (e && e.target !== vwTrack) return;
    vwTrack.removeEventListener("transitionend", finish);
    clearTimeout(timer);
    vwTrack.classList.remove("anim");
    vwTrack.style.transform = "";
    vw.index = target;
    vw.animating = false;
    renderViewer();
  };
  vwTrack.addEventListener("transitionend", finish);
  timer = setTimeout(finish, 450);
}

function snapTrack() {
  vwTrack.classList.add("anim");
  vwTrack.style.transform = "";
  setTimeout(() => vwTrack.classList.remove("anim"), 350);
}

// 缩放：.vw-media 以屏幕中心为原点 translate(x, y) scale(s)
function applyZoom(animate) {
  const m = mediaEl();
  if (!m) return;
  m.classList.toggle("anim", !!animate);
  const { s, x, y } = vw.zoom;
  m.style.transform = s === 1 && !x && !y ? "" : `translate(${x}px, ${y}px) scale(${s})`;
  vwDialog.classList.toggle("zoomed", s > 1.01);
}

// 放大后最多拖到照片边缘
function zoomBounds(s) {
  const pic = mediaEl()?.querySelector(".vw-full, .vw-small");
  const W = vwStage.clientWidth;
  const H = vwStage.clientHeight;
  return {
    x: Math.max(0, ((pic?.offsetWidth || W) * s - W) / 2),
    y: Math.max(0, ((pic?.offsetHeight || H) * s - H) / 2),
  };
}

const clampTo = (v, max) => Math.min(max, Math.max(-max, v));

// 松手后：缩小过头的弹回原大小，拖出边缘的弹回来
function settleZoom() {
  let { s, x, y } = vw.zoom;
  if (s <= 1.01) { s = 1; x = 0; y = 0; }
  if (s > 6) { x *= 6 / s; y *= 6 / s; s = 6; }
  const b = zoomBounds(s);
  vw.zoom = { s, x: clampTo(x, b.x), y: clampTo(y, b.y) };
  applyZoom(true);
}

function zoomAt(cx, cy, s) {
  const c = stageCenter();
  const { s: s0, x: x0, y: y0 } = vw.zoom;
  // 手指（鼠标）下的那一点保持不动
  const lx = (cx - c.x - x0) / s0;
  const ly = (cy - c.y - y0) / s0;
  vw.zoom = { s, x: cx - c.x - lx * s, y: cy - c.y - ly * s };
}

function stageCenter() {
  const r = vwStage.getBoundingClientRect();
  return { x: r.left + r.width / 2, y: r.top + r.height / 2 };
}

// 手势：一根手指 → 判断是左右切换、下滑关闭还是（放大时）拖动；两根手指 → 缩放；没怎么动 → 单击 / 双击
const ptrs = new Map();
let gesture = null;
let lastTap = null;
let tapTimer = 0;

vwStage.addEventListener("pointerdown", (e) => {
  if ((e.pointerType === "mouse" && e.button !== 0) || vw.animating) return;
  // 视频的播放键、进度条这些控件上的手势交给控件
  if (e.target.closest(".vw-controls, .vw-play")) return;
  ptrs.set(e.pointerId, { x: e.clientX, y: e.clientY });
  if (ptrs.size === 1) {
    gesture = { type: "pending", x0: e.clientX, y0: e.clientY, t0: performance.now(), zoom0: { ...vw.zoom } };
  } else if (ptrs.size === 2 && canZoom()) {
    const [a, b] = [...ptrs.values()];
    snapTrack();
    gesture = { type: "pinch", d0: Math.hypot(a.x - b.x, a.y - b.y) || 1, zoom0: { ...vw.zoom }, m0: { x: (a.x + b.x) / 2, y: (a.y + b.y) / 2 } };
  }
});

vwStage.addEventListener("pointermove", (e) => {
  if (!ptrs.has(e.pointerId) || !gesture) return;
  ptrs.set(e.pointerId, { x: e.clientX, y: e.clientY });
  const g = gesture;
  if (g.type === "pinch") {
    if (ptrs.size < 2) return;
    const [a, b] = [...ptrs.values()];
    const s = Math.min(8, Math.max(0.6, g.zoom0.s * Math.hypot(a.x - b.x, a.y - b.y) / g.d0));
    vw.zoom = g.zoom0;
    zoomAt(g.m0.x, g.m0.y, s); // 以开始时两指中点为中心缩放，再跟着中点平移
    vw.zoom.x += (a.x + b.x) / 2 - g.m0.x;
    vw.zoom.y += (a.y + b.y) / 2 - g.m0.y;
    applyZoom(false);
    return;
  }
  const dx = e.clientX - g.x0;
  const dy = e.clientY - g.y0;
  if (g.type === "pending") {
    if (Math.hypot(dx, dy) < 8) return;
    if (vw.zoom.s > 1.01) g.type = "pan";
    else if (Math.abs(dx) > Math.abs(dy)) g.type = "swipe";
    else g.type = dy > 0 ? "dismiss" : "none";
    if (g.type !== "none") {
      try { vwStage.setPointerCapture(e.pointerId); } catch { /* 指针已经抬起 */ }
    }
  }
  if (g.type === "pan") {
    const b = zoomBounds(vw.zoom.s);
    const rubber = (v, max) => (Math.abs(v) <= max ? v : Math.sign(v) * (max + (Math.abs(v) - max) / 3));
    vw.zoom = { s: vw.zoom.s, x: rubber(g.zoom0.x + dx, b.x), y: rubber(g.zoom0.y + dy, b.y) };
    applyZoom(false);
  } else if (g.type === "swipe") {
    const edge = (dx > 0 && vw.index === 0) || (dx < 0 && vw.index === vw.items.length - 1);
    vwTrack.style.transform = `translateX(calc(-100% + ${edge ? dx / 3 : dx}px))`;
  } else if (g.type === "dismiss") {
    const p = Math.min(1, Math.max(0, dy) / 400);
    const m = mediaEl();
    if (m) {
      m.classList.remove("anim");
      m.style.transform = `translate(${dx * 0.6}px, ${Math.max(0, dy)}px) scale(${1 - p * 0.3})`;
    }
    vwDialog.style.setProperty("--vw-fade", String(1 - p));
  }
});

function pointerEnd(e) {
  if (!ptrs.has(e.pointerId)) return;
  ptrs.delete(e.pointerId);
  const g = gesture;
  if (!g) return;
  if (g.type === "pinch") {
    if (ptrs.size) { // 还剩一根手指：接着拖动
      const [p] = [...ptrs.values()];
      gesture = { type: "pan", x0: p.x, y0: p.y, t0: performance.now(), zoom0: { ...vw.zoom } };
      return;
    }
    gesture = null;
    settleZoom();
    return;
  }
  if (ptrs.size) return;
  gesture = null;
  const dx = e.clientX - g.x0;
  const dy = e.clientY - g.y0;
  const dt = Math.max(1, performance.now() - g.t0);
  if (g.type === "pending") {
    if (e.type === "pointerup") tap(e);
  } else if (g.type === "pan") {
    settleZoom();
  } else if (g.type === "swipe") {
    const fast = Math.abs(dx) / dt > 0.5 && Math.abs(dx) > 30;
    if (Math.abs(dx) > vwStage.clientWidth * 0.2 || fast) go(dx < 0 ? 1 : -1);
    else snapTrack();
  } else if (g.type === "dismiss") {
    if (dy > 120 || (dy / dt > 0.6 && dy > 40)) { closeViewer(); return; }
    const m = mediaEl();
    if (m) { m.classList.add("anim"); m.style.transform = ""; }
    vwDialog.style.setProperty("--vw-fade", "1");
  }
}
vwStage.addEventListener("pointerup", pointerEnd);
vwStage.addEventListener("pointercancel", pointerEnd);

function tap(e) {
  const now = performance.now();
  if (lastTap && now - lastTap.t < 300 && Math.hypot(e.clientX - lastTap.x, e.clientY - lastTap.y) < 30) {
    clearTimeout(tapTimer);
    lastTap = null;
    if (!canZoom()) return;
    if (vw.zoom.s > 1.01) vw.zoom = { s: 1, x: 0, y: 0 };
    else zoomAt(e.clientX, e.clientY, 2.5);
    settleZoom();
    return;
  }
  lastTap = { t: now, x: e.clientX, y: e.clientY };
  clearTimeout(tapTimer);
  tapTimer = setTimeout(() => {
    lastTap = null;
    vwDialog.classList.toggle("ui-hidden");
  }, 300);
}

// 电脑：滚轮 / 触控板缩放
vwStage.addEventListener("wheel", (e) => {
  if (!canZoom()) return;
  e.preventDefault();
  zoomAt(e.clientX, e.clientY, Math.min(6, Math.max(1, vw.zoom.s * Math.exp(-e.deltaY * (e.ctrlKey ? 0.01 : 0.002)))));
  settleZoom();
}, { passive: false });
vwDialog.addEventListener("gesturestart", (e) => e.preventDefault()); // Safari 自己的双指缩放整页
window.addEventListener("resize", () => { if (vwDialog.open && vw.zoom.s !== 1) settleZoom(); });

vwDialog.addEventListener("keydown", (e) => {
  if (e.key === "ArrowLeft") { e.preventDefault(); go(-1); }
  if (e.key === "ArrowRight") { e.preventDefault(); go(1); }
});
$("#vw-close").addEventListener("click", closeViewer);
$("#vw-prev").addEventListener("click", () => go(-1));
$("#vw-next").addEventListener("click", () => go(1));
$("#vw-download").addEventListener("click", async () => {
  const f = curItem();
  if (!f) return;
  try {
    const a = Object.assign(document.createElement("a"), { href: await viewerURL(f), rel: "noopener" });
    vwDialog.append(a); // 弹窗打开时页面其他部分点不到，链接放在弹窗里
    a.click();
    a.remove();
  } catch (err) { fail(err); }
});
$("#vw-delete").addEventListener("click", async () => {
  const f = curItem();
  if (!f || !(await trashItems([f])).length) return;
  vw.items.splice(vw.index, 1);
  if (!vw.items.length) { closeViewer(); return; }
  vw.index = Math.min(vw.index, vw.items.length - 1);
  renderViewer();
});

// ---------- 顶栏菜单、＋、弹窗 ----------

fillIcons();
$("#menu-btn").append(icon("user"));

const menu = $("#menu");
function toggleMenu(open = menu.hidden) {
  menu.hidden = !open;
  $("#menu-btn").setAttribute("aria-expanded", String(open));
  if (open) playClass(menu, "pop", 200);
}
$("#menu-btn").addEventListener("click", (e) => { e.stopPropagation(); toggleMenu(); });
// 菜单开着时点别处：只关菜单，不触发底下的东西
document.addEventListener("click", (e) => {
  if (menu.hidden || menu.contains(e.target) || $("#menu-btn").contains(e.target)) return;
  e.stopPropagation();
  e.preventDefault();
  toggleMenu(false);
}, true);

const ACTIONS = {
  backup: () => openBackup(),
  trash: () => openTrash(),
  security: () => openSecurity(),
  shortcuts: () => openShortcuts(),
  logout: () => logout(),
};
menu.addEventListener("click", (e) => {
  const b = e.target.closest("button[data-act]");
  if (!b) return;
  toggleMenu(false);
  ACTIONS[b.dataset.act]?.();
});
$("#backup-dot").addEventListener("click", () => openBackup());

// 弹窗里的“关闭”“取消”，以及跳到别的弹窗的按钮
document.addEventListener("click", (e) => {
  const close = e.target.closest("dialog [data-close]");
  if (close && !close.onclick) { closeDialog(close.closest("dialog")); return; }
  const jump = e.target.closest("dialog button[data-act='shortcuts']");
  if (jump) closeDialog(jump.closest("dialog")).then(() => openShortcuts());
});

// 右下角的 ＋：上传照片和视频、上传文件、新建文件夹、快捷指令
const sheet = $("#sheet");
function openSheet() {
  sheet.hidden = false;
  playClass(sheet, "open", 300);
}
function closeSheet() {
  if (sheet.hidden) return;
  playClass(sheet, "closing", 200).then(() => { sheet.hidden = true; });
}
$("#fab").addEventListener("click", openSheet);
sheet.addEventListener("click", (e) => {
  if (e.target === sheet) { closeSheet(); return; }
  const b = e.target.closest("[data-act]");
  if (b?.dataset.act === "cancel") closeSheet();
  if (b?.dataset.act === "mkdir") { closeSheet(); makeFolder(); }
  if (b?.dataset.act === "shortcuts") { closeSheet(); openShortcuts(); }
});
// 选文件时收起（选择器是系统弹出的）
for (const sel of ["#file-input", "#album-input"]) {
  $(sel).addEventListener("click", () => setTimeout(closeSheet, 300));
  $(sel).addEventListener("change", closeSheet);
}

async function makeFolder() {
  const name = prompt("文件夹名称");
  if (!name || !name.trim()) return;
  try {
    await request("POST", "/api/folders", { name: name.trim(), parent_id: state.folderId });
    load();
  } catch (err) { fail(err); }
}

function openShortcuts() {
  const dlg = $("#shortcuts");
  if (!dlg.open) dlg.showModal();
  renderShortcutSec();
}

// ---------- 回收站 ----------

async function openTrash() {
  const dlg = $("#trash-dlg");
  if (!dlg.open) dlg.showModal();
  await renderTrash();
}

async function renderTrash() {
  const list = $("#trash-list");
  list.replaceChildren(mk("li", { className: "hint", textContent: "正在读取…" }));
  let items;
  try {
    items = (await request("GET", "/api/trash")).items || [];
  } catch (err) {
    list.replaceChildren(mk("li", { className: "error", textContent: err.message }));
    return;
  }
  $("#trash-empty").disabled = !items.length;
  if (!items.length) { list.replaceChildren(mk("li", { className: "empty", textContent: "回收站是空的" })); return; }
  list.replaceChildren(...items.map((f) => {
    const restore = mk("button", { type: "button", className: "small" }, icon("restore"), "恢复");
    const purge = mk("button", { type: "button", className: "small danger" }, "永久删除");
    restore.addEventListener("click", async () => {
      try {
        const back = await request("POST", `/api/files/${f.id}/restore`);
        toast(back.parent_id === f.parent_id ? `已恢复“${f.name}”` : `已恢复“${f.name}”到根目录（原来的文件夹也删了）`);
        renderTrash();
        load();
        refreshBackup();
      } catch (err) {
        toast(err.status === 409 ? `原来的位置已经有“${f.name}”了，先把那个改个名再恢复` : err.message);
      }
    });
    purge.addEventListener("click", async () => {
      if (!confirm(`永久删除“${f.name}”？${f.type === "folder" ? "里面的文件也会一起删掉。" : ""}两个 Telegram 频道里的数据都会删除，无法恢复。`)) return;
      setButtonBusy(purge, true);
      try {
        await request("DELETE", `/api/files/${f.id}?permanent=1`);
        toast("已永久删除");
        renderTrash();
        refreshBackup();
      } catch (err) { setButtonBusy(purge, false); fail(err); }
    });
    const meta = `删除于 ${relTime(f.trashed_at)}` + (f.type === "file" ? ` · ${formatSize(f.size)}` : "");
    return mk("li", {}, icon(fileIcon(f), "ic lead"),
      mk("div", { className: "info" }, mk("b", { textContent: f.name }), mk("small", { textContent: meta })),
      mk("div", { className: "acts" }, restore, purge));
  }));
}

$("#trash-empty").addEventListener("click", async () => {
  const btn = $("#trash-empty");
  let items;
  try { items = (await request("GET", "/api/trash")).items || []; } catch (err) { fail(err); return; }
  if (!items.length || !confirm(`永久删除回收站里的 ${items.length} 项？两个 Telegram 频道里的数据都会删除，无法恢复。`)) return;
  btn.disabled = true;
  let done = 0;
  for (const f of items) {
    btn.textContent = `正在删除 ${done + 1} / ${items.length}…`;
    try { await request("DELETE", `/api/files/${f.id}?permanent=1`); done++; } catch (err) {
      if (err.status === 401) { fail(err); return; }
    }
  }
  btn.textContent = "清空回收站";
  toast(done === items.length ? "回收站已清空" : `删掉了 ${done} 项，${items.length - done} 项没删掉，稍后再试`);
  renderTrash();
  refreshBackup();
});

// ---------- 备份状态 ----------
// 顶栏的小圆点：绿色已双备份、橙色备份中、红色异常。点开看详情：两个 Telegram 账号的双备份，
// 以及手机上的照片备份到哪了（快捷指令每次运行会把清单报给服务端）。

let lastBackup = null; // 最近一次取到的 { st, stats }

function renderBackupDot(st, stats) {
  lastBackup = { st, stats };
  const el = $("#backup-dot");
  const label = el.querySelector(".label");
  el.className = "backup-dot";
  if (!st.enabled) {
    el.classList.add("bad");
    label.textContent = st.error ? "备份异常" : "未启用备份";
  } else if (st.backed_up >= st.parts) {
    el.classList.add("ok");
    label.textContent = "已双备份";
  } else {
    el.classList.add("busy");
    label.textContent = `备份中 ${backupPct(st)}%`;
  }
  el.hidden = false;
}

// 按字节算进度；没完成时最多显示 99%
const backupPct = (st) => (st.bytes > 0 ? Math.min(99, Math.floor((st.backed_up_bytes / st.bytes) * 100)) : 0);

function meterEl(pct) {
  const fill = mk("i");
  fill.style.width = `${pct}%`;
  return mk("span", { className: "meter" }, fill);
}

async function openBackup() {
  const dlg = $("#backup-dlg");
  const body = $("#backup-body");
  if (!dlg.open) {
    body.replaceChildren(mk("p", { className: "hint", textContent: "正在读取…" }));
    dlg.showModal();
  }
  const [st, stats, phone] = await Promise.all(["/api/backup/status", "/api/stats", "/api/backup/phone"]
    .map((p) => request("GET", p).catch(() => null)));
  if (st) renderBackupDot(st, stats);
  body.replaceChildren(telegramCard(st, stats), ...(phone?.albums || []).map(albumCard));
  if (!phone) body.append(mk("p", { className: "hint", textContent: "服务端还不支持手机备份记录（等服务端更新后再看）。" }));
}

function telegramCard(st, stats) {
  const card = mk("section", { className: "bk-card" });
  const head = mk("div", { className: "bk-head" }, icon("cloud"), mk("b", { textContent: "双备份" }));
  card.append(head);
  if (!st) { card.append(mk("p", { className: "hint", textContent: "读取不到备份状态。" })); return card; }
  if (!st.enabled) {
    head.append(mk("span", { className: "pill bad", textContent: st.error ? "异常" : "未启用" }));
    card.append(mk("p", { textContent: st.error ? `备份频道连接失败，新文件暂时只存了一份：${st.error}` : "服务端没有配置备份频道，文件只存了一份。" }));
    return card;
  }
  const files = stats ? `${stats.backed_up_files} / ${stats.files} 个文件` : `${st.backed_up} / ${st.parts} 个分块`;
  if (st.backed_up >= st.parts) {
    head.append(mk("span", { className: "pill ok", textContent: "已完成" }));
    card.append(mk("p", { textContent: `所有文件都在两个 Telegram 账号的频道里各存了一份（${files}，${formatSize(st.bytes)}）。一个账号出问题，另一个还在。` }));
  } else {
    const pct = backupPct(st);
    head.append(mk("span", { className: "pill busy", textContent: `${pct}%` }));
    card.append(meterEl(pct), mk("p", { textContent: `正在把新文件复制到备份账号：${files}（${formatSize(st.backed_up_bytes)} / ${formatSize(st.bytes)}）。后台自动进行，关掉页面也会继续。` }));
  }
  return card;
}

// 一个相册（“相册”“隐藏相册”）：网盘里有几个、最近什么时候传的，快捷指令最近一次清单
function albumCard(a) {
  const card = mk("section", { className: "bk-card" });
  card.append(mk("div", { className: "bk-head" }, icon(a.folder === "隐藏相册" ? "lock" : "image"), mk("b", { textContent: a.folder }),
    mk("span", { className: "bk-count", textContent: `${a.files} 个文件` })));
  card.append(mk("p", { className: "hint", textContent: a.last_upload_at ? `最近一次上传：${relTime(a.last_upload_at)}` : "还没有上传过" }));
  const byDate = a.checks.find((c) => c.by_date);
  const byFolder = a.checks.find((c) => !c.by_date);
  if (byDate) {
    const left = Math.max(0, byDate.todo - byDate.uploaded_since);
    const row = mk("div", { className: "bk-check" }, icon("bolt"), mk("div", {},
      mk("b", { textContent: `自动备份新照片 · ${relTime(byDate.checked_at)}` }),
      mk("small", { textContent: `看了手机上最新的 ${byDate.files} 张：` + (byDate.todo
        ? (left ? `${byDate.uploaded} 张已在网盘，要传 ${byDate.todo} 张，还剩约 ${left} 张` : `新传了 ${byDate.todo} 张，都备份好了 ✓`)
        : "都已经在网盘里 ✓") })));
    card.append(row);
  }
  if (byFolder) {
    const done = Math.min(byFolder.files, byFolder.uploaded + byFolder.uploaded_since);
    const pct = byFolder.files ? Math.floor((done / byFolder.files) * 100) : 100;
    const finished = done >= byFolder.files;
    card.append(mk("div", { className: "bk-check" }, icon("folder"), mk("div", {},
      mk("b", { textContent: `备份照片 · ${relTime(byFolder.checked_at)}` }),
      mk("small", { textContent: finished
        ? `「文件」里那个文件夹的 ${byFolder.files} 个都传好了 ✓`
        : `「文件」里那个文件夹有 ${byFolder.files} 个，已传 ${done} 个（${pct}%）。没传完的再运行一次「备份照片」接着传` }),
      finished ? "" : meterEl(pct))));
  }
  if (!a.checks.length) card.append(mk("p", { className: "hint", textContent: "还没有用快捷指令传过。" }));
  return card;
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
  $("#upload-close").hidden = true;
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
  $("#upload-summary").textContent = (s.sync ? "上传照片：" : "上传：") + parts.join(" · ");
  $("#uploads").hidden = false;
}

function finishBatch() {
  const s = stats;
  stats = null;
  if (!s) return;
  let msg = `上传完成：新传 ${s.uploaded} 个`;
  if (s.skipped) msg += `，跳过 ${s.skipped} 个（已存在）`;
  if (s.failed) msg += `，失败 ${s.failed} 个（见上方列表）`;
  toast(msg);
  $("#upload-summary").textContent = msg;
  keepAwake(false);
  refreshBackup();
  // 上传照片：一次选太多 iPhone 准备得很慢，分批选更顺；已传过的会自动跳过
  if (s.sync) { $("#upload-more").hidden = false; $("#upload-close").hidden = false; return; }
  if (!s.failed) setTimeout(() => { if (!stats) $("#uploads").hidden = true; }, 4000);
}

$("#upload-more").addEventListener("click", () => $("#album-input").click());
$("#upload-close").addEventListener("click", () => { if (!active && !queue.length) $("#uploads").hidden = true; });

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

// ---------- 上传照片和视频 ----------

// 去掉自动加的序号：“IMG_1 (2).jpg” → “IMG_1.jpg”
const baseName = (n) => n.replace(/ \(\d+\)(?=\.[^.]*$|$)/, "");
const withSuffix = (n, i) => n.replace(/(\.[^.]*)?$/, (ext) => ` (${i})${ext}`);

// 把选中的照片 / 视频传进当前文件夹（在根目录时传进“相册”文件夹）。已传过的（文件名 + 大小相同）自动跳过，
// 所以每次多选一些也不会重复上传；同名但内容不同的照片自动改名为“名字 (2)”。
async function uploadMedia(files) {
  if (!files.length) return;
  toast(`${pickedNote(files)}，正在检查哪些已经传过…`);
  try {
    let target = state.folderId;
    if (!target) {
      const root = await request("GET", "/api/folders/root/children");
      const album = root.items.find((f) => f.type === "folder" && f.name === ALBUM)
        ?? await request("POST", "/api/folders", { name: ALBUM, parent_id: null });
      target = album.id;
    }
    const { items } = await request("GET", `/api/folders/${target}/children`);
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
      toast(`没有新的：选中的 ${skipped} 个都已经传过了`);
      return;
    }
    if (state.folderId !== target) location.hash = `#/f/${target}`;
    enqueue(todo, target, true);
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
  uploadMedia(files);
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
  const busy = active || queue.length || gridGatePending || state.selecting || document.querySelector("dialog[open]");
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

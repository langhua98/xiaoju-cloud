// 小桔云盘的分享页：share.html#<token>[/<子文件夹 ID>]。不用登录，看别人分享的一个文件或一个文件夹
// （GET /api/s/{token}，见 internal/api/shares.go）。设了密码的先输密码，换来的 key 存在当前标签页里。
// 照片显示服务端缩小的预览（HEIC 也能看），视频直接播放，都能下载。

const API = (window.XIAOJU_API_URL || "").replace(/\/+$/, "");
const $ = (sel) => document.querySelector(sel);
const route = () => {
  const [token = "", folder = ""] = location.hash.replace(/^#/, "").split("/");
  return { token, folder };
};
const keyName = (token) => `xiaoju.share.${token}`;
let items = []; // 当前文件夹里的文件（不含子文件夹）
let viewing = -1; // 大图看的是 items 里的第几个

function mk(tag, props = {}, ...children) {
  const el = Object.assign(document.createElement(tag), props);
  el.append(...children.filter((c) => c !== null && c !== undefined && c !== ""));
  return el;
}

function getKey(token) {
  try { return sessionStorage.getItem(keyName(token)) || ""; } catch { return ""; }
}

function setKey(token, key) {
  try { sessionStorage.setItem(keyName(token), key); } catch { /* 隐私模式：只在这次打开有效 */ }
}

async function api(method, path, body) {
  const { token } = route();
  const headers = {};
  const key = getKey(token);
  if (key) headers["X-Share-Key"] = key;
  if (body) headers["Content-Type"] = "application/json";
  let resp;
  try {
    resp = await fetch(API + path, { method, headers, body: body ? JSON.stringify(body) : undefined });
  } catch {
    throw Object.assign(new Error("连不上网盘，稍后再试"), { status: 0 });
  }
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) throw Object.assign(new Error(data.error || `请求失败（${resp.status}）`), { status: resp.status, code: data.code });
  return data;
}

function formatSize(n) {
  if (n < 1024) return `${n} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let v = n;
  let i = -1;
  do { v /= 1024; i++; } while (v >= 1024 && i < units.length - 1);
  return `${v.toFixed(v < 10 ? 1 : 0)} ${units[i]}`;
}

const formatDate = (s) => new Date(s).toLocaleDateString("zh-CN", { year: "numeric", month: "long", day: "numeric" });
const isMedia = (f) => f.type === "file" && /^(image|video)\//.test(f.mime || "");
const isVideo = (f) => /^video\//.test(f.mime || "");

function countsText(c) {
  const parts = [];
  if (c.photos) parts.push(`${c.photos} 张照片`);
  if (c.videos) parts.push(`${c.videos} 个视频`);
  if (c.others) parts.push(`${c.others} 个文件`);
  return parts.join(" · ") || "空文件夹";
}

// 实况照片（同名的一张照片和一段几秒的 MOV）只显示照片，和网盘里一样
function hideLiveVideos(files) {
  const base = (n) => n.replace(/\.[^.]*$/, "").toLowerCase();
  const photos = new Set(files.filter((f) => /^image\//.test(f.mime || "")).map((f) => base(f.name)));
  return files.filter((f) => !(f.mime === "video/quicktime" && !(f.duration > 4) && photos.has(base(f.name))));
}

function message(text) {
  const el = $("#sh-msg");
  el.textContent = text;
  el.hidden = !text;
}

function thumb(url, onFail) {
  const img = mk("img", { className: "thumb", alt: "", decoding: "async", loading: "lazy", draggable: false });
  img.onload = () => img.classList.add("loaded");
  img.onerror = onFail;
  img.src = API + url;
  return img;
}

async function load() {
  const { token, folder } = route();
  if (!token) { $("#sh-title").textContent = "链接不完整"; message("请检查链接有没有复制全。"); return; }
  if (!API) { $("#sh-title").textContent = "打不开"; message("网页没有配置网盘地址。"); return; }
  document.body.classList.add("loading");
  let data;
  try {
    data = await api("GET", `/api/s/${token}${folder ? `?folder=${folder}` : ""}`);
  } catch (err) {
    document.body.classList.remove("loading");
    render(null);
    if (err.code === "share_password") { askPassword(); return; }
    $("#sh-title").textContent = err.code === "share_expired" ? "链接已过期" : err.status === 404 ? "链接已失效" : "打不开";
    message(err.code === "share_expired" ? "请让分享的人重新发一个链接。"
      : err.status === 404 ? "分享已经被撤销，或者分享的东西被删除了。" : err.message);
    return;
  }
  document.body.classList.remove("loading");
  $("#sh-pass").hidden = true;
  render(data);
}

function render(data) {
  for (const id of ["#sh-albums", "#sh-grid", "#sh-files", "#sh-crumbs"]) $(id).hidden = true;
  message("");
  if (!data) return;
  const { token } = route();
  const share = data.share;
  const here = data.folder || null;
  $("#sh-title").textContent = here ? here.name : share.name;
  document.title = `${here ? here.name : share.name} · 小桔云盘分享`;

  const crumbs = data.crumbs || [];
  if (crumbs.length > 1) {
    const nav = $("#sh-crumbs");
    nav.replaceChildren(...crumbs.flatMap((c, i) => {
      const last = i === crumbs.length - 1;
      const el = last ? mk("span", { textContent: c.name }) : mk("a", { href: `#${token}${i ? `/${c.id}` : ""}`, textContent: c.name });
      return i ? [mk("span", { className: "sep", textContent: "/" }), el] : [el];
    }));
    nav.hidden = false;
  }

  const all = data.items || [];
  const folders = all.filter((f) => f.type === "folder");
  items = hideLiveVideos(all.filter((f) => f.type === "file"));
  const media = items.filter(isMedia);
  const others = items.filter((f) => !isMedia(f));
  const photos = media.filter((f) => !isVideo(f)).length;
  const meta = [];
  if (photos) meta.push(`${photos} 张照片`);
  if (media.length - photos) meta.push(`${media.length - photos} 个视频`);
  if (others.length) meta.push(`${others.length} 个文件`);
  if (folders.length) meta.push(`${folders.length} 个文件夹`);
  meta.push(share.expires_at ? `${formatDate(share.expires_at)}前有效` : "长期有效");
  $("#sh-meta").textContent = meta.join(" · ");
  if (!all.length) message("这个文件夹是空的。");

  if (folders.length) {
    $("#sh-albums").replaceChildren(...folders.map((f) => {
      const cover = mk("span", { className: "cover" });
      const placeholder = mk("span", { className: "ph", textContent: "📁" });
      if (f.cover) cover.append(thumb(f.cover, () => cover.replaceChildren(placeholder)));
      else cover.append(placeholder);
      const card = mk("a", { className: "album", href: `#${token}/${f.id}` }, cover,
        mk("span", { className: "aname", textContent: f.name }),
        mk("span", { className: "acount", textContent: f.counts ? countsText(f.counts) : "" }));
      return card;
    }));
    $("#sh-albums").hidden = false;
  }
  if (media.length) {
    $("#sh-grid").replaceChildren(...media.map((f) => {
      const tile = mk("button", { type: "button", className: "tile media", title: f.name });
      if (f.thumb) tile.append(thumb(f.thumb, () => tile.replaceChildren(mk("span", { className: "tname", textContent: f.name }))));
      else tile.append(mk("span", { className: "tname", textContent: f.name }));
      if (isVideo(f)) tile.append(mk("span", { className: "badge", textContent: "▶" }));
      tile.addEventListener("click", () => openViewer(items.indexOf(f)));
      return tile;
    }));
    $("#sh-grid").hidden = false;
  }
  if (others.length) {
    $("#sh-files").replaceChildren(...others.map((f) => mk("li", {},
      mk("div", { className: "info" }, mk("b", { textContent: f.name }), mk("small", { textContent: `${formatSize(f.size)} · ${formatDate(f.updated_at)}` })),
      mk("a", { className: "small", href: API + f.url, download: f.name, textContent: "下载" }))));
    $("#sh-files").hidden = false;
  }
}

function askPassword() {
  $("#sh-title").textContent = "需要密码";
  $("#sh-meta").textContent = "";
  $("#sh-pass").hidden = false;
  $("#sh-password").focus();
}

$("#sh-pass").addEventListener("submit", async (e) => {
  e.preventDefault();
  const { token } = route();
  const err = $("#sh-pass-error");
  err.hidden = true;
  try {
    const { key } = await api("POST", `/api/s/${token}/unlock`, { password: $("#sh-password").value });
    setKey(token, key);
    load();
  } catch (e2) {
    err.textContent = e2.code === "share_wrong_password" ? "密码不对" : e2.status === 429 ? "试太多次了，过一会儿再试" : e2.message;
    err.hidden = false;
  }
});

// ---------- 大图 ----------

const viewer = $("#sh-viewer");
const media = () => items.filter(isMedia);

function openViewer(i) {
  viewing = i;
  if (!viewer.open) viewer.showModal();
  showCurrent();
}

function showCurrent() {
  const f = items[viewing];
  if (!f) return;
  const list = media();
  $("#sh-vname").textContent = f.name;
  $("#sh-vcount").textContent = `${list.indexOf(f) + 1} / ${list.length}`;
  const dl = $("#sh-download");
  dl.href = API + f.url;
  dl.download = f.name;
  const stage = $("#sh-stage");
  stage.replaceChildren(mk("div", { className: "vw-spin" }));
  if (isVideo(f)) {
    const v = mk("video", { controls: true, playsInline: true, preload: "metadata", src: `${API}${f.url}&inline=1` });
    v.setAttribute("playsinline", "");
    stage.replaceChildren(v);
    return;
  }
  const img = mk("img", { alt: f.name, decoding: "async", draggable: false });
  img.onload = () => { if (items[viewing] === f) stage.replaceChildren(img); };
  img.onerror = () => {
    if (img.dataset.fallback || !f.preview) { stage.replaceChildren(mk("p", { className: "sh-msg", textContent: "这张显示不了，可以下载后查看" })); return; }
    img.dataset.fallback = "1";
    img.src = `${API}${f.url}&inline=1`; // 没有预览（服务端做不了）：看原图
  };
  img.src = API + (f.preview || `${f.url}&inline=1`);
}

function step(d) {
  const list = media();
  const at = list.indexOf(items[viewing]);
  const next = list[at + d];
  if (!next) return;
  viewing = items.indexOf(next);
  showCurrent();
}

$("#sh-close").addEventListener("click", () => viewer.close());
$("#sh-prev").addEventListener("click", () => step(-1));
$("#sh-next").addEventListener("click", () => step(1));
viewer.addEventListener("close", () => $("#sh-stage").replaceChildren());
viewer.addEventListener("keydown", (e) => {
  if (e.key === "ArrowLeft") step(-1);
  if (e.key === "ArrowRight") step(1);
});
let touch = null;
$("#sh-stage").addEventListener("touchstart", (e) => { touch = e.touches.length === 1 ? { x: e.touches[0].clientX, y: e.touches[0].clientY } : null; }, { passive: true });
$("#sh-stage").addEventListener("touchend", (e) => {
  if (!touch) return;
  const t = e.changedTouches[0];
  const dx = t.clientX - touch.x;
  const dy = t.clientY - touch.y;
  touch = null;
  if (Math.abs(dx) > 50 && Math.abs(dx) > Math.abs(dy) * 1.5) step(dx < 0 ? 1 : -1);
  else if (dy > 90 && Math.abs(dy) > Math.abs(dx)) viewer.close(); // 往下滑关闭
});

window.addEventListener("hashchange", () => { if (viewer.open) viewer.close(); load(); });
load();

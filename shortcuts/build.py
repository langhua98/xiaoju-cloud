#!/usr/bin/env python3
"""编译并签名“备份照片到小桔云盘”快捷指令。

    python3 shortcuts/build.py --out web/xiaoju-backup.shortcut [--api https://xxx.hf.space] [--skip-sign]

1. 用 Cherri 编译 backup.cherri（默认上传地址取 --api，或 web/config.js 里的 XIAOJU_API_URL）；
2. 修正导入问题指向的操作（Cherri 1.3.2 会把两个问题都指向第 0 个操作）；
3. iOS 15 起快捷指令文件必须签名才能导入：交给 RoutineHub 的 HubSign 服务签名（Cherri 在非 macOS 上也用它）。
   文件里只有默认上传地址，不含任何密钥（密钥在安装时由用户填写）。
"""
import argparse
import json
import pathlib
import plistlib
import re
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

HERE = pathlib.Path(__file__).resolve().parent
ROOT = HERE.parent
NAME = "备份照片到小桔云盘"
HUBSIGN = "https://hubsign.routinehub.services/sign"


def api_url(arg):
    if arg:
        return arg.rstrip("/")
    m = re.search(r'XIAOJU_API_URL\s*=\s*"([^"]*)"', (ROOT / "web" / "config.js").read_text())
    if not m or not m.group(1):
        sys.exit("no API URL: pass --api or set XIAOJU_API_URL in web/config.js")
    return m.group(1).rstrip("/")


def compile_cherri(cherri, source):
    with tempfile.TemporaryDirectory() as d:
        src = pathlib.Path(d) / "backup.cherri"
        src.write_text(source)
        subprocess.run([cherri, str(src), "--skip-sign", "--no-ansi"], check=True)
        out = list(pathlib.Path(d).glob("*.shortcut"))
        if len(out) != 1:
            sys.exit(f"cherri produced {len(out)} shortcut files")
        return plistlib.loads(out[0].read_bytes())


def fix_questions(wf):
    """把每个导入问题指向对应的“文本”操作（问题绑定的文本操作内容为空，按出现顺序一一对应）。"""
    texts = [i for i, a in enumerate(wf["WFWorkflowActions"])
             if a["WFWorkflowActionIdentifier"] == "is.workflow.actions.gettext"
             and a["WFWorkflowActionParameters"].get("WFTextActionText") == ""]
    questions = wf.get("WFWorkflowImportQuestions", [])
    if len(texts) < len(questions):
        sys.exit(f"{len(questions)} import questions but only {len(texts)} empty text actions")
    for q, i in zip(questions, texts):
        q["ActionIndex"] = i


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    # urllib 跟随 302 时会把 POST 改成不带请求体的 GET，签名就失败了：不跟随，直接报出跳转地址
    def redirect_request(self, *args, **kwargs):
        return None


def sign(xml):
    body = json.dumps({"shortcutName": NAME, "shortcut": xml}).encode()
    # 请求头和 Cherri 自带的 Go 客户端一致（有的 CDN 会拦截 Python 默认的 User-Agent）
    req = urllib.request.Request(HUBSIGN, data=body, method="POST", headers={
        "Content-Type": "application/json",
        "User-Agent": "Go-http-client/1.1",
    })
    for attempt in range(4):
        try:
            with urllib.request.build_opener(_NoRedirect).open(req, timeout=120) as resp:
                signed = resp.read()
            break
        except urllib.error.HTTPError as e:
            detail = e.read()[:300]
            # 限流或服务端临时出错：按 Retry-After（默认 30、60、90 秒）等一会儿再试
            if (e.code == 429 or e.code >= 500) and attempt < 3:
                wait = int(e.headers.get("Retry-After") or 30 * (attempt + 1))
                print(f"signing service: HTTP {e.code}, retrying in {wait}s", file=sys.stderr)
                time.sleep(min(wait, 120))
                continue
            sys.exit(f"signing service: HTTP {e.code}, location={e.headers.get('Location')!r}, "
                     f"server={e.headers.get('Server')!r}, body={detail!r}")
    if not signed.startswith(b"AEA1"):  # 签名后的快捷指令是 Apple Encrypted Archive
        sys.exit(f"signing service did not return a signed shortcut: {signed[:200]!r}")
    return signed


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--out", required=True)
    p.add_argument("--api")
    p.add_argument("--cherri", default="cherri")
    p.add_argument("--skip-sign", action="store_true", help="只编译（输出未签名的 XML，iOS 不能直接导入）")
    a = p.parse_args()

    source = (HERE / "backup.cherri").read_text().replace("__INGEST_URL__", api_url(a.api) + "/api/ingest")
    wf = compile_cherri(a.cherri, source)
    fix_questions(wf)
    xml = plistlib.dumps(wf, fmt=plistlib.FMT_XML).decode()
    data = xml.encode() if a.skip_sign else sign(xml)
    pathlib.Path(a.out).write_bytes(data)
    print(f"wrote {a.out} ({len(data)} bytes, {'unsigned' if a.skip_sign else 'signed'})")


if __name__ == "__main__":
    main()

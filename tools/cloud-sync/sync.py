#!/usr/bin/env python3
"""把小橘云盘里指定文件夹的改动推送到 GitHub。

每条映射：云盘文件夹 -> 仓库 / 分支 / 仓库里的目录。
只推送有变化的文件，每个仓库分支一次提交；云盘删掉的文件不会在 GitHub 删除；没变化就不提交。

判断有没有变化（从便宜到贵）：
  1. 云盘给每个文件一个 sha256。和上次推送时记下的一样、仓库里那个文件也没被别人改过，就跳过，不下载。
  2. 否则下载文件，在本地算 git blob sha，和仓库里的一样也跳过（只更新记录）。
  3. 不一样才上传并放进提交。

只用 Python 标准库。环境变量：
  XIAOJU_TOKEN    小橘云盘 agent 令牌（xa_…，只要读权限）
  GH_PUSH_TOKEN   能写目标仓库的 GitHub 令牌
  XIAOJU_URL      可选，默认 https://langhua1998-xiaoju-cloud.hf.space
用法：sync.py 配置.json [状态.json] [--dry-run]
"""
import base64
import hashlib
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

GITHUB = "https://api.github.com"
DEFAULT_CLOUD = "https://langhua1998-xiaoju-cloud.hf.space"
MAX_FILE_BYTES = 50 * 1024 * 1024
SKIP_NAMES = {".git", "node_modules", ".DS_Store"}


class SyncError(Exception):
    pass


def http(method, url, headers, body=None, timeout=60, allow404=False):
    data = None
    if body is not None:
        data = json.dumps(body).encode()
        headers = {**headers, "Content-Type": "application/json"}
    for attempt in range(4):
        req = urllib.request.Request(url, data=data, method=method, headers=headers)
        try:
            with urllib.request.urlopen(req, timeout=timeout) as res:
                return res.status, res.read()
        except urllib.error.HTTPError as e:
            text = e.read()
            if allow404 and e.code == 404:
                return 404, text
            # 云盘在 HF 上可能在睡觉（502/503），GitHub 偶尔 5xx，等一下再试
            if e.code >= 500 and attempt < 3:
                time.sleep(2 ** attempt * 5)
                continue
            raise SyncError(f"{method} {url.split('?')[0]} -> {e.code}: {text[:300].decode(errors='replace')}")
        except (urllib.error.URLError, TimeoutError) as e:
            if attempt < 3:
                time.sleep(2 ** attempt * 5)
                continue
            raise SyncError(f"{method} {url.split('?')[0]} 失败: {e}")


def clean(p):
    return [s.strip() for s in (p or "").split("/") if s.strip() and s.strip() not in (".", "..")]


def git_blob_sha(content):
    return hashlib.sha1(b"blob %d\0" % len(content) + content).hexdigest()


class Cloud:
    def __init__(self, base, token):
        self.base = base.rstrip("/")
        self.h = {"Authorization": f"Bearer {token}", "Accept": "application/json"}

    def list_folder(self, path):
        children, cursor = [], None
        while True:
            q = {"path": path, "limit": "1000"}
            if cursor:
                q["cursor"] = cursor
            _, b = http("GET", f"{self.base}/api/agent/v1/items?{urllib.parse.urlencode(q)}", self.h)
            j = json.loads(b)
            if j.get("item", {}).get("type") not in (None, "folder"):
                raise SyncError(f"云盘 {path} 不是文件夹")
            children += j.get("children") or []
            cursor = j.get("next_cursor")
            if not cursor:
                return children

    def walk(self, root):
        """返回 [(相对路径, item)]，包括子文件夹里的文件。"""
        out, stack = [], [("", "/" + "/".join(clean(root)))]
        while stack:
            rel, path = stack.pop()
            for c in self.list_folder(path):
                if c["name"] in SKIP_NAMES:
                    continue
                r = f"{rel}/{c['name']}" if rel else c["name"]
                if c["type"] == "folder":
                    stack.append((r, f"{path.rstrip('/')}/{c['name']}"))
                else:
                    out.append((r, c))
        return out

    def download(self, item):
        _, b = http("GET", f"{self.base}/api/agent/v1/items/{item['id']}/content",
                    {**self.h, "Accept": "*/*"}, timeout=300)
        return b


class GitHub:
    def __init__(self, token):
        self.h = {"Authorization": f"Bearer {token}", "Accept": "application/vnd.github+json",
                  "X-GitHub-Api-Version": "2022-11-28", "User-Agent": "xiaoju-cloud-sync"}

    def call(self, method, path, body=None, allow404=False, timeout=60):
        code, b = http(method, GITHUB + path, self.h, body, timeout, allow404)
        return None if code == 404 else json.loads(b or b"null")

    def default_branch(self, repo):
        r = self.call("GET", f"/repos/{repo}")
        return r["default_branch"]


def sync_target(cloud, gh, repo, branch, mappings, state, dry):
    """一个仓库分支：收集所有映射的文件，一次提交。返回推送的文件数。"""
    ref = gh.call("GET", f"/repos/{repo}/git/ref/heads/{urllib.parse.quote(branch)}", allow404=True)
    if ref is None:
        raise SyncError(f"{repo} 没有分支 {branch}")
    head = ref["object"]["sha"]
    base_tree = gh.call("GET", f"/repos/{repo}/git/commits/{head}")["tree"]["sha"]
    tree = gh.call("GET", f"/repos/{repo}/git/trees/{base_tree}?recursive=1")
    if tree.get("truncated"):
        print(f"  注意：{repo} 文件太多，目录树不完整，部分文件会多下载一次比较")
    in_repo = {e["path"]: e["sha"] for e in tree["tree"] if e["type"] == "blob"}

    entries, record, pushed = [], {}, []
    for m in mappings:
        prefix = "/".join(clean(m.get("path", "")))
        files = cloud.walk(m["cloud"])
        print(f"  {m['cloud']} -> {repo}@{branch}:/{prefix}  ({len(files)} 个文件)")
        for rel, item in files:
            rp = f"{prefix}/{rel}" if prefix else rel
            key = f"{repo}@{branch}:{rp}"
            old = state.get(key) or {}
            cur = in_repo.get(rp)
            if item.get("sha256") and old.get("sha256") == item["sha256"] and cur and cur == old.get("blob"):
                continue
            if (item.get("size") or 0) > MAX_FILE_BYTES:
                print(f"  跳过 {rp}：超过 50MB")
                continue
            content = cloud.download(item)
            blob = git_blob_sha(content)
            if blob == cur:
                state[key] = {"sha256": item.get("sha256", ""), "blob": blob}
                continue
            pushed.append(rp)
            record[key] = {"sha256": item.get("sha256", ""), "blob": blob}
            if dry:
                continue
            b = gh.call("POST", f"/repos/{repo}/git/blobs",
                        {"content": base64.b64encode(content).decode(), "encoding": "base64"}, timeout=300)
            entries.append({"path": rp, "mode": "100644", "type": "blob", "sha": b["sha"]})

    if not pushed:
        print("  没有变化，不提交")
        return 0
    for p in pushed:
        print(f"  + {p}")
    if dry:
        return len(pushed)

    names = ", ".join(pushed[:3]) + (f" 等 {len(pushed)} 个文件" if len(pushed) > 3 else "")
    msg = f"从小橘云盘同步：{names}"
    for attempt in range(3):
        new_tree = gh.call("POST", f"/repos/{repo}/git/trees", {"base_tree": base_tree, "tree": entries})
        commit = gh.call("POST", f"/repos/{repo}/git/commits",
                         {"message": msg, "tree": new_tree["sha"], "parents": [head]})
        try:
            gh.call("PATCH", f"/repos/{repo}/git/refs/heads/{urllib.parse.quote(branch)}",
                    {"sha": commit["sha"], "force": False})
            break
        except SyncError as e:
            # 期间有人推了代码：在新的 HEAD 上重做一次（不强推，不覆盖别人的提交）
            if "422" not in str(e) or attempt == 2:
                raise
            head = gh.call("GET", f"/repos/{repo}/git/ref/heads/{urllib.parse.quote(branch)}")["object"]["sha"]
            base_tree = gh.call("GET", f"/repos/{repo}/git/commits/{head}")["tree"]["sha"]
    state.update(record)
    print(f"  已提交 {commit['sha'][:7]}")
    return len(pushed)


def main(argv):
    dry = "--dry-run" in argv
    args = [a for a in argv if a != "--dry-run"]
    if not args:
        print(__doc__)
        return 2
    cfg = json.load(open(args[0], encoding="utf-8"))
    state_file = args[1] if len(args) > 1 else ".cloud-sync-state.json"
    try:
        state = json.load(open(state_file, encoding="utf-8"))
    except (OSError, ValueError):
        state = {}

    ct, gt = os.environ.get("XIAOJU_TOKEN"), os.environ.get("GH_PUSH_TOKEN")
    if not ct or not gt:
        print("缺少 XIAOJU_TOKEN 或 GH_PUSH_TOKEN")
        return 2
    cloud = Cloud(os.environ.get("XIAOJU_URL") or cfg.get("cloud_url") or DEFAULT_CLOUD, ct)
    gh = GitHub(gt)

    groups = {}
    for m in cfg.get("mappings", []):
        if not m.get("cloud") or not m.get("repo"):
            print(f"跳过不完整的映射：{m}")
            continue
        branch = m.get("branch") or gh.default_branch(m["repo"])
        groups.setdefault((m["repo"], branch), []).append(m)

    total, failed = 0, 0
    for (repo, branch), ms in groups.items():
        print(f"== {repo}@{branch}")
        try:
            total += sync_target(cloud, gh, repo, branch, ms, state, dry)
        except SyncError as e:
            failed += 1
            print(f"  失败：{e}")
    if not dry:
        with open(state_file, "w", encoding="utf-8") as f:
            json.dump(state, f, ensure_ascii=False, indent=0, sort_keys=True)
    print(f"完成：推送 {total} 个文件，{failed} 个仓库失败")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))

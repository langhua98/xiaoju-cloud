"""把 xiaoju-cloud 部署到 Hugging Face Space（由 GitHub Actions 调用）。

步骤：创建私有 Docker Space → 写入 Space Secrets → 上传源码 → 等待构建 → 打印运行日志。
所有密钥都从环境变量（GitHub Actions Secrets）读取，不会写进仓库或日志。
"""

import json
import os
import shutil
import subprocess
import sys
import tempfile
import time

import requests
from huggingface_hub import HfApi

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
HERE = os.path.dirname(os.path.abspath(__file__))

SECRET_KEYS = [
    "XIAOJU_DATABASE_URL",
    "XIAOJU_ADMIN_TOKEN",
    "XIAOJU_MASTER_KEY",
    "TG_APP_ID",
    "TG_APP_HASH",
    "TG_BOT_TOKEN",
    "TG_CHANNEL_ID",
]
VARIABLE_KEYS = ["TG_TRANSPORT", "XIAOJU_PART_SIZE", "XIAOJU_CORS_ORIGINS"]

DONE_STAGES = {"RUNNING", "RUNTIME_ERROR", "BUILD_ERROR", "CONFIG_ERROR", "NO_APP_FILE", "PAUSED", "STOPPED"}


def log(msg):
    print(msg, flush=True)


def stage_source(dst):
    """导出 git 跟踪的源码，换上 Space 专用的 Dockerfile 和 README（带 Space 元数据）。"""
    archive = subprocess.run(["git", "archive", "HEAD"], cwd=ROOT, check=True, capture_output=True).stdout
    subprocess.run(["tar", "-x", "-C", dst], input=archive, check=True)
    shutil.copy(os.path.join(HERE, "Dockerfile"), os.path.join(dst, "Dockerfile"))
    shutil.copy(os.path.join(HERE, "README.md"), os.path.join(dst, "README.md"))
    shutil.rmtree(os.path.join(dst, ".github"), ignore_errors=True)


def space_host(repo_id):
    return repo_id.replace("/", "-").replace("_", "-").replace(".", "-").lower() + ".hf.space"


def wait_for_stage(api, repo_id, timeout=1500):
    deadline = time.time() + timeout
    last = None
    # 刚上传时 stage 可能还是旧的 RUNNING，先等它离开旧状态
    time.sleep(20)
    while time.time() < deadline:
        stage = api.get_space_runtime(repo_id).stage
        if stage != last:
            log(f"space stage: {stage}")
            last = stage
        if stage in DONE_STAGES:
            return stage
        time.sleep(15)
    return last


def print_logs(token, repo_id, kind, seconds):
    """读取 Space 的 build/run 日志（SSE 流），最多 seconds 秒。"""
    url = f"https://huggingface.co/api/spaces/{repo_id}/logs/{kind}"
    log(f"----- {kind} logs (up to {seconds}s) -----")
    deadline = time.time() + seconds
    try:
        with requests.get(url, headers={"Authorization": f"Bearer {token}"}, stream=True, timeout=(10, 30)) as r:
            r.raise_for_status()
            for raw in r.iter_lines(decode_unicode=True):
                if raw and raw.startswith("data:"):
                    try:
                        log(json.loads(raw[5:]).get("data", "").rstrip())
                    except ValueError:
                        log(raw[5:].strip())
                if time.time() > deadline:
                    break
    except requests.RequestException as e:
        log(f"(log stream ended: {e})")
    log(f"----- end of {kind} logs -----")


def main():
    token = os.environ.get("HF_TOKEN", "").strip()
    if not token:
        log("::notice::HF_TOKEN secret is not set; skipping Hugging Face deployment.")
        return 0

    api = HfApi(token=token)
    user = api.whoami()["name"]
    repo_id = os.environ.get("HF_SPACE_ID", "").strip() or f"{user}/xiaoju-cloud"
    log(f"deploying to space {repo_id} (https://{space_host(repo_id)})")

    # 网页前端（GitHub Pages）需要公开 Space：私有 Space 的网关会拦截浏览器的跨域预检请求。
    # 公开后 API 仍由 XIAOJU_ADMIN_TOKEN 保护。
    private = os.environ.get("HF_SPACE_PUBLIC", "").strip().lower() not in ("1", "true", "yes")
    api.create_repo(repo_id, repo_type="space", space_sdk="docker", private=private, exist_ok=True)
    api.update_repo_visibility(repo_id, private=private, repo_type="space")
    log(f"space visibility: {'private' if private else 'public'}")

    for key in SECRET_KEYS:
        value = os.environ.get(key, "").strip()
        if value:
            api.add_space_secret(repo_id, key, value)
            log(f"secret set: {key}")
        else:
            log(f"secret not provided: {key}")
    for key in VARIABLE_KEYS:
        value = os.environ.get(key, "").strip()
        if value:
            api.add_space_variable(repo_id, key, value)
            log(f"variable set: {key}={value}")

    with tempfile.TemporaryDirectory() as tmp:
        stage_source(tmp)
        sha = os.environ.get("GITHUB_SHA", "local")[:7]
        api.upload_folder(
            folder_path=tmp,
            repo_id=repo_id,
            repo_type="space",
            commit_message=f"Deploy xiaoju-cloud {sha}",
            delete_patterns="*",
        )
    log("source uploaded; waiting for build...")

    stage = wait_for_stage(api, repo_id)
    if stage in ("BUILD_ERROR", "CONFIG_ERROR", "NO_APP_FILE"):
        print_logs(token, repo_id, "build", 30)
        return 1
    # 运行日志里包含 Telegram 连接检查结果（最长 90 秒超时），多读一会儿
    print_logs(token, repo_id, "run", 150)
    log(f"final stage: {stage}; url: https://{space_host(repo_id)}")
    return 0 if stage == "RUNNING" else 1


if __name__ == "__main__":
    sys.exit(main())

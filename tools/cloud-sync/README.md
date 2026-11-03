# 小橘云盘 → GitHub 自动同步

GitHub Actions 每 10 分钟跑一次 `sync.py`：看 `config.json` 里列的云盘文件夹，有新文件或改过的文件就推送到对应仓库的目录。

- 只推有变化的文件，一个仓库分支一次提交；没变化不提交。
- 云盘删掉的文件，GitHub 上不删。
- 不强推；期间有人推了代码，就在最新的提交上重做。

## 设置

1. 仓库 Settings → Secrets and variables → Actions，加两个 secret：
   - `XIAOJU_TOKEN`：小橘云盘的 agent 令牌（`xa_…`，只读就够）
   - `GH_PUSH_TOKEN`：能写目标仓库的 GitHub 令牌（fine-grained，Contents: Read and write）
2. 在 `config.json` 写映射：

```json
{
  "mappings": [
    {"cloud": "/代码/my-app", "repo": "langhua98/my-app", "branch": "main", "path": ""},
    {"cloud": "/笔记", "repo": "langhua98/notes", "path": "docs"}
  ]
}
```

`cloud` 是云盘文件夹（相对令牌的根文件夹），`path` 是仓库里的目录（空 = 仓库根目录），`branch` 不写就用默认分支。

想马上跑一次：Actions → 「云盘同步到 GitHub」→ Run workflow（可以勾 dry_run 只看不推）。

本地跑：`XIAOJU_TOKEN=xa_… GH_PUSH_TOKEN=… python3 tools/cloud-sync/sync.py tools/cloud-sync/config.json --dry-run`

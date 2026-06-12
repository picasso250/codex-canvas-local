# AGENTS.md

- 不要向前兼容

## 关于并行执行

- Default to serial shell execution.
- Only clearly read-only inspection commands may run in parallel.
- Never run `git` state-changing commands in parallel.
- Never run file-writing, process-management, package-manager, or migration commands in parallel.
- If unsure whether a command is read-only, run it serially.

## 关于 Wrangler

- `wrangler whoami` 会超时，但这不代表权限有问题。
- 你拥有权限，并且其他 `wrangler` 子命令都能成功。

## 关于错误处理

- "Don’t fight errors! Whenever you encounter the same error twice, research the web and find 3-5 possible ways to fix it. Then choose the most efficient solution and implement it."

## 关于服务重启

- 重启本地 `codex-canvas-local` 服务时，必须使用 `scripts/restart.ps1`。
- 默认命令：`powershell -ExecutionPolicy Bypass -File .\scripts\restart.ps1`
- 不要手动执行零散的 `go build`、`Stop-Process`、`Start-Process` 来重启服务。
- 脚本会先检查 `/api/jobs` 中是否存在 `queued` 或 `running` 任务；存在活动任务时默认拒绝重启。
- 只有明确需要中断活动任务时，才允许使用：`powershell -ExecutionPolicy Bypass -File .\scripts\restart.ps1 -Force`
- 重启后必须确认首页健康检查成功，或至少确认 `http://127.0.0.1:8765/` 返回 `200`。

# 从 Telegram 频道置顶的每日备份恢复数据库，然后启动云盘。
#   restore.ps1 -Check   只下载、解密、检查备份，不写库（先跑这个）
#   restore.ps1          恢复进空库 xiaoju，成功后启用开机任务并启动云盘
#   restore.ps1 -File X  用本地的 .xjdb 备份文件，不从 Telegram 下载
# 库里已经有表时 xiaoju 会拒绝恢复，不会覆盖。
param(
    [string]$Root = 'C:\xiaoju',
    [switch]$Check,
    [string]$File = ''
)
. "$PSScriptRoot\common.ps1"
Assert-Admin

Stop-Xiaoju
$envs = Use-EnvFile
foreach ($k in @('XIAOJU_MASTER_KEY', 'TG_APP_ID', 'TG_APP_HASH', 'TG_BOT_TOKEN', 'TG_CHANNEL_ID')) {
    if (-not $envs[$k]) { throw "xiaoju.env 里没有 $k，先运行 import-secrets.ps1。" }
}
Set-Location $Root

$cmdArgs = @('dbrestore')
if ($Check) { $cmdArgs += '--check' }
if ($File) { $cmdArgs += $File }
Step "xiaoju $($cmdArgs -join ' ')"
$ErrorActionPreference = 'Continue'   # xiaoju 的日志写在 stderr，别当成出错
& $Exe @cmdArgs 2>&1 | ForEach-Object { "$_" }
$code = $LASTEXITCODE
$ErrorActionPreference = 'Stop'
if ($code -ne 0) { throw "dbrestore 失败（退出码 $code），看上面的输出。" }

if ($Check) {
    Write-Host '备份完好。确认没问题就去掉 -Check 再跑一次，正式恢复。' -ForegroundColor Green
    return
}
Write-Host '恢复完成，启动云盘…' -ForegroundColor Green
Start-Xiaoju
foreach ($i in 1..60) {
    Start-Sleep -Seconds 2
    if (Test-Health) { Write-Host '云盘已启动：http://127.0.0.1:8080/healthz' -ForegroundColor Green; break }
}
if (-not (Test-Health)) { Write-Warning "两分钟了还没起来，看日志：$Logs" }
Write-Host '下一步：配隧道，把云盘开到公网（见 README 第 4 步）。'

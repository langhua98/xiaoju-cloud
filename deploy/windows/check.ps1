# 看一眼各部分是否正常：PostgreSQL、云盘、隧道、本机备份、最近的日志。
param([string]$Root = 'C:\xiaoju')
. "$PSScriptRoot\common.ps1"
$ErrorActionPreference = 'Continue'

function Show($name, $ok, $detail) {
    $mark = if ($ok) { '[好]' } else { '[坏]' }
    $color = if ($ok) { 'Green' } else { 'Red' }
    Write-Host ("{0} {1}  {2}" -f $mark, $name, $detail) -ForegroundColor $color
}

$pg = Get-Service -Name $PgService -ErrorAction SilentlyContinue
Show 'PostgreSQL' ($pg -and $pg.Status -eq 'Running') "$($pg.Status)，启动类型 $($pg.StartType)"

$task = Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
Show '云盘开机任务' ($task -and $task.State -ne 'Disabled') "$($task.State)"
Show '云盘 /healthz' (Test-Health) 'http://127.0.0.1:8080/healthz'

$cf = Get-Service -Name cloudflared -ErrorAction SilentlyContinue
$ts = Get-Service -Name Tailscale -ErrorAction SilentlyContinue
if ($cf) { Show 'Cloudflare Tunnel' ($cf.Status -eq 'Running') "$($cf.Status)" }
if ($ts) { Show 'Tailscale' ($ts.Status -eq 'Running') "$($ts.Status)" }
if (-not $cf -and -not $ts) { Show '隧道' $false '还没装 cloudflared 或 Tailscale' }

$last = Get-ChildItem $Backups -Filter 'xiaoju-*.dump' -ErrorAction SilentlyContinue | Sort-Object LastWriteTime | Select-Object -Last 1
if ($last) { Show '本机备份' ($last.LastWriteTime -gt (Get-Date).AddHours(-30)) "$($last.Name)，$([math]::Round($last.Length / 1MB, 1)) MB" }
else { Show '本机备份' $false '还没有（每天 3:40 自动备份；想马上试就运行 backup.ps1）' }

$log = Get-ChildItem $Logs -Filter 'xiaoju-*.log' -ErrorAction SilentlyContinue | Sort-Object LastWriteTime | Select-Object -Last 1
if ($log) {
    Write-Host ''
    Write-Host "最近的日志（$($log.FullName)）："
    Get-Content $log.FullName -Tail 15
}

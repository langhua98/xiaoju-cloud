# 把云盘更新到最新版：从 GitHub Release「windows-latest」下载 xiaoju.exe（main 分支每次改动会自动重新编译），
# 校验 SHA-256，换掉旧的，再重启云盘。
#   update.ps1               更新并重启
#   update.ps1 -RestartOnly  不下载，只重启（改了 xiaoju.env 以后用）
#   update.ps1 -NoStart      只下载，不启动（install.ps1 用）
# 注意：重启会打断正在进行的上传，挑没人用的时候。
param(
    [string]$Root = 'C:\xiaoju',
    [switch]$RestartOnly,
    [switch]$NoStart
)
. "$PSScriptRoot\common.ps1"
Assert-Admin

if (-not $RestartOnly) {
    $base = "https://github.com/$Repo/releases/download/$ReleaseTag"
    $new = "$Exe.new"
    $sumFile = "$Exe.sha256"
    Download "$base/xiaoju.exe" $new
    Download "$base/xiaoju.exe.sha256" $sumFile
    $want = ((Get-Content $sumFile -TotalCount 1) -split '\s+')[0].ToLower()
    $got = (Get-FileHash -Algorithm SHA256 $new).Hash.ToLower()
    if ($want -ne $got) {
        Remove-Item $new
        throw "下载的 xiaoju.exe 校验不对（要 $want，实际 $got），没有替换。"
    }
    if ((Test-Path $Exe) -and ((Get-FileHash -Algorithm SHA256 $Exe).Hash.ToLower() -eq $got)) {
        Remove-Item $new
        Write-Host '已经是最新版。'
        if (-not $NoStart) { Write-Host '没有重启。要重启用 -RestartOnly。' }
        return
    }
    Stop-Xiaoju
    Move-Item -Force $new $Exe
    Write-Host "xiaoju.exe 已更新（sha256 $got）。"
}

if ($NoStart) { return }
if ((Get-ScheduledTask -TaskName $TaskName).State -eq 'Disabled') {
    Write-Host '云盘还没启用（数据库还没恢复），先运行 restore.ps1。'
    return
}
Stop-Xiaoju
Start-Xiaoju
foreach ($i in 1..30) {
    Start-Sleep -Seconds 2
    if (Test-Health) { Write-Host '云盘已启动。' -ForegroundColor Green; return }
}
Write-Warning "一分钟了还没起来，看看日志：$Logs"

# 开机任务 xiaoju-cloud 跑的就是这个：一直运行云盘，退出了 10 秒后再拉起来。
# 不用手动运行。日志在 C:\xiaoju\logs\xiaoju-日期.log，保留 14 天。
param([string]$Root = 'C:\xiaoju')
. "$PSScriptRoot\common.ps1"

Get-Process -Name xiaoju -ErrorAction SilentlyContinue | Stop-Process -Force
Set-Location $Root
while ($true) {
    Get-ChildItem $Logs -Filter 'xiaoju-*.log' -ErrorAction SilentlyContinue |
        Where-Object { $_.LastWriteTime -lt (Get-Date).AddDays(-14) } | Remove-Item -ErrorAction SilentlyContinue
    $log = Join-Path $Logs ('xiaoju-{0:yyyyMMdd}.log' -f (Get-Date))
    try {
        Use-EnvFile | Out-Null   # 每次都重新读，改了 xiaoju.env 重启一下就生效
        Add-Content -Path $log -Value "---- $(Get-Date -Format s) 启动"
        cmd.exe /c "`"$Exe`" serve >> `"$log`" 2>&1"
        Add-Content -Path $log -Value "---- $(Get-Date -Format s) 退出，退出码 $LASTEXITCODE，10 秒后重启"
    } catch {
        Add-Content -Path $log -Value "---- $(Get-Date -Format s) run.ps1 出错：$_"
    }
    Start-Sleep -Seconds 10
}

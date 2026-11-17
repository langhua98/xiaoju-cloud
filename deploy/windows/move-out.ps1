# 把云盘从这台电脑搬到越狱 iPhone（见 deploy/ios-jailbreak/README.md）。管理员 PowerShell：
#   move-out.ps1          只打包密钥和 Telegram 会话文件（试装用，电脑上的云盘照常运行）
#   move-out.ps1 -Final   正式搬家：停掉电脑上的云盘并禁用开机任务，导出最新的完整数据库备份 move.xjdb（一条不丢）
#   move-out.ps1 -Undo    搬回来：重新启用并启动电脑上的云盘（手机上的云盘要先停掉）
# 打包的东西放在 C:\xiaoju\config\move（只有管理员能读），最后会打出往手机拷的命令。
param(
    [string]$Root = 'C:\xiaoju',
    [switch]$Final,
    [switch]$Undo,
    # 手机的地址（Tailscale 里的 IP 或名字，或者家里 Wi-Fi 的 IP），只用来打出拷贝命令
    [string]$Phone = '<手机IP>'
)
. "$PSScriptRoot\common.ps1"
Assert-Admin

if ($Undo) {
    Start-Xiaoju
    foreach ($i in 1..30) { Start-Sleep -Seconds 2; if (Test-Health) { break } }
    if (Test-Health) { Write-Host '电脑上的云盘已启动。' -ForegroundColor Green } else { Write-Warning "一分钟了还没起来，看日志：$Logs" }
    Write-Host '还要做：Tailscale 后台把这台电脑改回 node，然后运行  tailscale funnel --bg 8080'
    return
}

$out = Join-Path $Config 'move'
Remove-Item -Recurse -Force $out -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force -Path $out | Out-Null

if ($Final) {
    Step '停掉电脑上的云盘（之后不会再有新数据写进这台电脑）'
    Stop-Xiaoju
    Disable-ScheduledTask -TaskName $TaskName | Out-Null
    Step '导出完整的数据库备份 move.xjdb（用主密钥加密）'
    Use-EnvFile | Out-Null
    $ErrorActionPreference = 'Continue'   # xiaoju 的日志写在 stderr，别当成出错
    & $Exe dbbackup (Join-Path $out 'move.xjdb') 2>&1 | ForEach-Object { "$_" }
    $code = $LASTEXITCODE
    $ErrorActionPreference = 'Stop'
    if ($code -ne 0) {
        Write-Warning "导出失败（退出码 $code）。电脑上的云盘已经停了，要恢复就运行 move-out.ps1 -Undo。"
        exit 1
    }
}

Step '拷密钥和 Telegram 会话文件'
Copy-Item $EnvFile (Join-Path $out 'xiaoju.env')
foreach ($f in @('tg-session.json', 'tg-backup-session.json')) {
    $p = Join-Path $DataDir $f
    if (Test-Path $p) { Copy-Item $p (Join-Path $out $f) }
}
Get-ChildItem $out | ForEach-Object { Write-Host ("  {0}  {1:N0} 字节" -f $_.Name, $_.Length) }

Write-Host ''
Write-Host '在这个窗口里运行下面这一行，拷到手机（密码是手机 mobile 用户的密码）：' -ForegroundColor Green
Write-Host "  scp -r `"$out`" mobile@${Phone}:/var/mobile/xiaoju/inbox/" -ForegroundColor Yellow
Write-Host '拷完在手机上（root）运行：  sh /var/mobile/xiaoju/scripts/import-from-pc.sh'
if ($Final) {
    Write-Host '然后：  sh /var/mobile/xiaoju/scripts/restore.sh /var/mobile/xiaoju/inbox/move/move.xjdb'
}
Write-Host '拷完以后可以删掉这个文件夹（里面有主密钥）：' -NoNewline; Write-Host "  Remove-Item -Recurse $out"

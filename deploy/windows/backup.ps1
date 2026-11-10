# 本机备份：pg_dump 导出整个库到 C:\xiaoju\backups，保留 14 天。开机任务 xiaoju-pg-backup 每天 3:40 跑一次。
# 恢复本机备份：pg_restore -h 127.0.0.1 -U xiaoju -d xiaoju --clean --if-exists <文件>
# （云盘自己也会每天把加密备份传到 Telegram，那份用 restore.ps1 恢复。）
param([string]$Root = 'C:\xiaoju')
. "$PSScriptRoot\common.ps1"

$envs = Use-EnvFile
$out = Join-Path $Backups ('xiaoju-{0:yyyyMMdd-HHmm}.dump' -f (Get-Date))
& (Join-Path $PgHome 'bin\pg_dump.exe') "--dbname=$($envs['XIAOJU_DATABASE_URL'])" -Fc -f $out
if ($LASTEXITCODE -ne 0) {
    Remove-Item $out -ErrorAction SilentlyContinue
    Add-Content (Join-Path $Logs 'backup.log') "$(Get-Date -Format s) 备份失败，退出码 $LASTEXITCODE"
    exit 1
}
Add-Content (Join-Path $Logs 'backup.log') "$(Get-Date -Format s) 备份成功 $out $((Get-Item $out).Length) 字节"
Get-ChildItem $Backups -Filter 'xiaoju-*.dump' | Where-Object { $_.LastWriteTime -lt (Get-Date).AddDays(-14) } | Remove-Item

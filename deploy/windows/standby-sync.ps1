# 只读备用：把本机数据库整个复制到云端的备用数据库（Hugging Face 上的只读备用云盘连的就是它）。
# 电脑关着时手机、网页自动换到备用，能看、能下载，不能改；电脑开了自动换回来。见 README.md「8. 云端只读备用」。
#
#   standby-sync.ps1 -Setup '<云端数据库连接串>'   第一次：记下连接串，注册每小时一次的开机任务，马上同步一次
#   standby-sync.ps1                                  马上同步一次（开机任务 xiaoju-standby-sync 每小时跑的就是这个）
#
# 同步是整库覆盖（一个事务里先删后建，失败了云端还是上一份，不会只剩一半）。云端那边改不了数据，所以不会丢东西。
param(
    [string]$Root = 'C:\xiaoju',
    [string]$Setup,
    [int]$EveryMinutes = 60
)
. "$PSScriptRoot\common.ps1"

$StandbyTaskName = 'xiaoju-standby-sync'
$logFile = Join-Path $Logs 'standby-sync.log'
function Log([string]$msg) {
    Add-Content -Encoding UTF8 -Path $logFile -Value "$(Get-Date -Format s) $msg"
    Write-Host $msg
}
$pgBin = Join-Path $PgHome 'bin'

function LocalSql([string]$url, [string]$q) {
    $out = & (Join-Path $pgBin 'psql.exe') "--dbname=$url" -v ON_ERROR_STOP=1 -tAc $q
    if ($LASTEXITCODE -ne 0) { throw "psql 出错：$q" }
    return "$out".Trim()
}

if ($Setup) {
    Assert-Admin
    $url = $Setup.Trim()
    if ($url -notmatch '^postgres(ql)?://') { throw '连接串应该以 postgresql:// 开头，再复制一次试试。' }
    Step '1/3 检查能不能连上云端数据库'
    $ver = LocalSql $url 'SHOW server_version'
    Write-Host "连上了，云端 PostgreSQL 版本 $ver"
    $envs = Read-EnvFile $EnvFile
    $envs['XIAOJU_STANDBY_DATABASE_URL'] = $url
    Write-EnvFile $EnvFile $envs

    Step '2/3 注册开机任务（每小时同步一次）'
    if ($PSScriptRoot -ne $Scripts) { Copy-Item -Force $PSCommandPath (Join-Path $Scripts 'standby-sync.ps1') }
    $principal = New-ScheduledTaskPrincipal -UserId 'SYSTEM' -LogonType ServiceAccount -RunLevel Highest
    $settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable `
        -ExecutionTimeLimit (New-TimeSpan -Minutes 50) -MultipleInstances IgnoreNew
    $action = New-ScheduledTaskAction -Execute 'powershell.exe' `
        -Argument "-NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -File `"$Scripts\standby-sync.ps1`" -Root `"$Root`""
    # 开机 5 分钟后第一次（等数据库、云盘起来），之后每隔 EveryMinutes 分钟
    $boot = New-ScheduledTaskTrigger -AtStartup
    $boot.Delay = 'PT5M'
    $repeat = New-ScheduledTaskTrigger -Once -At (Get-Date).AddMinutes(5) `
        -RepetitionInterval (New-TimeSpan -Minutes $EveryMinutes) -RepetitionDuration (New-TimeSpan -Days 3650)
    Register-ScheduledTask -TaskName $StandbyTaskName -Action $action -Trigger @($boot, $repeat) `
        -Principal $principal -Settings $settings -Force | Out-Null
    Write-Host "开机任务 $StandbyTaskName 已注册。"
    Step '3/3 马上同步一次'
}

$envs = Use-EnvFile
$local = $envs['XIAOJU_DATABASE_URL']
$remote = $envs['XIAOJU_STANDBY_DATABASE_URL']
if (-not $remote) { throw "还没设置云端数据库，先运行：standby-sync.ps1 -Setup '<连接串>'" }

try {
    # 本机库是空的（比如刚重装还没恢复）时不同步，免得把云端的好数据也清空
    $files = [int](LocalSql $local 'SELECT count(*) FROM files')
    if ($files -lt 1) { throw '本机数据库里一个文件都没有，可能还没恢复，这次不同步。' }
    $size = [long](LocalSql $local 'SELECT pg_database_size(current_database())')
    $thumbs = [long](LocalSql $local "SELECT pg_total_relation_size('thumbnails')")
    $mb = [math]::Round($size / 1MB)
    # 免费云数据库一般只有 0.5 GB：库太大时不带缩略图（备用模式看照片时现做，慢一点）
    $skipThumbs = ($size -gt 450MB) -and ($size - $thumbs -lt 450MB)
    if ($size - $thumbs -ge 450MB) { Log "提醒：数据库有 $mb MB，去掉缩略图也超过 450 MB，免费云数据库可能放不下。" }

    $sql = Join-Path $TmpDir 'standby.sql'
    $args = @("--dbname=$local", '-Fp', '--encoding=UTF8', '--clean', '--if-exists', '--no-owner', '--no-privileges', '-f', "$sql.raw")
    if ($skipThumbs) { $args += '--exclude-table-data=thumbnails' }
    $t0 = Get-Date
    & (Join-Path $pgBin 'pg_dump.exe') @args
    if ($LASTEXITCODE -ne 0) { throw "pg_dump 失败，退出码 $LASTEXITCODE" }

    # PostgreSQL 17 导出的开头有 SET transaction_timeout，老版本的云端数据库不认识，去掉
    $utf8 = New-Object Text.UTF8Encoding($false)
    $in = New-Object IO.StreamReader("$sql.raw", $utf8)
    $out = New-Object IO.StreamWriter($sql, $false, $utf8)
    try {
        while ($null -ne ($line = $in.ReadLine())) {
            if ($line -match '^SET transaction_timeout') { continue }
            $out.WriteLine($line)
        }
    } finally { $in.Close(); $out.Close() }
    Remove-Item "$sql.raw"

    # 一个事务里执行：中途出错整个回滚，云端保持上一次的样子
    $env:PGCLIENTENCODING = 'UTF8'
    & (Join-Path $pgBin 'psql.exe') "--dbname=$remote" -q -X -v ON_ERROR_STOP=1 --single-transaction -f $sql | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "写入云端失败，退出码 $LASTEXITCODE（云端还是上一次同步的数据）" }
    Remove-Item $sql
    $note = if ($skipThumbs) { '，不带缩略图' } else { '' }
    Log "同步成功：$files 个文件，本机库 $mb MB$note，用时 $([math]::Round(((Get-Date) - $t0).TotalSeconds)) 秒"
} catch {
    Log "同步失败：$_"
    exit 1
}

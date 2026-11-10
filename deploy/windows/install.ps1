# 在家里的 Windows 电脑上装好小橘云盘需要的一切（方案 A：云盘和 PostgreSQL 都在本机）。
# 用法（管理员 PowerShell）：
#   powershell -ExecutionPolicy Bypass -File install.ps1
# 可以重复运行：已经装好的部分会跳过。不会启动云盘，恢复完数据库（restore.ps1）才启动。
param(
    [string]$Root = 'C:\xiaoju',
    # PostgreSQL 数据放哪儿。默认在 $Root\pgdata；C 盘不够大就改到别的盘，比如 D:\xiaoju-pgdata
    [string]$PgData = ''
)
. "$PSScriptRoot\common.ps1"
Assert-Admin
if (-not $PgData) { $PgData = Join-Path $Root 'pgdata' }

Step '1/8 关掉睡眠和休眠（插电时）'
powercfg /change standby-timeout-ac 0
powercfg /change hibernate-timeout-ac 0
powercfg /hibernate off
Write-Host '已关闭。断电后自动开机要在 BIOS 里设置（见 README）。'

Step '2/8 建目录'
foreach ($d in @($Root, $Bin, $Tools, $Config, $DataDir, $TmpDir, $Logs, $Backups, $Scripts)) {
    New-Item -ItemType Directory -Force -Path $d | Out-Null
}
# config 里有主密钥、Telegram 令牌：只给管理员和 SYSTEM
icacls $Config /inheritance:r /grant:r '*S-1-5-32-544:(OI)(CI)F' '*S-1-5-18:(OI)(CI)F' | Out-Null
Copy-Item -Force "$PSScriptRoot\*.ps1" $Scripts
Write-Host "脚本已复制到 $Scripts，以后都从那里运行。"

Step '3/8 安装 PostgreSQL 17'
$superFile = Join-Path $Config 'postgres-superuser.txt'
if (Get-Service -Name $PgService -ErrorAction SilentlyContinue) {
    Write-Host "已经装过（服务 $PgService），跳过。"
    if (-not (Test-Path $superFile)) { throw "找不到 $superFile（postgres 超级用户密码）。PostgreSQL 不是这个脚本装的，请手动建库或先卸载。" }
} else {
    $superPw = New-Secret
    Set-Content -Encoding ASCII -Path $superFile -Value $superPw
    $installer = Join-Path $env:TEMP 'postgresql-17-windows-x64.exe'
    Download 'https://get.enterprisedb.com/postgresql/postgresql-17.8-1-windows-x64.exe' $installer
    Write-Host "安装中（几分钟），数据目录：$PgData"
    $p = Start-Process -Wait -PassThru -FilePath $installer -ArgumentList @(
        '--mode', 'unattended', '--unattendedmodeui', 'none',
        '--superpassword', $superPw, '--serverport', '5432',
        '--datadir', "`"$PgData`"", '--servicename', $PgService,
        '--enable-components', 'server,commandlinetools',
        '--disable-components', 'pgAdmin,stackbuilder')
    if ($p.ExitCode -ne 0) { throw "PostgreSQL 安装失败，退出码 $($p.ExitCode)。日志在 $env:TEMP\install-postgresql.log" }
    Remove-Item $installer -ErrorAction SilentlyContinue
}
Set-Service -Name $PgService -StartupType Automatic
Start-Service -Name $PgService

Step '4/8 只监听本机，建库和用户 xiaoju'
$env:PGPASSWORD = (Get-Content $superFile -TotalCount 1).Trim()
$psql = Join-Path $PgHome 'bin\psql.exe'
function Sql([string]$q, [string]$db = 'postgres') {
    $out = & $psql -h 127.0.0.1 -U postgres -d $db -v ON_ERROR_STOP=1 -tAc $q
    if ($LASTEXITCODE -ne 0) { throw "psql 出错：$q" }
    return $out
}
if ((Sql 'SHOW listen_addresses') -ne 'localhost') {
    Sql "ALTER SYSTEM SET listen_addresses = 'localhost'" | Out-Null
    Restart-Service -Name $PgService
    Start-Sleep -Seconds 3
}
$envs = Read-EnvFile $EnvFile
if ($envs['XIAOJU_DATABASE_URL'] -match '^postgres://xiaoju:([0-9a-f]+)@') {
    $dbPw = $Matches[1]
} else {
    $dbPw = New-Secret
}
if (-not (Sql "SELECT 1 FROM pg_roles WHERE rolname = 'xiaoju'")) {
    Sql "CREATE ROLE xiaoju LOGIN PASSWORD '$dbPw'" | Out-Null
} else {
    Sql "ALTER ROLE xiaoju PASSWORD '$dbPw'" | Out-Null
}
if (-not (Sql "SELECT 1 FROM pg_database WHERE datname = 'xiaoju'")) {
    Sql 'CREATE DATABASE xiaoju OWNER xiaoju' | Out-Null
    Write-Host '建好了空库 xiaoju（表等恢复时再建）。'
} else {
    Write-Host '库 xiaoju 已经有了，跳过。'
}
$hasTables = [int](Sql "SELECT count(*) FROM pg_tables WHERE schemaname = 'public'" 'xiaoju') -gt 0
Remove-Item Env:PGPASSWORD

Step '5/8 下载工具：age（解密密钥）、ffmpeg 和 vips（缩略图）'
function Get-Zip([string]$name, [string]$url, [string]$check) {
    $dest = Join-Path $Tools $name
    if (Test-Path (Join-Path $dest $check)) { Write-Host "$name 已经有了。"; return }
    $zip = Join-Path $env:TEMP "$name.zip"
    $tmp = Join-Path $env:TEMP "$name-unzip"
    Download $url $zip
    Remove-Item -Recurse -Force $tmp, $dest -ErrorAction SilentlyContinue
    Expand-Archive -Path $zip -DestinationPath $tmp
    # 压缩包里都套了一层目录，取那一层
    $inner = Get-ChildItem $tmp -Directory | Select-Object -First 1
    Move-Item $inner.FullName $dest
    Remove-Item -Recurse -Force $tmp, $zip
}
Get-Zip 'age'    'https://github.com/FiloSottile/age/releases/download/v1.2.1/age-v1.2.1-windows-amd64.zip' 'age.exe'
Get-Zip 'ffmpeg' 'https://github.com/GyanD/codexffmpeg/releases/download/7.1.1/ffmpeg-7.1.1-essentials_build.zip' 'bin\ffmpeg.exe'
Get-Zip 'vips'   'https://github.com/libvips/build-win64-mxe/releases/download/v8.17.2/vips-dev-w64-all-8.17.2.zip' 'bin\vips.exe'

Step '6/8 下载云盘程序 xiaoju.exe'
& "$Scripts\update.ps1" -Root $Root -NoStart

Step '7/8 写环境变量文件，生成解密用的密钥对'
$envs['XIAOJU_DATABASE_URL']    = "postgres://xiaoju:$dbPw@127.0.0.1:5432/xiaoju?sslmode=disable"
$envs['XIAOJU_HTTP_ADDR']       = '127.0.0.1:8080'
$envs['XIAOJU_STORAGE']         = 'telegram'
$envs['XIAOJU_TMP_DIR']         = $TmpDir
$envs['TG_SESSION_FILE']        = Join-Path $DataDir 'tg-session.json'
$envs['TG_BACKUP_SESSION_FILE'] = Join-Path $DataDir 'tg-backup-session.json'
Write-EnvFile $EnvFile $envs
if (-not (Test-Path $AgeKey)) {
    & (Join-Path $Tools 'age\age-keygen.exe') -o $AgeKey
}
$pub = (Get-Content $AgeKey | Where-Object { $_ -match '^# public key: ' }) -replace '^# public key: ', ''

Step '8/8 注册开机任务'
$ps = 'powershell.exe'
$principal = New-ScheduledTaskPrincipal -UserId 'SYSTEM' -LogonType ServiceAccount -RunLevel Highest
# 云盘：开机就启动（不用登录），挂了 run.ps1 自己会重启它
$settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -RestartCount 999 `
    -RestartInterval (New-TimeSpan -Minutes 1) -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable
$action = New-ScheduledTaskAction -Execute $ps -Argument "-NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -File `"$Scripts\run.ps1`" -Root `"$Root`""
Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger (New-ScheduledTaskTrigger -AtStartup) `
    -Principal $principal -Settings $settings -Force | Out-Null
if (-not $hasTables) {
    # 数据还没恢复：先别启动，不然云盘会在空库里建表，dbrestore 就不肯恢复了
    Disable-ScheduledTask -TaskName $TaskName | Out-Null
}
# 每天凌晨 3:40 本机备份一次
$bsettings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable
$baction = New-ScheduledTaskAction -Execute $ps -Argument "-NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -File `"$Scripts\backup.ps1`" -Root `"$Root`""
Register-ScheduledTask -TaskName $BackupTaskName -Action $baction -Trigger (New-ScheduledTaskTrigger -Daily -At '03:40') `
    -Principal $principal -Settings $bsettings -Force | Out-Null

Write-Host ''
Write-Host '装好了。下一步：把 GitHub 上的密钥导过来。' -ForegroundColor Green
Write-Host '1. 打开 https://github.com/langhua98/xiaoju-cloud/actions/workflows/export-secrets.yml'
Write-Host '2. 点 Run workflow，公钥一栏粘贴下面这一行：'
Write-Host "   $pub" -ForegroundColor Yellow
Write-Host '3. 跑完后下载 secrets 那个文件（zip），然后运行：'
Write-Host "   powershell -ExecutionPolicy Bypass -File $Scripts\import-secrets.ps1 <下载的 zip 路径>"
Set-Content -Encoding ASCII -Path (Join-Path $Root 'age-public-key.txt') -Value $pub

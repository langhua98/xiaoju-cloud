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
# Telegram（MTProto）要求本机时间准，时间服务默认可能没开：设成自动启动并马上对一次时
Set-Service -Name w32time -StartupType Automatic
Start-Service -Name w32time -ErrorAction SilentlyContinue
w32tm /config /manualpeerlist:"time.windows.com,0x9 pool.ntp.org,0x9" /syncfromflags:manual /update | Out-Null
w32tm /resync /force | Out-Null
Write-Host '已开启自动对时。'
# 关屏后别断网：网卡不许 Windows 为省电关掉，Wi-Fi 驱动自己的省电也关掉，
# 支持「S0 低电量待机」的电脑（多数新电脑）插电待机时保持联网。
foreach ($a in Get-NetAdapter -Physical | Where-Object { $_.InterfaceDescription -notmatch 'Bluetooth' }) {
    # 设备管理器「允许计算机关闭此设备以节约电源」：有这个开关的网卡立刻关掉；注册表那份重启后也生效
    Get-CimInstance -Namespace root/wmi -ClassName MSPower_DeviceEnable -ErrorAction SilentlyContinue |
        Where-Object { $_.InstanceName -like "$($a.PnPDeviceID)*" } |
        ForEach-Object { Set-CimInstance -InputObject $_ -Property @{ Enable = $false } }
    Get-ChildItem 'HKLM:\SYSTEM\CurrentControlSet\Control\Class\{4d36e972-e325-11ce-bfc1-08002be10318}' -ErrorAction SilentlyContinue |
        Where-Object { (Get-ItemProperty $_.PSPath -ErrorAction SilentlyContinue).NetCfgInstanceId -eq $a.InterfaceGuid } |
        ForEach-Object { Set-ItemProperty -Path $_.PSPath -Name PnPCapabilities -Value 0x18 -Type DWord }
    # 驱动高级属性里的省电（联发科叫 Power Saving / LowPowerEnable，英特尔叫 MIMO 省电等）：能选 Disabled 的都关掉。
    # 改了网卡会重启一下，断网几秒。
    Get-NetAdapterAdvancedProperty -Name $a.Name -ErrorAction SilentlyContinue |
        Where-Object { $_.DisplayName -match 'Power Sav|省电|节能|Energy.Efficient|Green Ethernet' -and
                       $_.ValidDisplayValues -contains 'Disabled' -and $_.DisplayValue -ne 'Disabled' } |
        ForEach-Object { Set-NetAdapterAdvancedProperty -Name $a.Name -RegistryKeyword $_.RegistryKeyword -DisplayValue 'Disabled' }
}
# 无线适配器省电模式：最高性能（S0 待机的电脑上这项被系统隐藏了，设不上就算了）
# （交给 cmd 吞掉输出：powercfg 设不上时写 stderr，PowerShell 5.1 会当成出错让脚本中断）
cmd /c 'powercfg /setacvalueindex SCHEME_CURRENT 19cafa3d-c9d5-4b0e-a7c3-2c1daedb5a10 12bbebe6-58d6-4636-95bb-3217ef867c1a 0 >nul 2>&1'
# 待机时的网络连接：插电时启用
cmd /c 'powercfg /setacvalueindex SCHEME_CURRENT SUB_NONE F15576E8-98B7-4186-B944-EAFA664402D9 1 >nul 2>&1'
powercfg /setactive SCHEME_CURRENT
Write-Host '已关掉网卡省电，关屏后保持联网。'

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
    # 用户名是中文时 %TEMP% 里有中文，EDB 安装器会在初始化数据库那步失败：换成纯英文的临时目录
    $pgTmp = Join-Path $TmpDir 'pg-install'
    New-Item -ItemType Directory -Force -Path $pgTmp | Out-Null
    $oldTemp = $env:TEMP; $oldTmp = $env:TMP
    $env:TEMP = $pgTmp; $env:TMP = $pgTmp
    $installer = Join-Path $pgTmp 'postgresql-17-windows-x64.exe'
    Download 'https://get.enterprisedb.com/postgresql/postgresql-17.8-1-windows-x64.exe' $installer
    Write-Host "安装中（几分钟），数据目录：$PgData"
    $p = Start-Process -Wait -PassThru -FilePath $installer -ArgumentList @(
        '--mode', 'unattended', '--unattendedmodeui', 'none',
        '--superpassword', $superPw, '--serverport', '5432',
        '--datadir', "`"$PgData`"", '--servicename', $PgService,
        '--enable-components', 'server,commandlinetools',
        '--disable-components', 'pgAdmin,stackbuilder')
    $env:TEMP = $oldTemp; $env:TMP = $oldTmp
    if ($p.ExitCode -ne 0) { throw "PostgreSQL 安装失败，退出码 $($p.ExitCode)。日志在 $pgTmp\install-postgresql.log" }
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
# 上次中途失败可能留下空文件：里面没有公钥就当没有，重新生成（还没用这把钥匙加密过任何东西）
if ((Test-Path $AgeKey) -and -not (Select-String -Quiet -Path $AgeKey -Pattern '^# public key: age1')) {
    Remove-Item -Force $AgeKey
}
if (-not (Test-Path $AgeKey)) {
    # age-keygen 在 Windows 上总会往 stderr 打一句 "world-readable" 警告（config 目录其实已经限制了权限）。
    # 输出被重定向时 PowerShell 5.1 会把 stderr 当成出错，所以这里临时放宽，按退出码判断。
    $ErrorActionPreference = 'Continue'
    & (Join-Path $Tools 'age\age-keygen.exe') -o $AgeKey 2>&1 | ForEach-Object { "$_" } | Out-Null
    $code = $LASTEXITCODE
    $ErrorActionPreference = 'Stop'
    if ($code -ne 0 -or -not (Test-Path $AgeKey)) { throw "age-keygen 失败（退出码 $code）。" }
}
$pub = (Get-Content $AgeKey | Where-Object { $_ -match '^# public key: ' }) -replace '^# public key: ', ''
if (-not $pub) { throw "$AgeKey 里没有公钥，删掉它再运行一次。" }

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

# 几个脚本共用的路径和小工具。其它脚本开头用 `. "$PSScriptRoot\common.ps1"` 引进来。
# 兼容 Windows 自带的 PowerShell 5.1（本文件存成带 BOM 的 UTF-8，不然中文会乱码）。

$ErrorActionPreference = 'Stop'

if (-not $Root) { $Root = 'C:\xiaoju' }
$Bin     = Join-Path $Root 'bin'        # xiaoju.exe
$Tools   = Join-Path $Root 'tools'      # age、ffmpeg、vips
$Config  = Join-Path $Root 'config'     # xiaoju.env、密钥（只有管理员和 SYSTEM 能读）
$DataDir = Join-Path $Root 'data'       # Telegram 会话文件
$TmpDir  = Join-Path $Root 'tmp'
$Logs    = Join-Path $Root 'logs'
$Backups = Join-Path $Root 'backups'    # 每天的 pg_dump
$Scripts = Join-Path $Root 'scripts'

$EnvFile = Join-Path $Config 'xiaoju.env'
$AgeKey  = Join-Path $Config 'age-key.txt'
$Exe     = Join-Path $Bin 'xiaoju.exe'

$PgVersion = '17'
$PgHome    = "C:\Program Files\PostgreSQL\$PgVersion"
$PgService = "postgresql-x64-$PgVersion"

$TaskName       = 'xiaoju-cloud'
$BackupTaskName = 'xiaoju-pg-backup'

$Repo = 'langhua98/xiaoju-cloud'
$ReleaseTag = 'windows-latest'

# 老系统上 Invoke-WebRequest 默认不用 TLS 1.2，GitHub 会拒绝
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

function Assert-Admin {
    $id = [Security.Principal.WindowsIdentity]::GetCurrent()
    if (-not (New-Object Security.Principal.WindowsPrincipal($id)).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw '请用「以管理员身份运行」的 PowerShell 执行这个脚本。'
    }
}

function Step($msg) { Write-Host ''; Write-Host "== $msg" -ForegroundColor Cyan }

# 读 KEY=VALUE 格式的文件，# 开头的是注释。
function Read-EnvFile([string]$path) {
    $h = [ordered]@{}
    if (-not (Test-Path $path)) { return $h }
    foreach ($line in Get-Content -Encoding UTF8 $path) {
        $t = $line.Trim()
        if (-not $t -or $t.StartsWith('#')) { continue }
        $i = $t.IndexOf('=')
        if ($i -lt 1) { continue }
        $h[$t.Substring(0, $i).Trim()] = $t.Substring($i + 1)
    }
    return $h
}

function Write-EnvFile([string]$path, $h) {
    $lines = @('# 小橘云盘的环境变量。由 install.ps1 / import-secrets.ps1 生成，改完要重启云盘（update.ps1 -RestartOnly）。')
    foreach ($k in $h.Keys) { $lines += "$k=$($h[$k])" }
    Set-Content -Encoding UTF8 -Path $path -Value $lines
}

# 把环境变量文件加载进当前进程，xiaoju.exe 会继承。
function Use-EnvFile {
    $h = Read-EnvFile $EnvFile
    if ($h.Count -eq 0) { throw "没找到 $EnvFile，先运行 install.ps1。" }
    foreach ($k in $h.Keys) { [Environment]::SetEnvironmentVariable($k, $h[$k], 'Process') }
    $env:PATH = "$Tools\ffmpeg\bin;$Tools\vips\bin;$PgHome\bin;$env:PATH"
    return $h
}

function New-Secret([int]$bytes = 24) {
    $b = New-Object byte[] $bytes
    (New-Object Security.Cryptography.RNGCryptoServiceProvider).GetBytes($b)
    return -join ($b | ForEach-Object { '{0:x2}' -f $_ })
}

function Download([string]$url, [string]$out) {
    Write-Host "下载 $url"
    $ProgressPreference = 'SilentlyContinue'   # 5.1 显示进度条时下载慢很多
    Invoke-WebRequest -UseBasicParsing -Uri $url -OutFile $out
}

# 停掉云盘：计划任务停了以后 xiaoju.exe 子进程可能还活着，一起杀掉。
function Stop-Xiaoju {
    Stop-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
    Get-Process -Name xiaoju -ErrorAction SilentlyContinue | Stop-Process -Force
    Start-Sleep -Seconds 1
}

function Start-Xiaoju {
    Enable-ScheduledTask -TaskName $TaskName | Out-Null
    Start-ScheduledTask -TaskName $TaskName
}

function Test-Health {
    try {
        $r = Invoke-WebRequest -UseBasicParsing -TimeoutSec 5 -Uri 'http://127.0.0.1:8080/healthz'
        return $r.StatusCode -eq 200
    } catch { return $false }
}

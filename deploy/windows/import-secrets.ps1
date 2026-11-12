# 把 GitHub 工作流 export-secrets 导出的加密密钥（只有这台电脑的 age 私钥能解开）写进 xiaoju.env。
# 用法：import-secrets.ps1 <下载的 secrets.zip 或 secrets.env.age>
# 本机专用的几项（数据库地址、监听地址、临时目录、会话文件）保留本机的值，不会被覆盖。
param(
    [Parameter(Mandatory = $true)][string]$Path,
    [string]$Root = 'C:\xiaoju'
)
. "$PSScriptRoot\common.ps1"
Assert-Admin

$file = $Path
if ($Path -like '*.zip') {
    $tmp = Join-Path $env:TEMP 'xiaoju-secrets-unzip'
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
    Expand-Archive -Path $Path -DestinationPath $tmp
    $file = (Get-ChildItem $tmp -Recurse -Filter '*.age' | Select-Object -First 1).FullName
    if (-not $file) { throw "$Path 里没有 .age 文件。" }
}

# 直接解密到内存，明文不落盘
$plain = & (Join-Path $Tools 'age\age.exe') -d -i $AgeKey $file
if ($LASTEXITCODE -ne 0) { throw '解密失败：这个文件不是用本机公钥加密的？重新跑一次工作流，公钥用 C:\xiaoju\age-public-key.txt 里的。' }
if ($tmp) { Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue }

$keepLocal = @('XIAOJU_DATABASE_URL', 'XIAOJU_HTTP_ADDR', 'XIAOJU_STORAGE', 'XIAOJU_TMP_DIR', 'TG_SESSION_FILE', 'TG_BACKUP_SESSION_FILE')
$envs = Read-EnvFile $EnvFile
$added = @()
foreach ($line in $plain) {
    $i = $line.IndexOf('=')
    if ($i -lt 1 -or $line.StartsWith('#')) { continue }
    $k = $line.Substring(0, $i); $v = $line.Substring($i + 1)
    if ($keepLocal -contains $k -or $v -eq '') { continue }
    # 连 Telegram 的方式看本机网络：本机已经设过（比如这里的网络只通 websocket）就不让云端的值覆盖
    if ($k -eq 'TG_TRANSPORT' -and $envs['TG_TRANSPORT']) { continue }
    $envs[$k] = $v
    $added += $k
}
Write-EnvFile $EnvFile $envs
Write-Host "写入了 $($added.Count) 项：$($added -join ', ')" -ForegroundColor Green
if (-not $envs['XIAOJU_MASTER_KEY']) { Write-Warning '没有 XIAOJU_MASTER_KEY！没有主密钥，备份和文件都解不开。' }
Write-Host "下一步：先检查备份  powershell -ExecutionPolicy Bypass -File $Scripts\restore.ps1 -Check"

# MSI 卸载行为验证脚本（需管理员权限运行）
#
# 为什么需要它：MSI 的「连数据一起删」靠公开属性 REMOVEDATA + RemoveFolderEx 实现，
# 其中"默认不删"与"勾选才删"是两条完全不同的路径，靠肉眼看界面无法确认到底删没删。
# 本脚本用静默卸载把两条路径各跑一遍，并用哨兵文件断言结果。
#
# ⚠️ 重要：测试 REMOVEDATA=1 时会**真实删除** %AppData%\Mandaryn。
#    脚本会先把它备份成 zip，测试后再恢复。若不希望脚本碰真实数据，加 -NoRestore
#    则只做备份、不自动恢复（你需要自己解压）。
#
# 用法（在管理员 PowerShell 中）：
#   powershell -ExecutionPolicy Bypass -File build\windows\test-msi-uninstall.ps1
#   ... -Msi bin\mandaryn-amd64.msi        # 指定其它 MSI
#   ... -SkipDataDelete                    # 只验"不勾选保数据"，跳过删除分支
#
# 退出码：0 = 全部通过；1 = 有失败项；2 = 环境不满足（非管理员等）

[CmdletBinding()]
param(
    [string]$Msi = "",
    [switch]$SkipDataDelete,
    [switch]$NoRestore
)

$ErrorActionPreference = "Stop"

# ── 0) 前置检查 ────────────────────────────────────────────────
$principal = New-Object Security.Principal.WindowsPrincipal(
    [Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Write-Host "错误：本脚本必须以管理员身份运行（perMachine 安装需要提权）。" -ForegroundColor Red
    Write-Host "      右键 PowerShell → 以管理员身份运行，再执行本脚本。" -ForegroundColor Red
    exit 2
}

if (-not $Msi) {
    $repoRoot = Split-Path -Parent (Split-Path -Parent (Split-Path -Parent $PSCommandPath))
    $Msi = Join-Path $repoRoot "bin\mandaryn-amd64.msi"
}
if (-not (Test-Path $Msi)) {
    Write-Host "错误：找不到 MSI：$Msi" -ForegroundColor Red
    Write-Host "      先构建：wails3 task windows:create:msi:package" -ForegroundColor Red
    exit 2
}
$Msi = (Resolve-Path $Msi).Path

$dataDir = Join-Path $env:APPDATA "Mandaryn"
$sentinel = Join-Path $dataDir "__uninst-test-sentinel.txt"
$results = @()

function Record([string]$name, [bool]$ok, [string]$detail = "") {
    $script:results += [pscustomobject]@{ Name = $name; Ok = $ok; Detail = $detail }
    $color = if ($ok) { "Green" } else { "Red" }
    $mark = if ($ok) { "PASS" } else { "FAIL" }
    Write-Host ("  [{0}] {1} {2}" -f $mark, $name, $detail) -ForegroundColor $color
}

function Invoke-Msi([string]$action, [string]$extra = "") {
    # action: /i 或 /x
    $log = Join-Path $env:TEMP ("mandaryn-msi-{0}-{1}.log" -f ($action -replace '/',''), (Get-Random))
    $args = @($action, $Msi, "/qn", "/norestart", "/l*v", $log)
    if ($extra) { $args += $extra }
    $p = Start-Process -FilePath "msiexec.exe" -ArgumentList $args -Wait -PassThru
    return [pscustomobject]@{ Code = $p.ExitCode; Log = $log }
}

Write-Host ""
Write-Host "=== Mandaryn MSI 卸载行为验证 ===" -ForegroundColor Cyan
Write-Host "MSI      : $Msi"
Write-Host "数据目录 : $dataDir"
Write-Host ""

# ── 1) 备份真实用户数据 ───────────────────────────────────────
$backup = $null
if (Test-Path $dataDir) {
    $backup = Join-Path $env:TEMP ("mandaryn-data-backup-{0}.zip" -f (Get-Date -Format "yyyyMMdd-HHmmss"))
    Write-Host "备份现有数据 → $backup" -ForegroundColor Yellow
    Compress-Archive -Path (Join-Path $dataDir "*") -DestinationPath $backup -Force
    Write-Host ("       已备份 {0:N1} MB" -f ((Get-Item $backup).Length / 1MB)) -ForegroundColor Yellow
} else {
    Write-Host "当前无数据目录（将从干净状态开始测试）" -ForegroundColor Yellow
}

# ── 2) 安装 ───────────────────────────────────────────────────
Write-Host ""
Write-Host "-- 安装 --" -ForegroundColor Cyan
$r = Invoke-Msi "/i"
Record "安装成功（退出码 0）" ($r.Code -eq 0) "exit=$($r.Code)"
if ($r.Code -ne 0) {
    Write-Host "  查看日志：$($r.Log)" -ForegroundColor Red
    exit 1
}
$installedExe = Join-Path $env:ProgramFiles "Oberon\Mandaryn\mandaryn.exe"
Record "安装目录含 mandaryn.exe" (Test-Path $installedExe)

# ── 3) 造哨兵数据 ─────────────────────────────────────────────
New-Item -ItemType Directory -Path $dataDir -Force | Out-Null
"sentinel-$([guid]::NewGuid())" | Set-Content -Path $sentinel -Encoding UTF8
Record "已创建哨兵文件" (Test-Path $sentinel)

# ── 4) 路径 A：不勾选（默认）→ 数据必须保留 ───────────────────
Write-Host ""
Write-Host "-- 路径 A：REMOVEDATA 未设置（等价于用户不勾选）--" -ForegroundColor Cyan
$r = Invoke-Msi "/x"
Record "卸载成功（退出码 0）" ($r.Code -eq 0) "exit=$($r.Code)"
Record "安装目录已删除" (-not (Test-Path (Split-Path $installedExe)))
Record "★ 用户数据**保留**（默认不删）" (Test-Path $sentinel) `
    $(if (-not (Test-Path $sentinel)) { "哨兵文件被删了！" } else { "" })

if (-not $SkipDataDelete) {
    # ── 5) 路径 B：REMOVEDATA=1 → 数据必须被删 ─────────────────
    Write-Host ""
    Write-Host "-- 路径 B：REMOVEDATA=1（等价于用户勾选）--" -ForegroundColor Cyan
    if (-not (Test-Path $sentinel)) {
        "sentinel-$([guid]::NewGuid())" | Set-Content -Path $sentinel -Encoding UTF8
    }
    $r = Invoke-Msi "/i"
    Record "重新安装成功" ($r.Code -eq 0) "exit=$($r.Code)"
    if (-not (Test-Path $sentinel)) {
        New-Item -ItemType Directory -Path $dataDir -Force | Out-Null
        "sentinel-$([guid]::NewGuid())" | Set-Content -Path $sentinel -Encoding UTF8
    }
    $r = Invoke-Msi "/x" "REMOVEDATA=1"
    Record "卸载成功（带 REMOVEDATA=1）" ($r.Code -eq 0) "exit=$($r.Code)"
    Record "★ 用户数据**被删除**（勾选后删）" (-not (Test-Path $sentinel)) `
        $(if (Test-Path $sentinel) { "哨兵文件仍在！删除逻辑没生效" } else { "" })
}

# ── 6) 恢复数据 ───────────────────────────────────────────────
if ($backup -and (Test-Path $backup)) {
    if ($NoRestore) {
        Write-Host ""
        Write-Host "已按 -NoRestore 跳过恢复。备份在：$backup" -ForegroundColor Yellow
        if (Test-Path $dataDir) { Remove-Item -Recurse -Force $dataDir }
        New-Item -ItemType Directory -Path $dataDir -Force | Out-Null
        Expand-Archive -Path $backup -DestinationPath $dataDir -Force
        Write-Host "已恢复备份到 $dataDir" -ForegroundColor Yellow
    } else {
        Write-Host ""
        Write-Host "恢复原数据…" -ForegroundColor Yellow
        if (Test-Path $dataDir) { Remove-Item -Recurse -Force $dataDir }
        New-Item -ItemType Directory -Path $dataDir -Force | Out-Null
        Expand-Archive -Path $backup -DestinationPath $dataDir -Force
        Write-Host "已恢复备份到 $dataDir（备份文件保留在 $backup）" -ForegroundColor Yellow
    }
} elseif (Test-Path $dataDir) {
    # 本来没有数据 → 清掉测试产生的目录
    Remove-Item -Recurse -Force $dataDir
}

# ── 7) 汇总 ───────────────────────────────────────────────────
$failed = $results | Where-Object { -not $_.Ok }
Write-Host ""
Write-Host "=== 结果 ===" -ForegroundColor Cyan
Write-Host ("通过 {0} / {1}" -f ($results.Count - $failed.Count), $results.Count)
if ($failed) {
    Write-Host "失败项：" -ForegroundColor Red
    $failed | ForEach-Object { Write-Host ("  - " + $_.Name + " " + $_.Detail) -ForegroundColor Red }
    Write-Host ""
    Write-Host "注意：本脚本只能验证**静默**路径（无界面）。UI 观感、以及点「删除」后是否" -ForegroundColor Yellow
    Write-Host "      先出现「卸载选项」页，仍需人工双击 MSI 观察一次。" -ForegroundColor Yellow
    exit 1
}

Write-Host ""
Write-Host "静默路径全部通过。" -ForegroundColor Green
Write-Host "仍需人工确认（脚本无法验证）：" -ForegroundColor Yellow
Write-Host "  1. 双击 MSI，向导页应正常显示（中文界面）" -ForegroundColor Yellow
Write-Host "  2. 从「添加/删除程序」卸载，点「删除」后应**先出现「卸载选项」页**" -ForegroundColor Yellow
Write-Host "  3. 该页勾选框默认**不勾选**" -ForegroundColor Yellow
exit 0

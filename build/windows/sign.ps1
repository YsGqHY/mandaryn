# Mandaryn 签名统一入口
#
# 为什么要有这一层：签名后端会变（本机 USB 令牌 / 可导出 PFX / 将来接云签名）。
# 把"怎么签"收敛到一个可插拔入口，CI 与本地就都只认这一条命令：
#
#   build/windows/sign.ps1 -Target exe       -InputPath bin\mandaryn.exe
#   build/windows/sign.ps1 -Target installer -InputPath bin\mandaryn-amd64-installer.exe
#   build/windows/sign.ps1 -Target msi       -InputPath bin\mandaryn-amd64.msi
#
# 契约（调用方可以依赖这些）：
#   - 成功 → 退出码 0，且对 $InputPath **原地**签名（不产出副本，不改路径）
#   - 失败 → 非 0 退出码，原因写 stderr（CI 会因此变红）
#   - 后端由 signing.config.psd1 的 backend 决定；-Backend 可临时覆盖
#
# 配置文件查找顺序：-ConfigPath → $env:MANDARYN_SIGNING_CONFIG → build/windows/signing.config.psd1
# 真实配置文件**不入库**（见 .gitignore），模板见 signing.config.example.psd1。

[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidateSet('exe', 'installer', 'msi')]
    [string]$Target,

    [Parameter(Mandatory = $true)]
    [string]$InputPath,

    [ValidateSet('auto', 'token-csp', 'pfx', 'cloud-esigner')]
    [string]$Backend = 'auto',

    [string]$ConfigPath = '',

    # 仅供自签名证书的本地验证用：跳过 verify。
    # 生产/CI **不要**加这个开关 —— verify 是"签了但链不可用"的唯一拦网。
    [switch]$SkipVerify
)

$ErrorActionPreference = 'Stop'
$here = Split-Path -Parent $MyInvocation.MyCommand.Path

# 定位 signtool：优先 PATH，其次在 Windows SDK 目录里找最新版本。
# 为什么不要求用户手配 PATH：SDK 装好后 signtool 默认**不在** PATH 里，
# 而"装 SDK 却没配 PATH"是这一步最常见的卡点（本机实测就是这种情形）。
function Resolve-SignTool {
    $cmd = Get-Command signtool.exe -ErrorAction SilentlyContinue
    if ($null -ne $cmd) { return $cmd.Source }

    # 注意：不能写 "${env:ProgramFiles(x86)}" —— 变量名里的 "(x86)" 会被解析器当成
    # 子表达式边界而报语法错（本机实测）。用 [Environment] 取值更稳妥。
    # 且必须容错：该环境变量在部分会话（如经 Git Bash 启动）里为空，
    # 直接喂给 Join-Path 会抛 "无法将参数绑定到参数 Path"（本机实测踩到）。
    $roots = @()
    foreach ($name in @('ProgramFiles(x86)', 'ProgramFiles')) {
        $base = [Environment]::GetEnvironmentVariable($name)
        if (-not [string]::IsNullOrWhiteSpace($base)) { $roots += (Join-Path $base 'Windows Kits\10\bin') }
    }
    # 环境变量全空时的硬兜底：SDK 的默认安装位置是固定的
    $roots += 'C:\Program Files (x86)\Windows Kits\10\bin'
    $roots = $roots | Select-Object -Unique
    if ($roots.Count -eq 0) { return $null }
    $candidates = @()
    foreach ($root in $roots) {
        if ([string]::IsNullOrWhiteSpace($root) -or -not (Test-Path $root)) { continue }
        $candidates += Get-ChildItem -Path $root -Directory -ErrorAction SilentlyContinue |
            Where-Object { $_.Name -match '^10\.' } |
            ForEach-Object { Join-Path $_.FullName 'x64\signtool.exe' } |
            Where-Object { Test-Path $_ }
    }
    if ($candidates.Count -eq 0) { return $null }
    # 版本号排序取最新（10.0.19041 < 10.0.22621）
    return ($candidates | Sort-Object { [version]((Split-Path (Split-Path $_ -Parent) -Parent | Split-Path -Leaf)) } |
        Select-Object -Last 1)
}

$script:SignToolPath = Resolve-SignTool

# 统一调用 signtool 并返回退出码。
#
# 为什么不用 `& $signtool @args`：在部分 PowerShell 5.1 会话里（例如经 Git Bash 继承环境），
# 直接调用带空格路径的 exe 会**静默失败**且 $LASTEXITCODE 为空（本机实测，
# 报 CantActivateDocumentInPipeline）。Start-Process -Wait 能稳定拿到退出码。
function Invoke-SignTool {
    param([Parameter(Mandatory = $true)][string[]]$Arguments)
    # Start-Process 的 -ArgumentList 会按空格切分，含空格的参数（如 /d "Mandaryn") 会被
    # 拆成两个 —— 现象是 "File not found: <后半个词>"（本机实测）。因此统一加引号并转义内部引号。
    $quoted = $Arguments | ForEach-Object {
        if ($_ -match '\s') { '"' + ($_ -replace '"', '\"') + '"' } else { $_ }
    }
    $proc = Start-Process -FilePath $script:SignToolPath -ArgumentList ($quoted -join ' ') -NoNewWindow -Wait -PassThru
    return $proc.ExitCode
}

function Fail([string]$message) {
    [Console]::Error.WriteLine("sign.ps1: $message")
    exit 1
}

# —— 1) 定位配置文件 ——
if ([string]::IsNullOrWhiteSpace($ConfigPath)) {
    if (-not [string]::IsNullOrWhiteSpace($env:MANDARYN_SIGNING_CONFIG)) {
        $ConfigPath = $env:MANDARYN_SIGNING_CONFIG
    } elseif (Test-Path (Join-Path $here 'signing.config.psd1')) {
        $ConfigPath = Join-Path $here 'signing.config.psd1'
    }
}
if ([string]::IsNullOrWhiteSpace($ConfigPath) -or -not (Test-Path $ConfigPath)) {
    Fail "找不到签名配置。请复制 signing.config.example.psd1 为 signing.config.psd1 并填写（该文件不入库）。"
}
$Config = Import-PowerShellDataFile -Path $ConfigPath

# —— 2) 校验输入 ——
if (-not (Test-Path -LiteralPath $InputPath)) {
    Fail "待签名文件不存在：$InputPath"
}
$full = (Resolve-Path -LiteralPath $InputPath).Path

# MSI 只能用 signtool：wails3 内置签名器不支持 MSI（能力边界，不是配置问题）。
# 两个后端实现本来就都走 signtool，这里显式记一笔，避免将来有人换成别的实现。
if ($Target -eq 'msi') {
    Write-Host "sign.ps1: target=msi，强制使用 signtool（不支持 wails3 内置签名）"
}

# —— 3) 决定后端 ——
if ($Backend -eq 'auto') {
    if (-not [string]::IsNullOrWhiteSpace($Config.backend)) {
        $Backend = $Config.backend
    } elseif (-not [string]::IsNullOrWhiteSpace($Config.thumbprint)) {
        $Backend = 'token-csp'
    } elseif (-not [string]::IsNullOrWhiteSpace($Config.pfxPath)) {
        $Backend = 'pfx'
    } else {
        Fail "配置里既没有 thumbprint 也没有 pfxPath，无法判断用哪个后端。"
    }
}

# —— 4) 分发到后端实现 ——
$signerPath = Join-Path (Join-Path $here 'signers') "$Backend.ps1"
if (-not (Test-Path $signerPath)) {
    Fail "后端 '$Backend' 尚未实现：未找到 $signerPath。可用后端见 signers/ 目录。"
}
. $signerPath

Write-Host "sign.ps1: target=$Target backend=$Backend file=$full"

# 不用 switch：Fail 内部会 exit，而 exit 出现在 switch 块里会被解析器判成语法错误
# （Windows PowerShell 5.1 实测报 "switch 语句子句缺少语句块"）。
# 后端函数返回 signtool 的退出码（不依赖 $LASTEXITCODE，因为 Invoke-SignTool 不设置它）。
$signCode = 1
if ($Backend -eq 'token-csp') {
    $signCode = Sign-WithTokenCsp -InputPath $full -Config $Config
} elseif ($Backend -eq 'pfx') {
    $signCode = Sign-WithPfx -InputPath $full -Config $Config
} else {
    Fail "后端 '$Backend' 没有对应的实现分支（请在 sign.ps1 里补一个 elseif）。"
}

if ($signCode -ne 0) {
    Fail "签名失败（后端 $Backend 退出码 $signCode）"
}

# —— 5) 就地验证：签完立刻验，避免"看起来签了、其实链不可用" ——
if ($SkipVerify) {
    Write-Warning "已按 -SkipVerify 跳过 verify（仅限自签名证书的本地测试）"
    exit 0
}
if ($null -eq $script:SignToolPath) {
    Write-Warning "未找到 signtool，跳过签名验证。请自行执行：signtool verify /pa /all `"$full`""
    exit 0
}
$verifyCode = Invoke-SignTool -Arguments @('verify', '/pa', '/all', $full)
if ($verifyCode -ne 0) {
    Fail "签名验证失败（退出码 $verifyCode）：$full"
}
Write-Host "sign.ps1: 已签名并通过 verify：$full"
exit 0

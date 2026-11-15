# Mandaryn 便携包打包脚本
#
# 产物结构（解压后）：
#   mandaryn-<version>-windows-amd64-portable\
#   ├─ mandaryn.exe
#   ├─ sherpa-onnx-c-api.dll / sherpa-onnx-cxx-api.dll / onnxruntime.dll
#   ├─ LICENSE / sherpa-onnx-go-windows-LICENSE / onnxruntime-LICENSE / THIRD-PARTY-NOTICES.md
#   └─ data\README.txt          ← 关键：data\ 的存在触发便携模式
#
# 为什么 data\ 里要放一个 README：便携模式靠「exe 同级存在 data\ 目录」判定
# （internal/paths 的 portableDataRoot）。空的 data\ 在部分解压工具里会被丢弃，
# 导致解压后不是便携模式、数据写进 %AppData%，用户会以为"没生效"。
#
# 用法：
#   powershell -File build/windows/portable.ps1 -Version 2.0.0 -BinDir <bin> -RootDir <repo> -OutFile <zip>

param(
    [Parameter(Mandatory = $true)][string]$Version,
    [Parameter(Mandatory = $true)][string]$BinDir,
    [Parameter(Mandatory = $true)][string]$RootDir,
    [Parameter(Mandatory = $true)][string]$OutFile
)

$ErrorActionPreference = 'Stop'

$name = "mandaryn-$Version-windows-amd64-portable"
# 暂存目录放系统临时区：不在仓库里留中间产物
$stage = Join-Path $env:TEMP ("mandaryn-portable-" + [guid]::NewGuid().ToString('N'))
$payload = Join-Path $stage $name

try {
    New-Item -ItemType Directory -Path $payload -Force | Out-Null

    # 1) 主程序与运行时 DLL（必须同目录：exe 的导入表直接引用 sherpa-onnx-c-api.dll）
    $required = @(
        'mandaryn.exe',
        'sherpa-onnx-c-api.dll',
        'sherpa-onnx-cxx-api.dll',
        'onnxruntime.dll'
    )
    foreach ($f in $required) {
        $src = Join-Path $BinDir $f
        if (-not (Test-Path $src)) {
            throw "缺少 $f（$src）—— 先跑 wails3 task build"
        }
        Copy-Item $src $payload
    }

    # 2) 许可与第三方声明（合规：随二进制分发）
    Copy-Item (Join-Path $RootDir 'LICENSE') $payload
    $thirdParty = Join-Path $RootDir 'build\third-party'
    foreach ($f in @('sherpa-onnx-go-windows-LICENSE', 'onnxruntime-LICENSE', 'THIRD-PARTY-NOTICES.md')) {
        Copy-Item (Join-Path $thirdParty $f) $payload
    }

    # 3) data\：便携模式标记目录
    $dataDir = Join-Path $payload 'data'
    New-Item -ItemType Directory -Path $dataDir -Force | Out-Null
    # 用普通字符串数组拼接而不是 here-string：here-string 的结束标记必须顶格，
    # 在本文件里缩进会被破坏（PowerShell 报 MissingEndCurlyBrace）。
    $readmeLines = @(
        'Mandaryn 便携模式数据目录',
        '==========================',
        '',
        '这个目录存在即代表应用以「便携模式」运行：',
        'dataRoot（数据库、密钥、日志、录音）都会写在这里，而不是 %AppData%\Mandaryn。',
        '',
        '- 想整体搬家：把整个应用目录（含本目录）一起复制走。',
        '- 想改回普通模式：删除本目录，或设置环境变量 MANDARYN_DATA_DIR 指向别处。',
        '- 注意：模型包不在本目录（modelRoot 默认 ~/.mandaryn，可在「设置 → 模型」里改）。',
        '',
        '详细说明见 docs/DEPLOYMENT.md 的「运行时目录与权限」一节。'
    )
    Set-Content -Path (Join-Path $dataDir 'README.txt') -Value $readmeLines -Encoding UTF8

    # 4) 打包：压缩**目录本身**，让 zip 里带顶层文件夹（解压后不会散落一地）
    if (Test-Path $OutFile) { Remove-Item $OutFile -Force }
    Compress-Archive -Path $payload -DestinationPath $OutFile -CompressionLevel Optimal

    $size = [math]::Round((Get-Item $OutFile).Length / 1MB, 1)
    Write-Host "便携包已生成：$OutFile（$size MB）"
}
finally {
    # 只清理本次自己创建的临时暂存目录
    if (Test-Path $stage) { Remove-Item $stage -Recurse -Force -ErrorAction SilentlyContinue }
}

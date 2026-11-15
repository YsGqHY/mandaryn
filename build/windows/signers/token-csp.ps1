# 后端：USB 令牌 / 智能卡里的代码签名证书
#
# 私钥不出设备，因此**不能**导出 .pfx。两种选证书方式：
#   a) 按指纹：signtool /sha1 <thumbprint>（首选，简单）
#   b) 按 CSP + 密钥容器：signtool /csp <provider> /k <container>（a 失败时用）
#
# 配置项（signing.config.psd1）：
#   thumbprint       证书指纹（必填，走 a 方式）
#   csp              私钥提供程序名（选填，走 b 方式时需要）
#   keyContainer     密钥容器名（选填，走 b 方式时需要）
#   timestampUrl     时间戳服务（必填）
#   timestampUrlBackup 备用时间戳（选填）
#   description      签名描述（选填，显示在 UAC/属性里）

function Sign-WithTokenCsp {
    param(
        [Parameter(Mandatory = $true)][string]$InputPath,
        [Parameter(Mandatory = $true)]$Config
    )

    # 复用入口脚本解析出的 signtool（PATH 优先，否则取 SDK 最新版）
    $signtool = $script:SignToolPath
    if ($null -eq $signtool) {
        [Console]::Error.WriteLine("找不到 signtool.exe：请安装 Windows SDK 的 Signing Tools（sign.ps1 会自动在 SDK 目录查找）")
        exit 1
    }

    if ([string]::IsNullOrWhiteSpace($Config.thumbprint)) {
        [Console]::Error.WriteLine("token-csp 后端需要 thumbprint（用 Get-ChildItem Cert:\CurrentUser\My -CodeSigningCert 获取）")
        exit 1
    }

    $ts = $Config.timestampUrl
    if ([string]::IsNullOrWhiteSpace($ts)) {
        [Console]::Error.WriteLine("timestampUrl 不能为空：没有时间戳，证书到期后历史签名会一起失效")
        exit 1
    }

    $args = @(
        'sign',
        '/sha1', $Config.thumbprint,
        '/fd', 'sha256',
        '/td', 'sha256',
        '/tr', $ts
    )
    # b 方式：显式指定 CSP 与密钥容器（令牌驱动把私钥映射到容器）
    if (-not [string]::IsNullOrWhiteSpace($Config.csp)) {
        $args += @('/csp', $Config.csp)
    }
    if (-not [string]::IsNullOrWhiteSpace($Config.keyContainer)) {
        $args += @('/k', $Config.keyContainer)
    }
    if (-not [string]::IsNullOrWhiteSpace($Config.description)) {
        $args += @('/d', $Config.description)
    }
    $args += $InputPath

    Write-Host ("执行：signtool " + ($args -join ' '))
    $code = Invoke-SignTool -Arguments $args

    # 时间戳偶发失败时换备用服务器重试一次（不会重复签名内容，只是补时间戳）
    if ($code -ne 0 -and -not [string]::IsNullOrWhiteSpace($Config.timestampUrlBackup)) {
        Write-Warning "签名/时间戳失败（退出码 $code），改用备用时间戳重试：$($Config.timestampUrlBackup)"
        $retry = $args.Clone()
        for ($i = 0; $i -lt $retry.Count; $i++) {
            if ($retry[$i] -eq '/tr') { $retry[$i + 1] = $Config.timestampUrlBackup }
        }
        $code = Invoke-SignTool -Arguments $retry
    }

    if ($code -ne 0) {
        [Console]::Error.WriteLine(@"
token-csp 签名失败（退出码 $code）。常见原因：
  - 令牌未插入 / 未装驱动       → 运行 certutil -scinfo 确认容器可见
  - PIN 输错或令牌已锁           → 用厂商工具解锁（PUK），不要反复试
  - 证书不在 CurrentUser\My      → 若在 LocalMachine 需改脚本加 /sm
  - 时间戳服务器不可达           → 配 timestampUrlBackup
"@)
    }

    return $code
}

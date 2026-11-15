# 后端：可导出的 .pfx 证书（软件型 / 自签名）
#
# ⚠️ 合规提醒：2023-06-01 起 CA/B Forum 强制私钥硬件化，商业 CA 的新证书**不再**交付
# 可导出 .pfx。本后端只适用于自签名证书或少数旧证书。证书是令牌型时请用 token-csp。
#
# 配置项（signing.config.psd1）：
#   pfxPath             .pfx / .p12 路径（必填）
#   pfxPasswordEnvVar   存放密码的环境变量名（推荐，避免密码进命令行/日志）
#   pfxPassword         密码明文（不推荐：会出现在进程命令行与日志里）
#   timestampUrl / timestampUrlBackup / description 同上

function Sign-WithPfx {
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

    if ([string]::IsNullOrWhiteSpace($Config.pfxPath) -or -not (Test-Path -LiteralPath $Config.pfxPath)) {
        [Console]::Error.WriteLine("pfxPath 无效或文件不存在：$($Config.pfxPath)")
        exit 1
    }

    # 密码优先从环境变量读：CI 里由 Secret 注入，避免写进配置文件与命令行
    $password = ''
    if (-not [string]::IsNullOrWhiteSpace($Config.pfxPasswordEnvVar)) {
        $password = [Environment]::GetEnvironmentVariable($Config.pfxPasswordEnvVar)
        if ([string]::IsNullOrEmpty($password)) {
            [Console]::Error.WriteLine("环境变量 $($Config.pfxPasswordEnvVar) 为空：请在 CI Secret 或本地环境里设置 PFX 密码")
            exit 1
        }
    } elseif (-not [string]::IsNullOrWhiteSpace($Config.pfxPassword)) {
        Write-Warning "使用配置文件里的明文 pfxPassword：建议改为 pfxPasswordEnvVar + 环境变量"
        $password = $Config.pfxPassword
    }

    $ts = $Config.timestampUrl
    if ([string]::IsNullOrWhiteSpace($ts)) {
        [Console]::Error.WriteLine("timestampUrl 不能为空：没有时间戳，证书到期后历史签名会一起失效")
        exit 1
    }

    $args = @('sign', '/f', $Config.pfxPath)
    if (-not [string]::IsNullOrEmpty($password)) {
        $args += @('/p', $password)
    }
    $args += @('/fd', 'sha256', '/td', 'sha256', '/tr', $ts)
    if (-not [string]::IsNullOrWhiteSpace($Config.description)) {
        $args += @('/d', $Config.description)
    }
    $args += $InputPath

    # 不打印含密码的命令行
    Write-Host "执行：signtool sign /f $($Config.pfxPath) /p *** /fd sha256 /td sha256 /tr $ts $InputPath"
    $code = Invoke-SignTool -Arguments $args

    if ($code -ne 0 -and -not [string]::IsNullOrWhiteSpace($Config.timestampUrlBackup)) {
        Write-Warning "签名失败（退出码 $code），改用备用时间戳重试：$($Config.timestampUrlBackup)"
        $retry = $args.Clone()
        for ($i = 0; $i -lt $retry.Count; $i++) {
            if ($retry[$i] -eq '/tr') { $retry[$i + 1] = $Config.timestampUrlBackup }
        }
        $code = Invoke-SignTool -Arguments $retry
    }

    if ($code -ne 0) {
        [Console]::Error.WriteLine(@"
pfx 签名失败（退出码 $code）。常见原因：
  - PFX 密码错误                 → 核对 pfxPasswordEnvVar 指向的值
  - PFX 不含私钥                 → 重新导出（勾选"导出私钥"）
  - 证书链不全                   → 导出时勾选"包含所有证书"或用 /ac 附上中间 CA
  - 时间戳服务器不可达           → 配 timestampUrlBackup
"@)
    }

    return $code
}

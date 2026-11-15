# 签名配置模板 —— 复制为 signing.config.psd1 后填写真实值。
#
#   Copy-Item build/windows/signing.config.example.psd1 build/windows/signing.config.psd1
#
# ⚠️ signing.config.psd1 **不入库**（已在 .gitignore 里）。里面不放密码：
# 密码走环境变量（pfxPasswordEnvVar），CI 里由 Secret 注入。
#
# backend 取值：
#   token-csp      USB 令牌 / 智能卡（私钥不出设备）—— 需要 thumbprint
#   pfx            可导出 .pfx（仅自签名或旧证书）—— 需要 pfxPath
#   cloud-esigner  预留：云签名服务（SSL.com eSigner 等），尚未实现
@{
    # auto 时会按 thumbprint → pfxPath 的顺序自动判断
    backend            = 'auto'

    # —— 证书选择（token-csp 用）——
    # 获取：Get-ChildItem Cert:\CurrentUser\My -CodeSigningCert | Select-Object Thumbprint, Subject
    thumbprint         = ''
    # 可选：/sha1 选不中时，用 CSP + 密钥容器显式指定
    # 获取：$c.PrivateKey.CspKeyContainerInfo.ProviderName / .KeyContainerName
    csp                = ''
    keyContainer       = ''

    # —— 证书文件（pfx 用）——
    pfxPath            = ''
    # 推荐：密码放环境变量，不要写 pfxPassword
    pfxPasswordEnvVar  = 'MANDARYN_PFX_PASSWORD'
    pfxPassword        = ''

    # —— 时间戳（必填）——
    # 没有时间戳，证书到期后所有历史版本的签名会一起失效
    timestampUrl       = 'http://timestamp.digicert.com'
    timestampUrlBackup = 'http://timestamp.sectigo.com'

    # —— 签名描述（可选，显示在 UAC / 文件属性里）——
    description        = 'Mandaryn'

    # —— 预留：云签名后端参数（cloud-esigner 实现后启用）——
    # cloudProvider    = 'ssl-esigner'
    # cloudEndpoint    = ''
    # cloudCredentialId = ''
    # cloudTotpEnvVar  = 'MANDARYN_ESIGNER_TOTP'
}

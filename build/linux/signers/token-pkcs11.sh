# 签名后端：USB 令牌 / 智能卡（PKCS#11）—— Ubuntu
#
# 私钥不出设备，因此不能导出 .pfx。用 osslsigncode 通过 PKCS#11 调用令牌。
#
# 需要由 sign.sh source 后调用：sign_with_token_pkcs11 <绝对路径> <exe|installer|msi>
#
# 配置项（signing.config）：
#   PKCS11_MODULE    PKCS#11 库路径（必填）
#                     OpenSC：/usr/lib/x86_64-linux-gnu/opensc-pkcs11.so
#                     eToken：/usr/lib/libeTPkcs11.so（厂商客户端自带）
#                     YubiKey：/usr/lib/x86_64-linux-gnu/libykcs11.so
#   PKCS11_URI       证书/私钥的 PKCS#11 URI（必填），形如
#                     pkcs11:token=MandarynCS;object=Code%20Signing
#                     用 `pkcs11-tool --module "$PKCS11_MODULE" -O --type cert` 查
#                     label 里的空格要写成 %20
#   PIN_FILE         PIN 文件路径（必填，权限 600）。**不要**把 PIN 写进本文件。
#   TIMESTAMP_URL        RFC3161 时间戳（必填；没有它证书到期后历史签名一起失效）
#   TIMESTAMP_URL_BACKUP 备用时间戳（可选；osslsigncode 没有重试参数，靠这里兜）
#   SIGN_DESCRIPTION     签名描述（可选，显示在文件属性里）
#   PKCS11_PROVIDER  OpenSSL 3 provider 路径（可选，缺省走 engine 路线）
#   PKCS11_ENGINE    OpenSSL engine 路径（可选，默认 .../engines-3/pkcs11.so）

sign_with_token_pkcs11() {
    local input="$1"
    local target="$2"

    command -v osslsigncode >/dev/null 2>&1 || {
        printf 'token-pkcs11: 找不到 osslsigncode。安装：sudo apt install osslsigncode\n' >&2
        return 1
    }
    [ -n "${PKCS11_MODULE:-}" ] || { printf 'token-pkcs11: 配置缺少 PKCS11_MODULE\n' >&2; return 1; }
    [ -n "${PKCS11_URI:-}" ]    || { printf 'token-pkcs11: 配置缺少 PKCS11_URI\n' >&2; return 1; }
    [ -n "${PIN_FILE:-}" ]      || { printf 'token-pkcs11: 配置缺少 PIN_FILE（PIN 不要写进配置）\n' >&2; return 1; }
    [ -f "$PIN_FILE" ]          || { printf 'token-pkcs11: PIN_FILE 不存在：%s\n' "$PIN_FILE" >&2; return 1; }
    [ -n "${TIMESTAMP_URL:-}" ] || { printf 'token-pkcs11: 配置缺少 TIMESTAMP_URL（没有时间戳，证书到期后历史签名会失效）\n' >&2; return 1; }

    local ts="$TIMESTAMP_URL"
    # 用临时**目录**而不是 mktemp 建文件：osslsigncode 的 man page 明确写着
    # "Output files are not overwritten."，而 mktemp 会把文件先建出来（哪怕 0 字节），
    # 传给 -out 就会被判定为已存在而失败。目录 + 不存在的文件名才正确。
    local tmpdir; tmpdir="$(mktemp -d "${TMPDIR:-/tmp}/mandaryn-sign.XXXXXX")"
    local out="$tmpdir/signed"
    local desc="${SIGN_DESCRIPTION:-Mandaryn}"

    # 证书/私钥用同一个 URI，靠 type= 区分
    local cert_uri="${PKCS11_URI};type=cert"
    local key_uri="${PKCS11_URI};type=private"

    do_sign() {
        local tsurl="$1"
        local -a args=(
            sign
            -pkcs11module "$PKCS11_MODULE"
            -pkcs11cert "$cert_uri"
            -key "$key_uri"
            -readpass "$PIN_FILE"
            -h sha256
            -n "$desc"
            -ts "$tsurl"
            -in "$input"
            -out "$out"
        )
        # MSI 必须加 -add-msi-dse：让签名覆盖 MSI 元数据，而不只是文件内容
        if [ "$target" = "msi" ]; then
            args+=(-add-msi-dse)
        fi
        # 两条令牌路线：配了 PKCS11_PROVIDER 走 OpenSSL 3 provider，否则走 engine。
        #
        # 参数名是 -pkcs11engine 而**不是** -engine/-login：osslsigncode 2.8（Ubuntu
        # 24.04 源内版本）只认 -pkcs11engine，传 -engine/-login 会直接 "Unknown option"。
        #
        # 另注（实测，Ubuntu 24.04 + osslsigncode 2.8 + libengine-pkcs11-openssl
        # 0.4.12 + OpenSSL 3.0）：engine 路线即使参数正确，真实签名阶段仍会
        # segfault（exit 139），且 24.04 官方源没有 pkcs11 provider 包
        # （ossl-modules 里只有 legacy.so）。因此 **Ubuntu 24.04 上请优先用 jsign
        # 后端**（signers/jsign.sh，走 SunPKCS11，不依赖 OpenSSL engine）。
        if [ -n "${PKCS11_PROVIDER:-}" ]; then
            args+=(-provider "$PKCS11_PROVIDER")
        else
            args+=(-pkcs11engine "${PKCS11_ENGINE:-/usr/lib/x86_64-linux-gnu/engines-3/pkcs11.so}")
        fi
        osslsigncode "${args[@]}"
    }

    printf 'token-pkcs11: 签名 %s（时间戳 %s）\n' "$input" "$ts"
    if ! do_sign "$ts"; then
        if [ -n "${TIMESTAMP_URL_BACKUP:-}" ]; then
            printf 'token-pkcs11: 首次失败，改用备用时间戳重试：%s\n' "$TIMESTAMP_URL_BACKUP" >&2
            do_sign "$TIMESTAMP_URL_BACKUP" || {
                rm -f "$out"
                printf 'token-pkcs11: 签名失败。常见原因：令牌未插/pcscd 未跑（pcsc_scan）、PIN 错（注意会锁卡）、URI 与 pkcs11-tool -O 的 label 不一致、时间戳不可达、缺中间 CA（加 -ac）\n' >&2
                printf 'token-pkcs11: 若在 Ubuntu 24.04 上（osslsigncode 2.8），engine 路线可能 segfault，请改用 jsign 后端：CONFIG_BACKEND="jsign"\n' >&2
                return 1
            }
        else
            rm -f "$out"
            printf 'token-pkcs11: 签名失败（未配置 TIMESTAMP_URL_BACKUP，不重试）\n' >&2
            return 1
        fi
    fi

    # osslsigncode 用 -out 写新文件（不原地改），这里替换回去
    mv -f "$out" "$input"
    printf 'token-pkcs11: 完成 %s\n' "$input"
}

# 签名后端：jsign（Java，令牌兼容性更好）
#
# 什么时候用它：osslsigncode 认不出你的令牌时（厂商中间件非标准，尤其 SafeNet eToken）。
#
# 需要由 sign.sh source 后调用：sign_with_jsign <绝对路径> <exe|installer|msi>
#
# 配置项（signing.config）：
#   JSIGN_KEYSTORE   SunPKCS11 配置文件路径（必填），内容形如
#                      name = MandarynCS
#                      library = /usr/lib/x86_64-linux-gnu/opensc-pkcs11.so
#                      slot = 0
#                    也可以直接写内联配置（含 \n），见 jsign 文档
#   JSIGN_ALIAS      证书别名（必填），如 "Code Signing"
#   PIN_FILE         PIN 文件路径（必填）。通过 --storepass file: 读取，不进命令行
#   TIMESTAMP_URL    RFC3161 时间戳（必填）
#   SIGN_DESCRIPTION 签名描述（可选）
#   JSIGN_STORETYPE  可选。默认 PKCS11；厂商专用可填 ETOKEN / OPENSC / YUBIKEY / PIV

sign_with_jsign() {
    local input="$1"
    local target="$2"

    command -v jsign >/dev/null 2>&1 || {
        printf 'jsign: 找不到 jsign。安装：sudo apt install -y openjdk-17-jre-headless && sudo dpkg -i jsign_<ver>_all.deb\n' >&2
        return 1
    }
    [ -n "${JSIGN_KEYSTORE:-}" ] || { printf 'jsign: 配置缺少 JSIGN_KEYSTORE\n' >&2; return 1; }
    [ -n "${JSIGN_ALIAS:-}" ]    || { printf 'jsign: 配置缺少 JSIGN_ALIAS\n' >&2; return 1; }
    [ -n "${PIN_FILE:-}" ]       || { printf 'jsign: 配置缺少 PIN_FILE\n' >&2; return 1; }
    [ -f "$PIN_FILE" ]           || { printf 'jsign: PIN_FILE 不存在：%s\n' "$PIN_FILE" >&2; return 1; }
    [ -n "${TIMESTAMP_URL:-}" ]  || { printf 'jsign: 配置缺少 TIMESTAMP_URL\n' >&2; return 1; }

    local -a args=(
        --storetype "${JSIGN_STORETYPE:-PKCS11}"
        --keystore "$JSIGN_KEYSTORE"
        --alias "$JSIGN_ALIAS"
        # PIN 用 file: 前缀读文件，避免出现在进程命令行（ps 可见）
        --storepass "file:$PIN_FILE"
        --tsaurl "$TIMESTAMP_URL"
        # jsign 默认是旧的 Authenticode 时间戳，要 RFC3161 必须显式指定
        --tsmode RFC3161
        # osslsigncode 没有重试参数，jsign 有 —— 网络抖动时更省事
        --tsretries 5
        --tsretrywait 15
    )
    if [ -n "${SIGN_DESCRIPTION:-}" ]; then
        args+=(--name "$SIGN_DESCRIPTION")
    fi
    # jsign 默认 --replace=false，即重复签名会**叠加**嵌套签名；而 osslsigncode 的
    # sign 默认是替换（`-nest` 才叠加）。统一成替换，否则同一产物被签两次时两个后端
    # 结果不同，且 osslsigncode verify 只看主签名 → 叠加会被静默放过。
    args+=(--replace)
    # jsign 是就地签名（没有 --output），MSI 也原生支持，不需要额外开关

    printf 'jsign: 签名 %s（storetype=%s, 时间戳 %s）\n' "$input" "${JSIGN_STORETYPE:-PKCS11}" "$TIMESTAMP_URL"
    if ! jsign "${args[@]}" "$input"; then
        printf 'jsign: 签名失败。常见原因：PCSC 服务未跑（pcsc_scan）、PIN 错（会锁卡）、别名与 pkcs11-tool -O 的 label 不一致、时间戳不可达、pkcs11.cfg 里 library 路径错\n' >&2
        return 1
    fi
    printf 'jsign: 完成 %s\n' "$input"
}

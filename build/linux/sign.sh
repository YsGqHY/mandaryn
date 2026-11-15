#!/usr/bin/env bash
# Mandaryn 签名统一入口（Linux / Ubuntu 签名服务器）
#
# 与 build/windows/sign.ps1 **同一套契约**，换平台不用换调用方式：
#
#   build/linux/sign.sh --target exe       --input bin/mandaryn.exe
#   build/linux/sign.sh --target installer --input bin/mandaryn-amd64-installer.exe
#   build/linux/sign.sh --target msi       --input bin/mandaryn-amd64.msi
#
# 契约：
#   - 成功 → 退出码 0，且**原地**签名（不产出副本、不改文件名）
#   - 失败 → 非 0 退出码，原因写 stderr（CI 会因此变红）
#   - 后端由 signing.config 的 BACKEND 决定，--backend 可临时覆盖
#
# 配置文件查找顺序：--config → $MANDARYN_SIGNING_CONFIG → build/linux/signing.config
# 真实配置**不入库**（见 .gitignore），模板见 signing.config.example。
#
# 为什么不照搬 Windows 的 signtool：signtool 是 Windows SDK 组件、没有 Linux 版本；
# 用 Wine 跑 Windows SDK 也不可靠（osslsigncode 作者明确说过 signtool 在 Wine 下
# 因重度依赖 Windows CryptoAPI 而失败）。Linux 上改用 osslsigncode / jsign。

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

TARGET=""
INPUT=""
BACKEND=""
CONFIG=""
SKIP_VERIFY=0
PREFLIGHT=0

die() {
    printf 'sign.sh: %s\n' "$1" >&2
    exit 1
}

usage() {
    cat <<'USAGE'
用法: build/linux/sign.sh --target <exe|installer|msi> --input <文件> [选项]

选项:
  --target <类型>   必填。exe | installer | msi
  --input <路径>    必填。待签名文件（会被原地签名）
  --backend <名称>  可选。token-pkcs11 | jsign；缺省读配置
  --config <路径>   可选。签名配置；缺省读 $MANDARYN_SIGNING_CONFIG 或 signing.config
  --preflight       只做环境预检（令牌/证书/私钥/时间戳/配置完整性），不签名
  --skip-verify     仅供自签名证书的本地测试；CI 不要用
  -h, --help        显示本帮助
USAGE
}

while [ $# -gt 0 ]; do
    case "$1" in
        --target)   TARGET="${2:-}"; shift 2 ;;
        --input)    INPUT="${2:-}"; shift 2 ;;
        --backend)  BACKEND="${2:-}"; shift 2 ;;
        --config)   CONFIG="${2:-}"; shift 2 ;;
        --preflight) PREFLIGHT=1; shift ;;
        --skip-verify) SKIP_VERIFY=1; shift ;;
        -h|--help)  usage; exit 0 ;;
        *)          die "未知参数：$1（--help 查看用法）" ;;
    esac
done

# —— 0) 预检模式：不签名，只回答"这台机器现在能不能签" ——
# 为什么单独一条路径：真正的失败（pcscd 没跑 / 私钥不在卡里 / URI 写错）在签名时
# 只表现为一句笼统的报错，排查成本高。预检把每一步拆开、各自给结论。
if [ "$PREFLIGHT" -eq 1 ]; then
    rc=0
    say() { printf '%-22s %s\n' "$1" "$2"; }

    # 配置定位（预检也要求配置，否则后面几条无从判断）
    if [ -z "$CONFIG" ]; then
        if [ -n "${MANDARYN_SIGNING_CONFIG:-}" ]; then
            CONFIG="$MANDARYN_SIGNING_CONFIG"
        elif [ -f "$HERE/signing.config" ]; then
            CONFIG="$HERE/signing.config"
        fi
    fi
    if [ -z "$CONFIG" ] || [ ! -f "$CONFIG" ]; then
        say "配置文件" "✗ 未找到（复制 signing.config.example 为 signing.config）"
        exit 1
    fi
    say "配置文件" "✓ $CONFIG"
    # shellcheck disable=SC1090
    . "$CONFIG"

    # pcscd
    if ! command -v systemctl >/dev/null 2>&1; then
        say "pcscd" "? 没有 systemctl（非 systemd 环境），请手工确认 pcscd 在跑"
    elif systemctl is-active --quiet pcscd; then
        say "pcscd" "✓ running"
    else
        say "pcscd" "✗ 未运行 → sudo systemctl enable --now pcscd"
        rc=1
    fi

    # 读卡器与卡片
    if command -v pcsc_scan >/dev/null 2>&1; then
        if pcsc_scan -r 2>/dev/null | grep -qi "card"; then
            say "读卡器/卡片" "✓ 已识别"
        else
            say "读卡器/卡片" "✗ 未识别 → 检查 USB、libccid、journalctl -u pcscd"
            rc=1
        fi
    else
        say "读卡器/卡片" "? 未装 pcsc-tools（apt install pcsc-tools）"
    fi

    # PKCS#11 库与令牌
    if [ -z "${PKCS11_MODULE:-}" ]; then
        say "PKCS11_MODULE" "✗ 配置里为空 → dpkg -L opensc | grep pkcs11"
        rc=1
    elif [ ! -f "$PKCS11_MODULE" ]; then
        say "PKCS11_MODULE" "✗ 文件不存在：$PKCS11_MODULE"
        rc=1
    else
        say "PKCS11_MODULE" "✓ $PKCS11_MODULE"
    fi

    # PIN 文件
    if [ -z "${PIN_FILE:-}" ] || [ ! -f "${PIN_FILE:-}" ]; then
        say "PIN_FILE" "✗ 未配置或不存在（PIN 不要写进 signing.config）"
        rc=1
    elif [ "$(stat -c '%a' "$PIN_FILE" 2>/dev/null)" != "600" ]; then
        say "PIN_FILE" "⚠ 存在但权限不是 600 → chmod 600 $PIN_FILE"
    else
        say "PIN_FILE" "✓ $PIN_FILE（600）"
    fi

    # 证书与私钥（这是最关键的一条：只有 cert 没有 privkey 时签不了）
    # PIN 必须用 env: 前缀传给 pkcs11-tool —— 它只认 env:VARIABLE，
    # **不支持** file:（会把 file:/path 当成字面 PIN，登录失败后看起来像"证书不存在"）。
    # 用子 shell + export，PIN 不落到命令行（ps 不可见）。
    if command -v pkcs11-tool >/dev/null 2>&1 && [ -n "${PIN_FILE:-}" ] && [ -f "${PIN_FILE:-}" ]; then
        if (
            export MANDARYN_PKCS11_PIN="$(cat "$PIN_FILE")"
            pkcs11-tool --module "${PKCS11_MODULE:-}" --pin env:MANDARYN_PKCS11_PIN -O --type cert
        ) 2>/dev/null | grep -q "Certificate Object"; then
            say "证书对象" "✓ 找到"
        else
            say "证书对象" "✗ 未找到 → 用 pkcs11-tool -O --type cert 查看 label，确认 PKCS11_URI"
            rc=1
        fi
        if (
            export MANDARYN_PKCS11_PIN="$(cat "$PIN_FILE")"
            pkcs11-tool --module "${PKCS11_MODULE:-}" --pin env:MANDARYN_PKCS11_PIN -O --type privkey
        ) 2>/dev/null | grep -q "Private Key Object"; then
            say "私钥对象" "✓ 在令牌内（私钥不出设备）"
        else
            say "私钥对象" "✗ 未找到 → 私钥不在卡里，需要重新导入"
            rc=1
        fi
    else
        say "证书/私钥对象" "? 未装 opensc（没有 pkcs11-tool），跳过"
    fi

    # 时间戳
    if [ -z "${TIMESTAMP_URL:-}" ]; then
        say "TIMESTAMP_URL" "✗ 为空（没有时间戳，证书到期后历史签名会失效）"
        rc=1
    elif command -v curl >/dev/null 2>&1; then
        if curl -sfI --max-time 10 "$TIMESTAMP_URL" >/dev/null 2>&1; then
            say "TIMESTAMP_URL" "✓ 可达：$TIMESTAMP_URL"
        else
            say "TIMESTAMP_URL" "⚠ 不可达：$TIMESTAMP_URL（签名会失败，配 TIMESTAMP_URL_BACKUP）"
        fi
    else
        say "TIMESTAMP_URL" "? 没有 curl，跳过连通性检查"
    fi

    # 工具
    for tool in osslsigncode jsign; do
        if command -v "$tool" >/dev/null 2>&1; then
            say "工具 $tool" "✓ $(command -v "$tool")"
        else
            say "工具 $tool" "? 未安装"
        fi
    done

    if [ "$rc" -eq 0 ]; then
        printf '\n预检通过：可以执行 build/linux/sign.sh --target exe --input <文件>\n'
    else
        printf '\n预检未通过：先处理上面标 ✗ 的项，再签名。\n'
    fi
    exit "$rc"
fi

case "$TARGET" in
    exe|installer|msi) ;;
    "") die "缺少 --target（只想检查环境请用 --preflight）" ;;
    *)  die "--target 只能是 exe / installer / msi，收到：$TARGET" ;;
esac

[ -n "$INPUT" ] || die "缺少 --input"
[ -f "$INPUT" ] || die "待签名文件不存在：$INPUT"
case "$INPUT" in /*) INPUT_ABS="$INPUT" ;; *) INPUT_ABS="$(cd "$(dirname "$INPUT")" && pwd)/$(basename "$INPUT")" ;; esac

# —— 1) 定位配置 ——
if [ -z "$CONFIG" ]; then
    if [ -n "${MANDARYN_SIGNING_CONFIG:-}" ]; then
        CONFIG="$MANDARYN_SIGNING_CONFIG"
    elif [ -f "$HERE/signing.config" ]; then
        CONFIG="$HERE/signing.config"
    fi
fi
[ -n "$CONFIG" ] && [ -f "$CONFIG" ] || die "找不到签名配置。请复制 signing.config.example 为 signing.config 并填写（该文件不入库）。"

# 配置是 shell 变量赋值文件（KEY=value），source 进来即可。
# 注意：配置里用 CONFIG_BACKEND 而不是 BACKEND —— 后者可能来自 --backend，
# source 时不能把它覆盖掉（命令行优先）。
# shellcheck disable=SC1090
. "$CONFIG"

[ -n "${BACKEND:-}" ] || BACKEND="${CONFIG_BACKEND:-}"
if [ -z "$BACKEND" ]; then
    if [ -n "${PKCS11_MODULE:-}" ] && [ -n "${PKCS11_URI:-}" ]; then
        BACKEND="token-pkcs11"
    elif [ -n "${JSIGN_KEYSTORE:-}" ]; then
        BACKEND="jsign"
    else
        die "配置里既没有 PKCS11_MODULE/PKCS11_URI 也没有 JSIGN_KEYSTORE，无法判断用哪个后端。"
    fi
fi

# —— 2) 分发到后端实现 ——
SIGNER="$HERE/signers/$BACKEND.sh"
[ -f "$SIGNER" ] || die "后端 '$BACKEND' 尚未实现：未找到 $SIGNER（可用后端见 signers/ 目录）"
# shellcheck disable=SC1090
. "$SIGNER"

printf 'sign.sh: target=%s backend=%s file=%s\n' "$TARGET" "$BACKEND" "$INPUT_ABS"

case "$BACKEND" in
    token-pkcs11) sign_with_token_pkcs11 "$INPUT_ABS" "$TARGET" ;;
    jsign)        sign_with_jsign "$INPUT_ABS" "$TARGET" ;;
    *)            die "后端 '$BACKEND' 没有对应的实现分支（请在 sign.sh 的 case 里补一行）。" ;;
esac

# —— 3) 验签：签完立刻验，避免"看起来签了、其实链不可用" ——
if [ "$SKIP_VERIFY" -eq 1 ]; then
    printf 'sign.sh: 已按 --skip-verify 跳过验签（仅限自签名证书的本地测试）\n' >&2
    exit 0
fi
command -v osslsigncode >/dev/null 2>&1 || {
    printf 'sign.sh: 警告：未找到 osslsigncode，跳过验签。请自行执行：osslsigncode verify -in %s\n' "$INPUT_ABS" >&2
    exit 0
}
# 这里刻意做**完整**验签（含时间戳链），与 CI 发布前那步不同：签名机上装了
# 完整 CA/TSA 证书，能给出最严格结论；CI 那步只证明"字节没丢签名"，所以用
# -ignore-timestamp 避免因 TSA 链不在容器里而误判。
VERIFY_LOG="$(mktemp "${TMPDIR:-/tmp}/mandaryn-verify.XXXXXX")"
if ! osslsigncode verify -in "$INPUT_ABS" >"$VERIFY_LOG" 2>&1; then
    printf 'sign.sh: 验签失败：%s\n' "$INPUT_ABS" >&2
    sed -n '1,20p' "$VERIFY_LOG" >&2
    rm -f "$VERIFY_LOG"
    exit 1
fi
rm -f "$VERIFY_LOG"
printf 'sign.sh: 已签名并通过验签：%s\n' "$INPUT_ABS"
exit 0

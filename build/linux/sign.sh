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
        --skip-verify) SKIP_VERIFY=1; shift ;;
        -h|--help)  usage; exit 0 ;;
        *)          die "未知参数：$1（--help 查看用法）" ;;
    esac
done

case "$TARGET" in
    exe|installer|msi) ;;
    "") die "缺少 --target" ;;
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
if ! osslsigncode verify -in "$INPUT_ABS" >/tmp/sign-verify.log 2>&1; then
    printf 'sign.sh: 验签失败：%s\n' "$INPUT_ABS" >&2
    sed -n '1,20p' /tmp/sign-verify.log >&2
    exit 1
fi
printf 'sign.sh: 已签名并通过验签：%s\n' "$INPUT_ABS"
exit 0

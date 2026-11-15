#!/usr/bin/env bash
# 把仓库根的 LICENSE（纯文本）转成 MSI 许可页要求的 RTF。
#
# 为什么需要这一步：WiX 的 WixUILicenseRtf 只接受 RTF，喂纯文本会在构建期报错。
# 为什么用脚本而不是手工维护两个文件：许可正文只能有一处来源，否则改了一处忘了
# 另一处，就会出现"安装界面显示的许可与实际 LICENSE 不一致"——那是合规问题。
#
# 输出：build/windows/msi/LICENSE.rtf（生成物，随源码一起提交，便于 CI 免依赖生成）
#
# 用法：bash build/windows/msi/make-license-rtf.sh

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
SRC="$ROOT/LICENSE"
OUT="$HERE/LICENSE.rtf"

[ -f "$SRC" ] || { echo "找不到 LICENSE：$SRC" >&2; exit 1; }

# 纯 ASCII 的 LICENSE 不需要 python；只有出现非 ASCII 字符（如版权符号）才需要它
# 转成 \uN? 形式。CI 上若没有 python 而文本又是纯 ASCII，不应因此失败。
if ! grep -qP '[^\x00-\x7F]' "$SRC" 2>/dev/null && ! grep -q '[^ -~	]' "$SRC" 2>/dev/null; then
    NEED_PY=0
else
    NEED_PY=1
    command -v python >/dev/null 2>&1 || {
        if [ -f "$OUT" ]; then
            echo "警告：LICENSE 含非 ASCII 字符但本机没有 python，保留已有 $OUT 不重新生成。" >&2
            exit 0
        fi
        echo "LICENSE 含非 ASCII 字符，需要 python 转义为 RTF：未找到 python" >&2
        exit 1
    }
fi

# RTF 转义：反斜杠与花括号是 RTF 的控制字符。
# 非 ASCII 需要 \uN?（RTF 是 7-bit 的），这一步用 python；纯 ASCII 时只用 sed。
escape() {
    sed -e 's/\\/\\\\/g' \
        -e 's/{/\\{/g' \
        -e 's/}/\\}/g' \
        -e 's/\t/\\tab /g' |
    if [ "$NEED_PY" = "1" ]; then
        python -c "
import sys
for ch in sys.stdin.read():
    o = ord(ch)
    if o < 128:
        sys.stdout.write(ch)
    else:
        n = o if o < 32768 else o - 65536
        sys.stdout.write('\\\\u%d?' % n)
"
    else
        cat
    fi
}

{
    printf '{\\rtf1\\ansi\\ansicpg1252\\deff0{\\fonttbl{\\f0\\fswiss Helvetica;}}\n'
    printf '\\f0\\fs20\n'
    # 每行一个 \par；空行也保留，维持原文段落结构
    while IFS= read -r line || [ -n "$line" ]; do
        if [ -z "$line" ]; then
            printf '\\par\n'
        else
            printf '%s\\par\n' "$(printf '%s' "$line" | escape)"
        fi
    done < "$SRC"
    printf '}\n'
} > "$OUT"

echo "已生成 $OUT（来源 $SRC）"

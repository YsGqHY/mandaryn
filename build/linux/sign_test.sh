#!/usr/bin/env bash
# sign.sh 的后端 argv 自测（不需要令牌、不需要 osslsigncode）
#
# 目的：在没有硬件令牌的机器上（含 Windows/Git Bash、CI）验证 sign.sh 生成的
# osslsigncode 命令行**精确正确** —— 令牌签名最容易出错的地方不是密码，而是：
#   - MSI 忘了 -add-msi-dse（签了但元数据没覆盖）
#   - provider / engine 两条路线选错
#   - PIN 走进了命令行（ps 可见）
#   - 备用时间戳回退没生效
#
# 做法：在 PATH 最前面放一个假 osslsigncode，它把 argv 原样记下来并建出 -out 文件，
# 然后断言 argv 内容。sign.sh 的行为与真实环境完全一致（它只依赖命令存在与否）。
#
# 用法：bash build/linux/sign_test.sh

set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SIGN="$HERE/sign.sh"

WORK="$(mktemp -d "${TMPDIR:-/tmp}/sign-test-XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

PASS=0
FAIL=0
ok()   { PASS=$((PASS + 1)); printf '  ✓ %s\n' "$1"; }
bad()  { FAIL=$((FAIL + 1)); printf '  ✗ %s\n' "$1"; }
check() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }

# —— 假 osslsigncode：记录 argv，按 -out 建文件，可注入失败 ——
STUB_DIR="$WORK/bin"
mkdir -p "$STUB_DIR"
cat > "$STUB_DIR/osslsigncode" <<'STUB'
#!/usr/bin/env bash
# 记录调用（每次一行，参数用 \x1f 分隔便于断言）
{
  printf '%s' "$*"
  printf '\n'
} >> "$STUB_LOG"

# 让失败注入生效：STUB_FAIL=1 时**只有第一次** sign 子命令失败，
# 这样才测得到"首次失败 → 备用时间戳重试 → 最终成功"这条路径。
if [ "${1:-}" = "sign" ]; then
  COUNT_FILE="${STUB_LOG}.signcount"
  n=0
  [ -f "$COUNT_FILE" ] && n="$(cat "$COUNT_FILE")"
  n=$((n + 1))
  printf '%s' "$n" > "$COUNT_FILE"
  if [ "${STUB_FAIL:-0}" = "1" ] && [ "$n" -eq 1 ]; then
    exit 1
  fi
fi

# sign 需要产出 -out 指定文件（sign.sh 之后会 mv 回原文件）
prev=""
for a in "$@"; do
  if [ "$prev" = "-out" ]; then
    : > "$a"
  fi
  prev="$a"
done
exit 0
STUB
chmod +x "$STUB_DIR/osslsigncode"
export PATH="$STUB_DIR:$PATH"
export STUB_LOG="$WORK/calls.log"

# —— 假 PIN 与假待签名文件 ——
PIN="$WORK/pin"
printf '12345678' > "$PIN"
chmod 600 "$PIN"

mkcfg() {
  # ${1-} 而不是 $1：set -u 下无参数时 $1 未绑定会直接报错（本测试踩到过）。
  local name="${1-cfg}"
  local cfg="$WORK/$name.config"
  {
    printf 'CONFIG_BACKEND="token-pkcs11"\n'
    printf 'PKCS11_MODULE="/usr/lib/x86_64-linux-gnu/opensc-pkcs11.so"\n'
    printf 'PKCS11_URI="pkcs11:token=TestToken;object=Code%%20Signing"\n'
    printf 'PIN_FILE="%s"\n' "$PIN"
    printf 'TIMESTAMP_URL="http://ts-primary.example"\n'
    printf 'TIMESTAMP_URL_BACKUP="http://ts-backup.example"\n'
    printf 'SIGN_DESCRIPTION="Mandaryn Test"\n'
    # 追加的额外配置行（可为空）。用 ${2:-} 而不是 "$@"：set -u 下无参数时 $1 未绑定。
    if [ $# -gt 1 ]; then
      printf '%s\n' "$2"
    fi
  } > "$cfg"
  printf '%s' "$cfg"
}

mkinput() {
  local f="$WORK/$1"
  printf 'FAKE-PE-CONTENT' > "$f"
  printf '%s' "$f"
}

reset_log() { : > "$STUB_LOG"; rm -f "$STUB_LOG.signcount"; }
last_call() { tail -1 "$STUB_LOG"; }
call_count() { wc -l < "$STUB_LOG" | tr -d ' '; }

# ─────────────────────────────────────────────────────────────
echo "1) exe + engine 路线（默认，未配 PKCS11_PROVIDER）"
cfg="$(mkcfg)"; in="$(mkinput a.exe)"; reset_log
"$SIGN" --target exe --input "$in" --config "$cfg" --skip-verify >/dev/null 2>&1
call="$(last_call)"
check "调用了 osslsigncode sign"        '[[ "$call" == sign* ]]'
check "带 -engine 与 -login"            '[[ "$call" == *"-engine"* && "$call" == *"-login"* ]]'
check "未带 -provider"                  '[[ "$call" != *"-provider"* ]]'
check "带 -pkcs11module"                '[[ "$call" == *"-pkcs11module /usr/lib/x86_64-linux-gnu/opensc-pkcs11.so"* ]]'
check "证书 URI 带 type=cert"           '[[ "$call" == *"pkcs11:token=TestToken;object=Code%20Signing;type=cert"* ]]'
check "私钥 URI 带 type=private"        '[[ "$call" == *";type=private"* ]]'
check "PIN 走 -readpass 文件而非 -pass" '[[ "$call" == *"-readpass $PIN"* && "$call" != *" -pass "* ]]'
check "sha256 + RFC3161 时间戳"         '[[ "$call" == *"-h sha256"* && "$call" == *"-ts http://ts-primary.example"* ]]'
check "**不带** -add-msi-dse（非 MSI）"  '[[ "$call" != *"-add-msi-dse"* ]]'
check "就地签名（原文件仍在）"           '[ -f "$in" ]'

# ─────────────────────────────────────────────────────────────
echo "2) msi 必须带 -add-msi-dse"
cfg="$(mkcfg)"; in="$(mkinput a.msi)"; reset_log
"$SIGN" --target msi --input "$in" --config "$cfg" --skip-verify >/dev/null 2>&1
call="$(last_call)"
check "带 -add-msi-dse"                 '[[ "$call" == *"-add-msi-dse"* ]]'

# ─────────────────────────────────────────────────────────────
echo "3) 配了 PKCS11_PROVIDER 时走 provider 路线"
cfg="$(mkcfg '' 'PKCS11_PROVIDER="/usr/lib/x86_64-linux-gnu/ossl-modules/pkcs11prov.so"')"
in="$(mkinput b.exe)"; reset_log
"$SIGN" --target exe --input "$in" --config "$cfg" --skip-verify >/dev/null 2>&1
call="$(last_call)"
check "带 -provider"                    '[[ "$call" == *"-provider /usr/lib/x86_64-linux-gnu/ossl-modules/pkcs11prov.so"* ]]'
check "不带 -engine / -login"           '[[ "$call" != *"-engine"* && "$call" != *"-login"* ]]'

# ─────────────────────────────────────────────────────────────
echo "4) 首次失败 → 用备用时间戳重试"
cfg="$(mkcfg)"; in="$(mkinput c.exe)"; reset_log
STUB_FAIL=1 "$SIGN" --target exe --input "$in" --config "$cfg" --skip-verify >/dev/null 2>&1
rc=$?
check "两次调用（首次 + 重试）"           '[ "$(call_count)" = "2" ]'
check "第二次用备用时间戳"               '[[ "$(last_call)" == *"-ts http://ts-backup.example"* ]]'
check "最终退出码 0"                     '[ "$rc" = "0" ]'

# ─────────────────────────────────────────────────────────────
echo "5) 无备用时间戳且失败 → 退出码非 0"
cfg="$WORK/nobackup.config"
{
  printf 'CONFIG_BACKEND="token-pkcs11"\n'
  printf 'PKCS11_MODULE="/usr/lib/x86_64-linux-gnu/opensc-pkcs11.so"\n'
  printf 'PKCS11_URI="pkcs11:token=T;object=O"\n'
  printf 'PIN_FILE="%s"\n' "$PIN"
  printf 'TIMESTAMP_URL="http://ts-primary.example"\n'
} > "$cfg"
in="$(mkinput d.exe)"; reset_log
STUB_FAIL=1 "$SIGN" --target exe --input "$in" --config "$cfg" --skip-verify >/dev/null 2>&1
check "退出码非 0"                       '[ "$?" != "0" ]'
check "只调用一次（无备用可退）"          '[ "$(call_count)" = "1" ]'

# ─────────────────────────────────────────────────────────────
echo "6) 配置缺 PKCS11_URI → 早期失败且不调用签名"
cfg="$WORK/missing.config"
printf 'CONFIG_BACKEND="token-pkcs11"\nPKCS11_MODULE="/x.so"\nPIN_FILE="%s"\nTIMESTAMP_URL="http://t"\n' "$PIN" > "$cfg"
in="$(mkinput e.exe)"; reset_log
"$SIGN" --target exe --input "$in" --config "$cfg" >/dev/null 2>&1
check "退出码非 0"                       '[ "$?" != "0" ]'
check "没有调用 osslsigncode"             '[ "$(call_count)" = "0" ]'

# ─────────────────────────────────────────────────────────────
echo "7) --preflight 在配置齐全时不签名"
cfg="$(mkcfg)"; reset_log
"$SIGN" --preflight --config "$cfg" >/dev/null 2>&1
check "没有调用 osslsigncode"             '[ "$(call_count)" = "0" ]'

# ─────────────────────────────────────────────────────────────
printf '\n结果：%d 通过，%d 失败\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ] || exit 1

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

# sign 需要产出 -out 指定文件（sign.sh 之后会 mv 回原文件）。
# 同时记录"调用时该路径是否已存在"——真实的 osslsigncode 明确拒绝覆盖已存在
# 的输出文件（man page: "Output files are not overwritten."），所以这里必须
# 断言为 no：sign.sh 若用 mktemp 预建文件就会踩到，签名在真机上直接失败。
prev=""
for a in "$@"; do
  if [ "$prev" = "-out" ]; then
    if [ -e "$a" ]; then
      printf 'exists\n' >> "$STUB_LOG.outpre"
    else
      printf 'absent\n' >> "$STUB_LOG.outpre"
    fi
    : > "$a"
  fi
  prev="$a"
done
exit 0
STUB
chmod +x "$STUB_DIR/osslsigncode"

# —— 假 pkcs11-tool：只认 env: 传 PIN，用来固化预检的正确用法 ——
# 真实 pkcs11-tool 的 --pin 只支持 env:VARIABLE（Debian man page），
# 不支持 file:。/ 写成 file: 时它会当成字面 PIN，登录失败 → 看起来像"证书不存在"。
cat > "$STUB_DIR/pkcs11-tool" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$STUB_LOG.pkcs11"
want=""
prev=""
for a in "$@"; do
  [ "$prev" = "--pin" ] && want="$a"
  prev="$a"
done
case "$want" in
  env:*)
    var="${want#env:}"
    eval "pin=\"\${$var:-}\""
    [ -z "$pin" ] && exit 2          # 变量没设好 = 登录失败
    ;;
  file:*) exit 3 ;;                   # 真实工具不支持 file:
  "")     exit 2 ;;                   # 没传 PIN
  *)      exit 2 ;;                   # 字面 PIN 不对
esac
# 登录成功后才会列出对象
case "$*" in
  *"--type cert"*)    echo "Certificate Object; type = X.509 cert" ;;
  *"--type privkey"*) echo "Private Key Object; RSA" ;;
esac
exit 0
STUB
chmod +x "$STUB_DIR/pkcs11-tool"
export PATH="$STUB_DIR:$PATH"

# —— 假 jsign：记录 argv（jsign 就地签名，不需要建 out 文件）——
cat > "$STUB_DIR/jsign" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$STUB_LOG.jsign"
exit 0
STUB
chmod +x "$STUB_DIR/jsign"
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

reset_log() { : > "$STUB_LOG"; rm -f "$STUB_LOG.signcount" "$STUB_LOG.outpre" "$STUB_LOG.pkcs11" "$STUB_LOG.jsign"; }
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
check "-out 路径在调用时不存在（禁止 mktemp 预建）" '[ "$(tail -1 "$STUB_LOG.outpre")" = "absent" ]'
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
echo "7) --preflight：不签名，且 PIN 走 env: 而非 file:"
# 预检会检查 PKCS11_MODULE 是否为真实存在的文件，所以这里造一个占位库。
printf 'not-a-real-pkcs11-module' > "$WORK/fake-pkcs11.so"
cfg="$WORK/preflight.config"
{
  printf 'CONFIG_BACKEND="token-pkcs11"\n'
  printf 'PKCS11_MODULE="%s"\n' "$WORK/fake-pkcs11.so"
  printf 'PKCS11_URI="pkcs11:token=TestToken;object=Code%%20Signing"\n'
  printf 'PIN_FILE="%s"\n' "$PIN"
  printf 'TIMESTAMP_URL="http://127.0.0.1:1/ts"\n'
} > "$cfg"
reset_log
out="$("$SIGN" --preflight --config "$cfg" 2>&1)"; pfr=$?
check "没有调用 osslsigncode"             '[ "$(call_count)" = "0" ]'
check "退出码 0（配置齐全时预检通过）"     '[ "$pfr" = "0" ]'
check "识别到证书对象 ✓"                  '[[ "$out" == *"证书对象"*"✓"* ]]'
check "识别到私钥对象 ✓"                  '[[ "$out" == *"私钥对象"*"✓"* ]]'
check "PIN 用 --pin env:"                 'grep -q -- "--pin env:" "$STUB_LOG.pkcs11"'
check "**没有**用 --pin file:"            '! grep -q -- "--pin file:" "$STUB_LOG.pkcs11"'

# ─────────────────────────────────────────────────────────────
echo "8) --preflight 在缺配置时退出 1"
reset_log
"$SIGN" --preflight --config "$WORK/nope.config" >/dev/null 2>&1
check "退出码 1"                          '[ "$?" = "1" ]'
check "没有调用 pkcs11-tool"              '[ ! -s "$STUB_LOG.pkcs11" ]'

# ─────────────────────────────────────────────────────────────
echo "9) 自检：把 env: 改回 file: 后预检必须失败（证明第 7 组断言非空）"
# 没有这条，第 7 组可能因为"预检压根不检查证书"而永远通过，断言就成了空话。
MUT="$WORK/mut"
mkdir -p "$MUT"
cp -r "$HERE/." "$MUT/"
sed 's/--pin env:MANDARYN_PKCS11_PIN/--pin "file:$PIN_FILE"/' "$HERE/sign.sh" > "$MUT/sign.sh"
check "变异副本已生成且与原文件不同" '! cmp -s "$HERE/sign.sh" "$MUT/sign.sh"'
"$MUT/sign.sh" --preflight --config "$WORK/preflight.config" >/dev/null 2>&1
check "改回 file: 后预检退出非 0"          '[ "$?" != "0" ]'

# ─────────────────────────────────────────────────────────────
echo "10) jsign 后端：参数拼装正确"
# 配置里只给 jsign 相关变量 → sign.sh 应自动选 jsign 后端
jcfg="$WORK/jsign.config"
{
  printf 'JSIGN_KEYSTORE="/etc/mandaryn/pkcs11.cfg"\n'
  printf 'JSIGN_ALIAS="Code Signing"\n'
  printf 'PIN_FILE="%s"\n' "$PIN"
  printf 'TIMESTAMP_URL="http://ts.example"\n'
  printf 'SIGN_DESCRIPTION="Mandaryn Selftest"\n'
} > "$jcfg"
in="$(mkinput f.exe)"; reset_log
"$SIGN" --target exe --input "$in" --config "$jcfg" --skip-verify >/dev/null 2>&1
jc="$(tail -1 "$STUB_LOG.jsign")"
check "自动识别为 jsign 后端并被调用"      '[ -n "$jc" ]'
check "带 --storetype PKCS11（默认）"      '[[ "$jc" == *"--storetype PKCS11"* ]]'
check "带 --keystore / --alias"            '[[ "$jc" == *"--keystore /etc/mandaryn/pkcs11.cfg"* && "$jc" == *"--alias Code Signing"* ]]'
check "PIN 走 --storepass file:（jsign 支持该前缀）" '[[ "$jc" == *"--storepass file:$PIN"* ]]'
check "PIN 未以明文出现"                    '[[ "$jc" != *"12345678"* ]]'
check "显式 --tsmode RFC3161"              '[[ "$jc" == *"--tsmode RFC3161"* ]]'
check "带重试参数"                          '[[ "$jc" == *"--tsretries 5"* && "$jc" == *"--tsretrywait 15"* ]]'
check "**带** --replace（对齐 osslsigncode 默认行为）" '[[ "$jc" == *"--replace"* ]]'
check "带 --name 描述"                      '[[ "$jc" == *"--name Mandaryn Selftest"* ]]'
check "没有调用 osslsigncode 签名"          '[ "$(call_count)" = "0" ]'

# ─────────────────────────────────────────────────────────────
echo "11) 验签门禁：签名成功但 verify 失败必须退出 1"
# 前面所有用例都用 --skip-verify，验签分支此前零覆盖。这里放开它。
cfg="$(mkcfg)"; in="$(mkinput g.exe)"; reset_log
"$SIGN" --target exe --input "$in" --config "$cfg" >/dev/null 2>&1
check "verify 成功时退出 0"                '[ "$?" = "0" ]'
check "确实跑了 verify"                    '[[ "$(last_call)" == verify* ]]'
check "verify 带 -in"                      '[[ "$(last_call)" == *"-in"* ]]'

# 让 verify 子命令失败，但 sign 仍需正常产出 -out 文件（否则 sign.sh 在 mv 那步就退出，
# 根本走不到验签 —— 第一版测试正是踩了这个坑）。
cat > "$WORK/verifyfail.sh" <<'STUB'
#!/usr/bin/env bash
if [ "${1:-}" = verify ]; then
  echo "FAKE: signature invalid" >&2
  exit 1
fi
prev=""
for a in "$@"; do
  [ "$prev" = "-out" ] && : > "$a"
  prev="$a"
done
exit 0
STUB
chmod +x "$WORK/verifyfail.sh"
FAILDIR="$WORK/failbin"; mkdir -p "$FAILDIR"; cp "$WORK/verifyfail.sh" "$FAILDIR/osslsigncode"
reset_log
PATH="$FAILDIR:$PATH" "$SIGN" --target exe --input "$(mkinput h.exe)" --config "$cfg" 2>"$WORK/err.txt"
check "verify 失败时退出 1（不能"看起来签了"）" '[ "$?" = "1" ]'
check "失败原因打印到 stderr"               'grep -q "验签失败" "$WORK/err.txt"'

# ─────────────────────────────────────────────────────────────
printf '\n结果：%d 通过，%d 失败\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ] || exit 1

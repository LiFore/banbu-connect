#!/usr/bin/env bash
# Antigravity 实测脚本（BXC-F0-016，ADR-046），只在 macOS 上运行。
#
# 只读：不修改任何文件、不登录、不发网络请求。不输出任何 token 原文，
# 凭据类字段只给 sha256 前 12 位指纹和长度，用来比较「登录前后、刷新前后、换机器后」是否变化。
#
# 用法：
#   scripts/probe-antigravity.sh snapshot <标签>    # 记一次快照，如 before-login / after-login / after-1h
#   scripts/probe-antigravity.sh diff <标签A> <标签B>
#
# 快照保存在 ~/boxctl-probe/<标签>/，可以整个目录发回来分析（里面没有 token 原文）。

set -euo pipefail

OUT_ROOT="$HOME/boxctl-probe"

# 可能存放 agy / Antigravity 应用数据的位置（来源说法不一，全部列出）。
CANDIDATE_DIRS=(
  "$HOME/.gemini"
  "$HOME/.config/agy"
  "$HOME/.antigravity"
  "$HOME/Library/Application Support/Antigravity"
  "$HOME/Library/Application Support/antigravity"
  "$HOME/Library/Application Support/Google/Antigravity"
)

# 名字像凭据的字段：只出指纹，不出原文。
SECRET_KEY_RE='token|secret|password|credential|cookie|session|key'

fp() { # 标准输入 → sha256 前 12 位
  shasum -a 256 | cut -c1-12
}

# 列出 JSON 文件的结构：键名、类型；字符串字段若像凭据则给指纹和长度，否则给原值（截断）。
json_shape() {
  /usr/bin/osascript -l JavaScript - "$1" "$SECRET_KEY_RE" <<'JXA' 2>/dev/null || echo "  (不是 JSON 或无法解析)"
ObjC.import('Foundation');
function run(argv) {
  const path = argv[0], re = new RegExp(argv[1], 'i');
  const data = $.NSString.stringWithContentsOfFileEncodingError(path, $.NSUTF8StringEncoding, null);
  const obj = JSON.parse(ObjC.unwrap(data));
  const lines = [];
  const walk = (v, p) => {
    if (v && typeof v === 'object' && !Array.isArray(v)) { for (const k of Object.keys(v)) walk(v[k], p ? p + '.' + k : k); return; }
    if (Array.isArray(v)) { lines.push(`  ${p}: array(${v.length})`); v.slice(0, 3).forEach((x, i) => walk(x, `${p}[${i}]`)); return; }
    if (typeof v === 'string' && (re.test(p) || v.length > 80)) { lines.push(`  ${p}: string len=${v.length}`); return; }
    lines.push(`  ${p}: ${typeof v} = ${String(v).slice(0, 80)}`);
  };
  walk(obj, '');
  return lines.join('\n');
}
JXA
}

# 对 JSON 中像凭据的字符串字段算指纹：JXA 取值，shell 里用 shasum 算（值只经过管道，不落盘不输出）。
json_secret_fps() {
  /usr/bin/osascript -l JavaScript - "$1" "$SECRET_KEY_RE" <<'JXA' 2>/dev/null | while IFS=$'\t' read -r p v; do
ObjC.import('Foundation');
function run(argv) {
  const re = new RegExp(argv[1], 'i');
  const obj = JSON.parse(ObjC.unwrap($.NSString.stringWithContentsOfFileEncodingError(argv[0], $.NSUTF8StringEncoding, null)));
  const out = [];
  const walk = (v, p) => {
    if (v && typeof v === 'object') { for (const k of Object.keys(v)) walk(v[k], p ? p + '.' + k : k); return; }
    if (typeof v === 'string' && (re.test(p) || v.length > 80)) out.push(p + '\t' + v);
  };
  walk(obj, '');
  return out.join('\n');
}
JXA
    [ -n "$p" ] && printf '  %s fp=%s\n' "$p" "$(printf '%s' "$v" | fp)"
  done
}

snapshot() {
  local label="$1" dir="$OUT_ROOT/$1"
  mkdir -p "$dir"
  echo "快照写入 $dir"

  {
    echo "# 时间"; date -u +%Y-%m-%dT%H:%M:%SZ
    echo "# 系统"; sw_vers; uname -m
    echo "# agy"
    if command -v agy >/dev/null; then
      echo "path: $(command -v agy)"; file -L "$(command -v agy)" || true
      agy --version 2>&1 | head -5 || true
    else
      echo "未找到 agy"
    fi
    echo "# Antigravity 应用"
    for app in /Applications/Antigravity.app "$HOME/Applications/Antigravity.app"; do
      [ -d "$app" ] || continue
      echo "$app"
      /usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' -c 'Print :CFBundleShortVersionString' "$app/Contents/Info.plist" 2>/dev/null || true
    done
    echo "# 相关环境变量（只列名字）"
    env | cut -d= -f1 | grep -Ei 'agy|antigravity|gemini|google|browser|proxy' || true
  } > "$dir/env.txt"

  # agy 的帮助：看有没有 --no-browser、打印授权链接、指定配置目录之类的选项（V-agy-browser）。
  if command -v agy >/dev/null; then
    { agy --help 2>&1; echo; agy help 2>&1; } | head -200 > "$dir/agy-help.txt" || true
  fi

  # 目录树：路径、大小、修改时间、权限（不读内容）。
  : > "$dir/tree.txt"
  for d in "${CANDIDATE_DIRS[@]}"; do
    [ -e "$d" ] || continue
    echo "== $d" >> "$dir/tree.txt"
    find "$d" -maxdepth 4 \( -name node_modules -o -name Cache -o -name 'Code Cache' -o -name GPUCache -o -name CachedData \) -prune -o \
      -type f -print0 2>/dev/null | xargs -0 stat -f '%Sp %z %Sm %N' -t '%Y-%m-%dT%H:%M:%S' 2>/dev/null >> "$dir/tree.txt" || true
  done

  # 名字像凭据或配置的文件：结构与指纹（V-agy-store、V-agy-refresh）。
  : > "$dir/files.txt"
  grep -Ei "$SECRET_KEY_RE|oauth|account|settings|auth" "$dir/tree.txt" | awk '{ $1=$2=$3=""; sub(/^ +/, ""); print }' | while read -r f; do
    [ -f "$f" ] || continue
    [ "$(stat -f %z "$f")" -le 1048576 ] || { echo "== $f (超过 1MB，跳过)" >> "$dir/files.txt"; continue; }
    {
      echo "== $f"
      echo "  file_fp=$(fp < "$f") size=$(stat -f %z "$f")"
      if head -c 1 "$f" | grep -q '[{[]'; then
        json_shape "$f"
        json_secret_fps "$f"
      else
        echo "  (非 JSON：$(file -b "$f"))"
      fi
    } >> "$dir/files.txt"
  done

  # Keychain：只列属性，不取密码（dump-keychain 不带 -d 不会读出秘密，也不会弹授权框）。
  security dump-keychain 2>/dev/null | awk '
    /^keychain:/ { if (blk ~ /(antigravity|gemini|Antigravity|Gemini|agy|cloudcode)/) print blk "\n"; blk="" }
    { blk = blk $0 "\n" }
    END { if (blk ~ /(antigravity|gemini|Antigravity|Gemini|agy|cloudcode)/) print blk }
  ' | grep -E '^keychain:|^class:|"svce"|"acct"|"labl"|"mdat"|"cdat"' > "$dir/keychain.txt" || true

  echo "完成：env.txt agy-help.txt tree.txt files.txt keychain.txt"
}

diff_snap() {
  local a="$OUT_ROOT/$1" b="$OUT_ROOT/$2"
  for f in tree.txt files.txt keychain.txt; do
    echo "######## $f"
    diff -u "$a/$f" "$b/$f" || true
  done
}

case "${1:-}" in
  snapshot) [ -n "${2:-}" ] || { echo "缺少标签"; exit 2; }; snapshot "$2" ;;
  diff) [ -n "${3:-}" ] || { echo "用法：diff <标签A> <标签B>"; exit 2; }; diff_snap "$2" "$3" ;;
  *) sed -n '2,13p' "$0"; exit 2 ;;
esac

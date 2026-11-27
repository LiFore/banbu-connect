#!/bin/bash
# 放在 dmg 里给用户双击运行（scripts/build-app.sh 打包时复制进去）：去掉 Banbu Connect Lite 的下载隔离标记。
# 应用只有 ad-hoc 签名、没有公证，从浏览器下载后 macOS 会提示「已损坏，无法打开」（知识库 K019）。
# 只处理 Banbu Connect Lite 本身，不动别的应用；需要时用 sudo（会要求输入开机密码）。

name="Banbu Connect Lite.app"
app=""
for dir in /Applications "$HOME/Applications"; do
	if [ -d "$dir/$name" ]; then
		app="$dir/$name"
		break
	fi
done

finish() {
	echo
	read -n 1 -s -r -p "按任意键关闭这个窗口…"
	echo
	exit "$1"
}

if [ -z "$app" ]; then
	echo "没有在「应用程序」里找到 Banbu Connect Lite。"
	echo "请先把安装窗口里的 Banbu Connect Lite 拖到「应用程序」，再双击运行这个文件。"
	finish 1
fi

echo "正在处理：$app"
if ! xattr -dr com.apple.quarantine "$app" 2>/dev/null; then
	echo "需要管理员权限，请输入这台 Mac 的开机密码（输入时不显示）："
	if ! sudo xattr -dr com.apple.quarantine "$app"; then
		echo "处理失败。可以打开安装窗口里的「安装说明」手动操作。"
		finish 1
	fi
fi

echo "已完成，正在打开 Banbu Connect Lite…"
open "$app"
finish 0

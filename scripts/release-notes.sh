#!/usr/bin/env bash
# Emit bilingual GitHub release notes for a tag from the two changelogs.
#
# Usage: release-notes.sh v1.4.1 > RELEASE_NOTES.md
set -euo pipefail

if [ "$#" -ne 1 ]; then
	echo "usage: $0 <tag>" >&2
	exit 2
fi

tag=$1
version=${tag#v}
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)

extract() {
	file=$1
	awk -v marker="## v$version" '
		index($0, marker) == 1 { found = 1; next }
		found && /^## / { exit }
		found { print }
	' "$file"
}

english=$(extract "$root/CHANGELOG.md")
chinese=$(extract "$root/CHANGELOG.zh-CN.md")

if [ -z "$english" ] || [ -z "$chinese" ]; then
	echo "release-notes: missing bilingual changelog section for v$version" >&2
	echo "release-notes: update CHANGELOG.md and CHANGELOG.zh-CN.md before tagging" >&2
	exit 1
fi

cat <<EOF
# hy2tun v$version

## English

$english

## 中文

$chinese

---

Full changelog (English): https://github.com/ayeaaaa/hy2tun/blob/main/CHANGELOG.md
完整更新日志（中文）: https://github.com/ayeaaaa/hy2tun/blob/main/CHANGELOG.zh-CN.md

EOF

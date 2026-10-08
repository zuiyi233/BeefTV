#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

node "$root_dir/embed-documentation.mjs"

# 插件包的顶层条目，两条打包路径共用同一份清单。
member_roots='manifest.json README.md docs assets web backend LICENSE'

# Info-ZIP zip 只随 Linux CI 和多数开发机提供；Windows 宿主机和发布包常常只有
# unzip。因此优先使用系统 zip（保持 Linux 产物不变），缺失时才走 Python zipfile
# 回退，复刻同样的成员集合、字节序、正斜杠成员名、deflate 和失败退出语义。
list_members() {
  find $member_roots -type f 2>/dev/null | LC_ALL=C sort
}

pick_python() {
  for candidate in python3 python py; do
    # Windows 上 python3 可能只是 Microsoft Store 的占位别名，能被 command -v 解析
    # 但跑不起来，所以必须真正执行一次再选定解释器。
    if command -v "$candidate" >/dev/null 2>&1 && "$candidate" -c 'import zipfile' >/dev/null 2>&1; then
      printf '%s\n' "$candidate"
      return 0
    fi
  done
  return 1
}

if command -v zip >/dev/null 2>&1; then
  packer=zip
else
  packer=python
  python_bin=$(pick_python) || {
    printf '%s\n' 'build-packages.sh: neither zip nor a working Python 3 interpreter is available' >&2
    exit 1
  }
  printf 'build-packages.sh: zip not found, packaging with the %s zipfile fallback\n' "$python_bin" >&2
fi

package_plugin() {
  package_id=$1
  package_dir="$root_dir/$package_id"
  output_file="$root_dir/$package_id.beeftv-plugin"
  temporary_file="$root_dir/.$package_id.beeftv-plugin.tmp"
  rm -f "$temporary_file"
  (
    cd "$package_dir"
    if [ "$packer" = zip ]; then
      list_members | zip -X -q "$temporary_file" -@
    else
      "$python_bin" - "$temporary_file" $member_roots <<'PY'
import os
import shutil
import stat
import sys
import time
import zipfile

archive_path = sys.argv[1]


def collect(path):
    # 对齐 `find <path> -type f`：起始路径不存在就跳过（find 的告警已被丢弃），
    # 只收普通文件，目录递归，符号链接和其他类型不作为成员。
    try:
        mode = os.lstat(path).st_mode
    except OSError:
        return []
    if stat.S_ISREG(mode):
        return [path]
    if not stat.S_ISDIR(mode):
        return []
    try:
        children = os.listdir(path)
    except OSError:
        return []
    members = []
    for child in children:
        members.extend(collect(path + "/" + child))
    return members


members = []
for root in sys.argv[2:]:
    members.extend(collect(root))
# 与 `LC_ALL=C sort` 一致：按文件名的原始字节排序，成员名保持相对包目录的正斜杠路径。
members.sort(key=os.fsencode)
if not members:
    # Info-ZIP 在无成员时退出码 12 且不产出归档，这里保持同样的失败语义。
    sys.exit(12)

with zipfile.ZipFile(
    archive_path, mode="w", compression=zipfile.ZIP_DEFLATED, strict_timestamps=False
) as archive:
    for name in members:
        info = os.stat(name)
        # 手工构造 ZipInfo：ZipFile.write() 会对 arcname 做 normpath，在 Windows 上
        # 把 docs/interface.md 变成 docs\interface.md，而后端会拒绝含反斜杠的成员名。
        entry = zipfile.ZipInfo(name, date_time=time.localtime(info.st_mtime)[:6])
        entry.compress_type = zipfile.ZIP_DEFLATED
        entry.external_attr = (info.st_mode & 0xFFFF) << 16
        with open(name, "rb") as source, archive.open(entry, "w") as target:
            shutil.copyfileobj(source, target)
PY
    fi
  )
  mv "$temporary_file" "$output_file"
}

for manifest in "$root_dir"/*/manifest.json; do
  package_dir=${manifest%/manifest.json}
  package_id=${package_dir##*/}
  package_plugin "$package_id"
done

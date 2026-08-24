#!/usr/bin/env bash
# 生成物漂移判据，两个产品共用一份。
#
# 此前根 Makefile 的 check-generate-cloud 与 public/Makefile 的 check-generate 是同一段
# 28 行配方，归一化后只差三个 echo 字符串。两份手写的同一算法，谁改了一边都不会有人发现。
#
# 住在 public/scripts/ 而不是 deploy/scripts/，因为 `public/` 必须能在磁盘上没有别的东西时
# 独立构建——它引用不到 deploy/。反过来根 Makefile 可以引用 public/，同 check-app-bundle.mjs
# 已有的方向。
#
# 用法：check-generate.sh <标签> <索引见证文件> <重生成命令> <路径>...
set -euo pipefail

if [[ "${1:-}" == "--self-test" ]]; then
  probe=$(mktemp -d /tmp/fairlb-check-generate-selfcheck.XXXXXX)
  trap 'rm -rf -- "$probe"' EXIT
  self=$(cd "$(dirname "$0")" && pwd)/$(basename "$0")
  mkdir -p "$probe/gen"
  printf 'stable\n' > "$probe/gen/a.txt"
  printf 'witness\n' > "$probe/witness"

  # 1) 指向不存在的路径必须报错，而不是「没发现漂移」。
  if (cd "$probe" && "$self" Probe witness true gen missing-dir >/dev/null 2>&1); then
    echo "x 自检失败：判据指向一个不存在的路径时仍然通过" >&2; exit 1
  fi
  # 2) 重生成不改变任何字节 → 通过。
  if ! (cd "$probe" && "$self" Probe witness true gen >/dev/null 2>&1); then
    echo "x 自检失败：产物稳定时报了红" >&2; exit 1
  fi
  # 3) 重生成改了字节 → 必须红。
  if (cd "$probe" && "$self" Probe witness "printf drift >> gen/a.txt" gen >/dev/null 2>&1); then
    echo "x 自检失败：重生成改动了产物而判据没有报红" >&2; exit 1
  fi
  # 4) 树没被 git 索引时说明情况并放行，而不是假装比对过。
  out=$(cd "$probe" && "$self" Probe witness true gen 2>&1)
  case "$out" in
    *"not indexed"*) : ;;
    *) echo "x 自检失败：未索引的树没有自陈，读数不可归属：$out" >&2; exit 1 ;;
  esac
  echo "check-generate self-test: 空指向、稳定、漂移、未索引四种情形各如实作答"
  exit 0
fi

if [[ $# -lt 4 ]]; then
  echo "usage: $0 <label> <index-witness> <regen-command> <path>..." >&2
  exit 2
fi

label=$1; witness=$2; regen=$3; shift 3
paths=("$@")

for d in "${paths[@]}"; do
  [ -e "$d" ] || { echo "x $d does not exist — the drift check is pointed at nothing"; exit 1; }
done

before=$(mktemp); after=$(mktemp)
trap 'rm -f -- "$before" "$after"' EXIT

hash_generated() {
  find "${paths[@]}" -type f -print0 \
    | LC_ALL=C sort -z \
    | LC_ALL=C xargs -0 shasum -a 256 \
    | shasum -a 256
}

hash_generated > "$before"
sh -c "$regen"
hash_generated > "$after"
if ! cmp -s "$before" "$after"; then
  echo "x $label code changed — rerun the generator and commit the result"
  exit 1
fi

if git ls-files --error-unmatch "$witness" >/dev/null 2>&1; then
  if [ -n "$(git status --porcelain --untracked-files=all -- "${paths[@]}")" ]; then
    echo "x $label code differs from what is committed — rerun the generator and commit the result"
    git status --porcelain --untracked-files=all -- "${paths[@]}"
    exit 1
  fi
else
  echo "i tree is not indexed by git (standalone copy or fresh module): $label content is stable"
fi

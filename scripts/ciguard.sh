#!/usr/bin/env bash
# Страж цепочки поставки CI и дерева (SECURITY.md, «Цепочка поставки»):
#
#   uses — каждое `uses:` в .github/workflows закреплено полным SHA коммита
#     (`owner/repo@<40 hex> # vX.Y.Z`), локальное `./…` или образ по digest:
#     тег перевешивается на чужой коммит без следа в нашем репозитории; образ
#     в `docker run` — тоже по digest;
#   permissions — у каждого джоба свой `permissions:`: токен по умолчанию
#     шире, чем нужно проверке;
#   case — два пути, различающиеся только регистром: на macOS это один файл,
#     на Linux — два, и сборка разъезжается между машинами.
#
# Исключений нет: закрепить можно любую зависимость CI.
set -euo pipefail
cd "$(dirname "$0")/.."
fail=0

for f in .github/workflows/*.yml .github/workflows/*.yaml; do
  [ -e "$f" ] || continue
  while IFS= read -r line; do
    n="${line%%:*}"
    ref="$(printf '%s' "${line#*:}" | sed -E 's/^[[:space:]-]*uses:[[:space:]]*//; s/[[:space:]]+#.*$//; s/^["'\'']//; s/["'\'']$//')"
    case "$ref" in
      ./*) continue ;;
      docker://*@sha256:*) continue ;;
    esac
    # Сравнение средствами bash, а не grep -q в конвейере: ранний выход grep под
    # pipefail даёт SIGPIPE и ложный отказ.
    if ! [[ "$ref" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_./-]+@[0-9a-f]{40}$ ]]; then
      echo "ciguard: ${f}:${n}: действие не закреплено SHA коммита: ${ref}" >&2
      fail=1
    fi
  done < <(grep -nE '^[[:space:]-]*uses:' "$f" || true)

  # Образ в `docker run` — по digest: тег образа перевешивается так же, как тег действия.
  while IFS= read -r line; do
    n="${line%%:*}"
    if ! [[ "${line#*:}" =~ @sha256:[0-9a-f]{64} ]]; then
      echo "ciguard: ${f}:${n}: образ docker run не закреплён digest" >&2
      fail=1
    fi
  done < <(grep -nE 'docker[[:space:]]+run[[:space:]]' "$f" || true)

  # Джобы — ключи второго уровня под jobs:, у каждого обязан быть permissions:.
  awk -v file="$f" '
    /^jobs:[[:space:]]*$/ { injobs = 1; next }
    injobs && /^[^[:space:]#]/ { injobs = 0 }
    injobs && /^  [A-Za-z0-9_-]+:[[:space:]]*$/ {
      if (job != "" && !perm) { printf "ciguard: %s:%d: у джоба %s нет permissions:\n", file, line, job > "/dev/stderr"; bad = 1 }
      job = $1; sub(/:$/, "", job); line = NR; perm = 0; next
    }
    injobs && /^    permissions:/ { perm = 1 }
    END {
      if (job != "" && !perm) { printf "ciguard: %s:%d: у джоба %s нет permissions:\n", file, line, job > "/dev/stderr"; bad = 1 }
      exit bad
    }
  ' "$f" || fail=1
done

dups="$(git ls-files | tr '[:upper:]' '[:lower:]' | sort | uniq -d)"
if [ -n "$dups" ]; then
  printf 'ciguard: пути различаются только регистром: %s\n' $dups >&2
  fail=1
fi

[ "$fail" = 0 ] || exit 1
echo "ciguard: ok"

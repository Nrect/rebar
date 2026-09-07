#!/usr/bin/env bash
# Одна версия Go на весь репозиторий: `go` — минимум, на котором пакет обязан
# собираться у потребителя, `toolchain` — патч, которым его собирает
# govulncheck. Разъезд версий значит, что часть модулей проверена не тем
# компилятором, а потребитель, подключивший два модуля, получит бамп Go, о
# котором не просил. go.work входит в сравнение: расхождение с ним ломает
# локальную сборку между модулями.
set -euo pipefail

cd "$(dirname "$0")/.."
awk_script="scripts/gomod-fields.awk"

files=$(find . -name go.mod -not -path './.git/*' | sort)
if [ -f go.work ]; then
	files="./go.work
$files"
fi

status=0
for directive in go toolchain; do
	table=""
	for f in $files; do
		value=$(awk -f "$awk_script" "$f" | awk -v d="$directive" '$1 == d { print $2; exit }')
		[ -n "$value" ] || value="(нет директивы)"
		table="$table$value $f
"
	done
	distinct=$(printf '%s' "$table" | awk '{ print $1 }' | sort -u | wc -l | tr -d ' ')
	if [ "$distinct" -gt 1 ]; then
		echo "lockstep: директива '$directive' разъехалась:"
		printf '%s' "$table" | sort | sed 's/^/  /'
		status=1
	else
		echo "lockstep: $directive $(printf '%s' "$table" | awk 'NR == 1 { print $1 }') — во всех $(printf '%s' "$table" | wc -l | tr -d ' ') файлах"
	fi
done

if [ "$status" -ne 0 ]; then
	echo
	echo "Правится одним PR: одинаковые 'go' и 'toolchain' во всех go.mod и в go.work."
fi
exit "$status"

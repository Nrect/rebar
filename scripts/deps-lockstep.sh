#!/usr/bin/env bash
# Общие зависимости — одна версия во всех модулях. Потребитель, подключивший
# два модуля тулкита, получает по MVS максимум из их требований: если у нас
# разные pgx, он собирается с версией, на которой ни один из модулей не
# тестировался. Сравниваются и прямые, и косвенные require: в графе
# потребителя разницы между ними нет.
set -euo pipefail

cd "$(dirname "$0")/.."
awk_script="scripts/gomod-fields.awk"

# Префиксы общих зависимостей. Дополняется вместе с белым списком стража
# импортов: библиотека, попавшая больше чем в один модуль, попадает и сюда.
prefixes="github.com/jackc/pgx/v5
go.opentelemetry.io/otel
github.com/google/uuid
github.com/stretchr/testify
github.com/testcontainers/testcontainers-go
github.com/pressly/goose/v3"

pairs=""
for f in $(find . -name go.mod -not -path './.git/*' | sort); do
	requires=$(awk -f "$awk_script" "$f" | awk '$1 == "require" { print $2, $3 }')
	while read -r module version; do
		[ -n "$module" ] || continue
		for p in $prefixes; do
			case "$module" in
			"$p" | "$p"/*)
				pairs="$pairs$module $version $f
"
				break
				;;
			esac
		done
	done <<-INNER
		$requires
	INNER
done

if [ -z "$pairs" ]; then
	echo "lockstep: общих зависимостей в модулях нет"
	exit 0
fi

status=0
for module in $(printf '%s' "$pairs" | awk '{ print $1 }' | sort -u); do
	rows=$(printf '%s' "$pairs" | awk -v m="$module" '$1 == m { print $2, $3 }' | sort)
	distinct=$(printf '%s\n' "$rows" | awk '{ print $1 }' | sort -u | wc -l | tr -d ' ')
	if [ "$distinct" -gt 1 ]; then
		echo "lockstep: $module разъехался по версиям:"
		printf '%s\n' "$rows" | sed 's/^/  /'
		status=1
	else
		echo "lockstep: $module $(printf '%s\n' "$rows" | awk 'NR == 1 { print $1 }')"
	fi
done

if [ "$status" -ne 0 ]; then
	echo
	echo "Правится одним PR: go get <модуль>@<версия> во всех перечисленных каталогах."
fi
exit "$status"

# Разбор go.mod/go.work: печатает "go <версия>", "toolchain <версия>" и
# "require <путь> <версия>" по одной записи на строку. Директивы вне блоков и
# внутри `require (…)` разбираются одинаково; replace/exclude/retract
# пропускаются — версия оттуда не является требованием модуля.
BEGIN { block = "" }

{ sub(/\/\/.*$/, ""); gsub(/^[ \t]+|[ \t]+$/, "") }
$0 == "" { next }

block != "" && $1 == ")" { block = ""; next }

block == "require" && NF >= 2 { print "require", $1, $2; next }
block != "" { next }

$NF == "(" && NF == 2 { block = $1; next }

$1 == "go" && NF == 2 { print "go", $2; next }
$1 == "toolchain" && NF == 2 { print "toolchain", $2; next }
$1 == "require" && NF >= 3 { print "require", $2, $3; next }

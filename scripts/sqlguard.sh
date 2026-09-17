#!/usr/bin/env bash
# Страж миграций (docs/CORRECTNESS.md, §12 и §13; CONVENTIONS §9): текст
# сравнивается побайтно, схема меняется на живой базе без долгих блокировок.
# Проверяет */migrations/*.sql всего репозитория, кроме testdata.
#
#   collate — текстовая колонка (text, varchar, char, citext) без COLLATE "C"
#     сразу после типа;
#   lock_timeout — первый оператор после `-- +goose Up` и после `-- +goose Down`
#     не `SET LOCAL lock_timeout`; у файла `-- +goose NO TRANSACTION` в разделе
#     нет `SET lock_timeout` и `RESET lock_timeout`;
#   live — в Up миграции после первой: RENAME, DROP COLUMN, DROP TABLE,
#     ALTER COLUMN … TYPE, SET NOT NULL, CREATE INDEX без CONCURRENTLY,
#     ADD CONSTRAINT CHECK или FOREIGN KEY без NOT VALID.
#
# Комментарии, строки и тела в долларовых кавычках вырезаются до проверки,
# сигнатуры функций — тоже: у аргумента функции сортировки нет.
#
# Исключения — scripts/sqlguard.allow: путь, строка кода (или `*` — все
# нарушения файла, или маркер раздела) и причина через табуляцию. Запись,
# которой больше нет в коде, — тоже отказ: починенное вычёркивают.
set -euo pipefail
cd "$(dirname "$0")/.."
exec perl -e '
use strict;
use warnings;

my %allow;
my %used;
open my $af, "<", "scripts/sqlguard.allow" or die "нет scripts/sqlguard.allow\n";
while (my $l = <$af>) {
  chomp $l;
  next if $l =~ /^\s*(#|$)/;
  my ($path, $code, $why) = split /\t/, $l, 3;
  die "sqlguard.allow: у записи нет причины: $l\n" unless defined $why && $why =~ /\S/;
  $allow{"$path\t$code"} = 1;
}

my @violations;
sub check_allow {
  my ($path, $code, $line_no, $what) = @_;
  for my $key ("$path\t$code", "$path\t*") {
    if ($allow{$key}) { $used{$key} = 1; return }
  }
  push @violations, "$path:$line_no: $what: $code";
}

# blank — комментарии, строки и долларовые тела заменяются пробелами; переводы
# строк остаются, номера строк не съезжают.
sub blank {
  my ($s) = @_;
  my $out = "";
  my $i = 0;
  my $n = length $s;
  while ($i < $n) {
    my $c = substr($s, $i, 1);
    if ($c eq "-" && substr($s, $i, 2) eq "--") {
      my $j = index($s, "\n", $i);
      $j = $n if $j < 0;
      $out .= " " x ($j - $i);
      $i = $j;
    } elsif ($c eq "\x27") {
      my $j = $i + 1;
      while ($j < $n) {
        if (substr($s, $j, 1) eq "\x27") {
          if (substr($s, $j + 1, 1) eq "\x27") { $j += 2; next }
          last;
        }
        $j++;
      }
      my $chunk = substr($s, $i, $j - $i + 1);
      $chunk =~ s/[^\n]/ /g;
      $out .= $chunk;
      $i = $j + 1;
    } elsif ($c eq "\$" && substr($s, $i) =~ /^(\$[A-Za-z_]*\$)/) {
      my $tag = $1;
      my $j = index($s, $tag, $i + length $tag);
      $j = $n - length $tag if $j < 0;
      my $chunk = substr($s, $i, $j + length($tag) - $i);
      $chunk =~ s/[^\n]/ /g;
      $out .= $chunk;
      $i = $j + length $tag;
    } else {
      $out .= $c;
      $i++;
    }
  }
  return $out;
}

sub spaces { my $t = shift; $t =~ s/[^\n]/ /g; return $t }

my $texttype = qr/\b(?:text|varchar|character\s+varying|char|character|bpchar|citext)\b(?:\s*\(\s*\d+\s*\))?(?:\s*\[\s*\])?/i;

for my $f (sort map { chomp; s{^\./}{}; $_ } `find . -path ./.git -prune -o -path ./.claude -prune -o -path "*/testdata/*" -prune -o -path "*/migrations/*.sql" -print`) {
  open my $fh, "<", $f or die "$f: $!\n";
  local $/;
  my $raw = <$fh>;
  close $fh;
  my @lines = split /\n/, $raw, -1;
  my $code = blank($raw);
  # Сигнатуры функций: аргументы и RETURNS сортировки не имеют.
  $code =~ s/(\bFUNCTION\s+[\w."]+\s*)(\((?:[^()]++|(?2))*\))/$1 . spaces($2)/gie;
  $code =~ s/(\bRETURNS\s+)((?:SETOF\s+)?[\w."]+(?:\s*\[\s*\])?)/$1 . spaces($2)/gie;
  my $lineof = sub { my $pos = shift; return 1 + (substr($code, 0, $pos) =~ tr/\n//) };
  my $text_of = sub { my $ln = shift; (my $t = $lines[$ln - 1] // "") =~ s/^\s+|\s+$//g; return $t };

  # collate
  while ($code =~ /$texttype/g) {
    my ($start, $end) = ($-[0], $+[0]);
    next if $start >= 2 && substr($code, $start - 2, 2) eq "::";
    next if substr($code, $end) =~ /^\s+COLLATE\s+"C"/i;
    my $ln = $lineof->($start);
    check_allow($f, $text_of->($ln), $ln, "текстовая колонка без COLLATE \"C\"");
  }

  # lock_timeout и live — по разделам Up и Down
  my $notx = $raw =~ /^--\s*\+goose\s+NO\s+TRANSACTION\s*$/mi;
  (my $base = $f) =~ s{.*/}{};
  my ($num) = $base =~ /^(\d+)_/;
  my $first = !defined($num) || $num + 0 <= 1;
  while ($raw =~ /^--\s*\+goose\s+(Up|Down)\s*$/mgi) {
    my $dir = ucfirst lc $1;
    my $from = $+[0];
    my $ln_marker = 1 + (substr($raw, 0, $-[0]) =~ tr/\n//);
    # Маркеры goose — комментарии, в $code они вырезаны: границу раздела ищем
    # в сыром тексте, операторы — в вырезанном.
    my $rawrest = substr($raw, $from);
    my $to = $rawrest =~ /^--\s*\+goose\s+(?:Up|Down)\s*$/mi ? $-[0] : length $rawrest;
    my $section = substr($code, $from, $to);
    if ($notx) {
      unless ($section =~ /\bSET\s+lock_timeout\b/i && $section =~ /\bRESET\s+lock_timeout\b/i) {
        check_allow($f, "-- +goose $dir", $ln_marker, "раздел без транзакции без SET и RESET lock_timeout");
      }
    } else {
      my ($stmt) = $section =~ /^\s*([^;]*)/;
      unless (defined $stmt && $stmt =~ /^SET\s+LOCAL\s+lock_timeout\b/i) {
        check_allow($f, "-- +goose $dir", $ln_marker, "первый оператор раздела не SET LOCAL lock_timeout");
      }
    }
    next if $dir ne "Up" || $first;
    my @live = (
      [qr/\bRENAME\b/i, "переименование на живой базе"],
      [qr/\bDROP\s+COLUMN\b/i, "удаление колонки на живой базе"],
      [qr/\bDROP\s+TABLE\b/i, "удаление таблицы на живой базе"],
      [qr/\bALTER\s+COLUMN\s+[\w"]+\s+(?:SET\s+DATA\s+)?TYPE\b/i, "смена типа переписывает таблицу"],
      [qr/\bSET\s+NOT\s+NULL\b/i, "SET NOT NULL сканирует таблицу под блокировкой"],
      [qr/\bCREATE\s+(?:UNIQUE\s+)?INDEX\b(?!\s+CONCURRENTLY)/i, "индекс без CONCURRENTLY блокирует запись"],
      [qr/\bADD\s+(?:CONSTRAINT\s+[\w"]+\s+)?(?:CHECK|FOREIGN\s+KEY)\b(?![^;]*\bNOT\s+VALID\b)/i, "ограничение без NOT VALID сканирует таблицу под блокировкой"],
    );
    for my $rule (@live) {
      my ($re, $what) = @$rule;
      while ($section =~ /$re/g) {
        my $ln = 1 + (substr($code, 0, $from + $-[0]) =~ tr/\n//);
        check_allow($f, $text_of->($ln), $ln, $what);
      }
    }
  }
}

my @stale = grep { !$used{$_} } sort keys %allow;
if (@violations || @stale) {
  print STDERR "sqlguard: $_\n" for @violations;
  print STDERR "sqlguard: запись разрешения больше не нужна, вычеркнуть: $_\n" for @stale;
  exit 1;
}
print "sqlguard: ok\n";
'

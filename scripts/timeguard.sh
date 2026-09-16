#!/usr/bin/env bash
# Страж времени (docs/CORRECTNESS.md, §8; CONVENTIONS §11): всё время — UTC и
# параметром. Проверяет весь репозиторий, модули по go.mod.
#
#   миграции (*/migrations/*.sql, без комментариев): timestamp без пояса,
#     now(), current_timestamp и родня, AT TIME ZONE, SET TIME ZONE;
#   Go-код прода (без _test.go и комментариев): time.Local, .Local(), любой
#     time.Now, кроме часов `func() time.Time { return time.Now().UTC() }` и
#     `time.Now().UTC().Truncate(time.Microsecond)`.
#
# Исключения — scripts/timeguard.allow: путь, строка кода и причина через
# табуляцию. Запись, которой больше нет в коде, — тоже отказ: починенное
# вычёркивают.
set -euo pipefail
cd "$(dirname "$0")/.."
exec perl -e '
use strict;
use warnings;

my %allow;
my %used;
open my $af, "<", "scripts/timeguard.allow" or die "нет scripts/timeguard.allow\n";
while (my $l = <$af>) {
  chomp $l;
  next if $l =~ /^\s*(#|$)/;
  my ($path, $code, $why) = split /\t/, $l, 3;
  die "timeguard.allow: у записи нет причины: $l\n" unless defined $why && $why =~ /\S/;
  $allow{"$path\t$code"} = 1;
}

my @violations;
sub norm { my $p = shift; $p =~ s{^\./}{}; return $p }
sub check_allow {
  my ($path, $code, $line_no, $what) = @_;
  my $key = norm($path) . "\t" . $code;
  if ($allow{$key}) { $used{$key} = 1; return }
  push @violations, norm($path) . ":$line_no: $what: $code";
}

my @mods = map { chomp; s{/go\.mod$}{}; $_ } `find . -name go.mod -not -path "./.git/*" -not -path "./.claude/*"`;

for my $m (@mods) {
  for my $f (map { chomp; $_ } `find $m -name "*.go" -not -name "*_test.go" -not -path "*/.claude/*"`) {
    open my $fh, "<", $f or die "$f: $!\n";
    my $n = 0;
    while (my $l = <$fh>) {
      $n++;
      chomp $l;
      (my $code = $l) =~ s{//.*$}{};
      next unless $code =~ /time\.(?:Now|Local)\b|\.Local\(\)/;
      (my $rest = $code) =~ s/func\(\) time\.Time \{ return time\.Now\(\)\.UTC\(\) \}//g;
      $rest =~ s/time\.Now\(\)\.UTC\(\)\.Truncate\(time\.Microsecond\)//g;
      next unless $rest =~ /time\.(?:Now|Local)\b|\.Local\(\)/;
      (my $trim = $code) =~ s/^\s+|\s+$//g;
      check_allow($f, $trim, $n, "время мимо часов в UTC");
    }
  }
}

for my $f (map { chomp; $_ } `find . -path ./.git -prune -o -path ./.claude -prune -o -path "*/migrations/*.sql" -print`) {
  open my $fh, "<", $f or die "$f: $!\n";
  my $n = 0;
  while (my $l = <$fh>) {
    $n++;
    chomp $l;
    (my $code = $l) =~ s/--.*$//;
    my $bad =
         $code =~ /\btimestamp\b(?!\s+with\s+time\s+zone)/i
      || $code =~ /\bnow\s*\(/i
      || $code =~ /\b(?:current_timestamp|localtimestamp|current_date|current_time|localtime|clock_timestamp|statement_timestamp|transaction_timestamp)\b/i
      || $code =~ /\bat\s+time\s+zone\b|\bset\s+time\s*zone\b/i;
    next unless $bad;
    (my $trim = $code) =~ s/^\s+|\s+$//g;
    check_allow($f, $trim, $n, "время в SQL мимо timestamptz и параметра");
  }
}

my @stale = grep { !$used{$_} } sort keys %allow;
if (@violations || @stale) {
  print STDERR "timeguard: $_\n" for @violations;
  print STDERR "timeguard: запись разрешения больше не нужна, вычеркнуть: $_\n" for @stale;
  exit 1;
}
print "timeguard: ok\n";
'

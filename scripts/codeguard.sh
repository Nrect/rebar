#!/usr/bin/env bash
# Страж границ процесса (docs/CORRECTNESS.md, §14; CONVENTIONS §12): у всего,
# что ждёт, — срок; у всего, что читается, — потолок; у горутины — владелец.
# Проверяет Go-код прода всего репозитория (без _test.go и testdata).
#
#   client — http.DefaultClient, http.Get/Head/Post/PostForm, литерал
#     http.Client без Timeout;
#   server — литерал http.Server без любого из ReadHeaderTimeout, ReadTimeout,
#     WriteTimeout, IdleTimeout;
#   body — io.ReadAll, json.NewDecoder или io.Copy по .Body без
#     io.LimitReader или http.MaxBytesReader в том же вызове;
#   goroutine — любой оператор go: владелец, recover и ожидание статически не
#     видны, поэтому каждая горутина прода — запись в списке с причиной.
#
# Строки, руны и комментарии вырезаются до проверки.
#
# Исключения — scripts/codeguard.allow: путь, строка кода и причина через
# табуляцию. Запись, которой больше нет в коде, — тоже отказ: починенное
# вычёркивают.
set -euo pipefail
cd "$(dirname "$0")/.."
exec perl -e '
use strict;
use warnings;

my %allow;
my %used;
open my $af, "<", "scripts/codeguard.allow" or die "нет scripts/codeguard.allow\n";
while (my $l = <$af>) {
  chomp $l;
  next if $l =~ /^\s*(#|$)/;
  my ($path, $code, $why) = split /\t/, $l, 3;
  die "codeguard.allow: у записи нет причины: $l\n" unless defined $why && $why =~ /\S/;
  $allow{"$path\t$code"} = 1;
}

my @violations;
sub check_allow {
  my ($path, $code, $line_no, $what) = @_;
  my $key = "$path\t$code";
  if ($allow{$key}) { $used{$key} = 1; return }
  push @violations, "$path:$line_no: $what: $code";
}

# blank — строки, руны и комментарии Go заменяются пробелами; переводы строк
# остаются, смещения и номера строк не съезжают.
sub blank {
  my ($s) = @_;
  my $out = "";
  my ($i, $n) = (0, length $s);
  while ($i < $n) {
    my $c = substr($s, $i, 1);
    my $two = substr($s, $i, 2);
    my $j;
    if ($two eq "//") {
      $j = index($s, "\n", $i); $j = $n if $j < 0;
    } elsif ($two eq "/*") {
      $j = index($s, "*/", $i + 2); $j = $j < 0 ? $n : $j + 2;
    } elsif ($c eq "`") {
      $j = index($s, "`", $i + 1); $j = $j < 0 ? $n : $j + 1;
    } elsif ($c eq "\"" || $c eq "\x27") {
      $j = $i + 1;
      while ($j < $n) {
        my $d = substr($s, $j, 1);
        if ($d eq "\\") { $j += 2; next }
        last if $d eq $c || $d eq "\n";
        $j++;
      }
      $j++;
    }
    if (defined $j) {
      my $chunk = substr($s, $i, $j - $i);
      $chunk =~ s/[^\n]/ /g;
      $out .= $chunk;
      $i = $j;
    } else {
      $out .= $c;
      $i++;
    }
  }
  return $out;
}

# closing — индекс парной скобки к открывающей на $open.
sub closing {
  my ($s, $open) = @_;
  my %pair = ("{" => "}", "(" => ")");
  my $o = substr($s, $open, 1);
  my $cl = $pair{$o};
  my $depth = 0;
  for (my $k = $open; $k < length $s; $k++) {
    my $ch = substr($s, $k, 1);
    if ($ch eq $o) { $depth++ }
    elsif ($ch eq $cl) { $depth--; return $k if $depth == 0 }
  }
  return length($s) - 1;
}

my @files = sort map { chomp; s{^\./}{}; $_ }
  `find . -path ./.git -prune -o -path ./.claude -prune -o -path "*/testdata/*" -prune -o -name "*.go" ! -name "*_test.go" -print`;

for my $f (@files) {
  open my $fh, "<", $f or die "$f: $!\n";
  local $/;
  my $raw = <$fh>;
  close $fh;
  my @lines = split /\n/, $raw, -1;
  my $code = blank($raw);
  my $lineof = sub { return 1 + (substr($code, 0, shift) =~ tr/\n//) };
  my $text_of = sub { (my $t = $lines[shift() - 1] // "") =~ s/^\s+|\s+$//g; return $t };
  my $report = sub {
    my ($pos, $what) = @_;
    my $ln = $lineof->($pos);
    check_allow($f, $text_of->($ln), $ln, $what);
  };

  while ($code =~ /\bhttp\.(?:DefaultClient\b|(?:Get|Head|Post|PostForm)\s*\()/g) {
    $report->($-[0], "клиент без таймаута: http.DefaultClient и его обёртки");
  }
  while ($code =~ /\bhttp\.Client\{/g) {
    my ($start, $open) = ($-[0], $+[0] - 1);
    my $body = substr($code, $open, closing($code, $open) - $open + 1);
    $report->($start, "клиент без Timeout") unless $body =~ /\bTimeout\s*:/;
  }
  while ($code =~ /\bhttp\.Server\{/g) {
    my ($start, $open) = ($-[0], $+[0] - 1);
    my $body = substr($code, $open, closing($code, $open) - $open + 1);
    my @missing = grep { $body !~ /\b$_\s*:/ } qw(ReadHeaderTimeout ReadTimeout WriteTimeout IdleTimeout);
    $report->($start, "сервер без таймаутов " . join(", ", @missing)) if @missing;
  }
  while ($code =~ /\b(?:io\.ReadAll|json\.NewDecoder|io\.Copy(?:N|Buffer)?)\s*\(/g) {
    my ($start, $open) = ($-[0], $+[0] - 1);
    my $args = substr($code, $open, closing($code, $open) - $open + 1);
    next unless $args =~ /\.Body\b/;
    next if $args =~ /\b(?:io\.LimitReader|http\.MaxBytesReader)\s*\(/;
    $report->($start, "тело без потолка: io.LimitReader или http.MaxBytesReader");
  }
  while ($code =~ /(?:^|[;{}])\s*go\s+(?=[\w(])/mg) {
    my $pos = $+[0];
    $report->($pos, "горутина прода: нужна запись с владельцем, recover и ожиданием");
  }
}

my @stale = grep { !$used{$_} } sort keys %allow;
if (@violations || @stale) {
  print STDERR "codeguard: $_\n" for @violations;
  print STDERR "codeguard: запись разрешения больше не нужна, вычеркнуть: $_\n" for @stale;
  exit 1;
}
print "codeguard: ok\n";
'

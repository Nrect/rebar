module github.com/nrect/rebar/kit

go 1.26.0

// Пин патч-версии stdlib: govulncheck проверяет ту stdlib, которой собран
// модуль. Пакеты kit ходят в net/http, net/url и crypto/rand, поэтому патчи
// закрываются здесь так же, как в mail. Бампается вместе с патч-релизами Go;
// еженедельный vuln-scan напомнит.
toolchain go1.26.6

require github.com/stretchr/testify v1.12.1

require go.yaml.in/yaml/v3 v3.0.5 // indirect

module github.com/nrect/rebar/mail

go 1.25.0

// Пин патч-версии stdlib: govulncheck проверяет ту stdlib, которой собран
// модуль. 1.26.6 закрывает net/http, crypto/tls, net/url и encoding/asn1
// (GO-2026-5026/5972/6089/6090/6218), которые sesv2 и httptest вызывают.
// Бампается вместе с патч-релизами Go; еженедельный vuln-scan напомнит.
toolchain go1.26.6

require (
	github.com/google/uuid v1.6.0
	github.com/stretchr/testify v1.12.1
)

require go.yaml.in/yaml/v3 v3.0.5 // indirect

module github.com/nrect/rebar/mail

go 1.25.0

// Пин патч-версии stdlib: net/mail до 1.26.3 уязвим (GO-2026-4977/4986),
// crypto/tls и encoding/asn1 до 1.26.5 (GO-2026-6090/5972) — оба на пути
// STARTTLS адаптера smtp. govulncheck проверяет ту stdlib, которой собран
// модуль. Бампается вместе с патч-релизами Go; еженедельный vuln-scan напомнит.
toolchain go1.26.6

require (
	github.com/google/uuid v1.6.0
	github.com/stretchr/testify v1.12.1
)

require go.yaml.in/yaml/v3 v3.0.5 // indirect

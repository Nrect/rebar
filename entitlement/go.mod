module github.com/nrect/rebar/entitlement

go 1.26.0

// Пин патч-версии stdlib: govulncheck проверяет ту stdlib, которой собран
// модуль, а версия одна на все модули тулкита (VERSIONING, «Единая версия
// Go»). Бампается вместе с патч-релизами Go.
toolchain go1.26.6

require (
	github.com/google/uuid v1.6.0
	github.com/stretchr/testify v1.12.1
)

require go.yaml.in/yaml/v3 v3.0.5 // indirect

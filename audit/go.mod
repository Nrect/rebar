module github.com/nrect/rebar/audit

go 1.26.0

// Пин патч-версии stdlib: govulncheck проверяет ту stdlib, которой собран
// модуль. Патч тот же, что у соседей по репозиторию (lockstep).
toolchain go1.26.6

require github.com/google/uuid v1.6.0

require (
	github.com/stretchr/testify v1.12.1
	go.yaml.in/yaml/v3 v3.0.5 // indirect
)

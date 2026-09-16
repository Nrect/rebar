module github.com/nrect/rebar/inbox

go 1.26.0

// Пин патч-версии stdlib: govulncheck проверяет ту stdlib, которой собран
// модуль. Патч тот же, что у соседей по репозиторию (lockstep).
toolchain go1.26.6

require (
	github.com/nrect/rebar/kit v0.3.0
	github.com/stretchr/testify v1.12.1
)

require go.yaml.in/yaml/v3 v3.0.5 // indirect

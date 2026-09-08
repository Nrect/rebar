module github.com/nrect/rebar/auth

go 1.26.0

// Пин патч-версии stdlib: govulncheck проверяет ту stdlib, которой собран
// модуль, а версия одна на все модули тулкита (VERSIONING, «Единая версия
// Go»). Бампается вместе с патч-релизами Go; еженедельный vuln-scan напомнит.
toolchain go1.26.6

require (
	github.com/google/uuid v1.6.0
	github.com/trustelem/zxcvbn v1.0.1
	golang.org/x/crypto v0.56.0
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/dlclark/regexp2 v1.12.0 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/stretchr/testify v1.12.1 // indirect
	github.com/test-go/testify v1.1.4 // indirect
	golang.org/x/sys v0.47.0 // indirect
)

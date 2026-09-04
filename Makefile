# Репозиторий многомодульный: каждый пакет — свой go.mod. Все цели обходят
# модули по одному, как их увидит потребитель; go.work нужен только редактору
# и локальной сборке между модулями.
MODULES := $(shell find . -name go.mod -not -path './.git/*' -exec dirname {} \; | sort)

.PHONY: ci lint test test-unit govulncheck modules hooks fmt

modules:
	@for m in $(MODULES); do echo $$m; done

# Полный гейт — то же, что блокирует CI.
ci: lint govulncheck
	@for m in $(MODULES); do \
		echo "== race tests: $$m"; \
		(cd $$m && CGO_ENABLED=1 go test -race -count=1 -timeout=600s ./...) || exit 1; \
	done
	@echo "CI gate passed locally"

lint:
	@for m in $(MODULES); do \
		echo "== lint: $$m"; \
		(cd $$m && golangci-lint run ./...) || exit 1; \
	done

fmt:
	@for m in $(MODULES); do (cd $$m && golangci-lint fmt ./...); done

test:
	@for m in $(MODULES); do \
		echo "== test: $$m"; \
		(cd $$m && go test -count=1 -cover -timeout=300s ./...) || exit 1; \
	done

# Без Docker: интеграционные тесты пропускаются по -short.
test-unit:
	@for m in $(MODULES); do \
		(cd $$m && go test -count=1 -short -timeout=120s ./...) || exit 1; \
	done

govulncheck:
	@for m in $(MODULES); do \
		echo "== govulncheck: $$m"; \
		(cd $$m && go run golang.org/x/vuln/cmd/govulncheck@latest ./...) || exit 1; \
	done

hooks:
	git config core.hooksPath .githooks
	@echo "Git hooks configured (.githooks/)"

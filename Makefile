# Репозиторий многомодульный: каждый пакет — свой go.mod. Все цели обходят
# модули по одному, как их увидит потребитель; go.work нужен только редактору
# и локальной сборке между модулями.
MODULES := $(shell find . -name go.mod -not -path './.git/*' -exec dirname {} \; | sort)

# Цели одного модуля: MODULE=mail. MUTANTS_EXCLUDE — список -E для gremlins
# (адаптеры и cmd мутируются впустую: их держат интеграционные тесты).
MUTANTS_EXCLUDE ?=

.PHONY: ci lint test test-unit govulncheck modules hooks fmt lockstep chip-check mutants

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

# Единая версия Go и общих зависимостей во всех модулях. Тот же джоб в CI.
lockstep:
	@scripts/toolchain-lockstep.sh
	@scripts/deps-lockstep.sh

# Гейт чипа перед сдачей: один модуль так, как его увидит потребитель
# (GOWORK=off — соседний модуль через workspace не подтянется).
chip-check:
	@test -n "$(MODULE)" || { echo "нужен MODULE=<каталог>, например: make chip-check MODULE=mail"; exit 1; }
	@test -f "$(MODULE)/go.mod" || { echo "$(MODULE)/go.mod не найден"; exit 1; }
	@echo "== lint: $(MODULE)"
	@cd $(MODULE) && GOWORK=off golangci-lint run ./...
	@echo "== go vet: $(MODULE)"
	@cd $(MODULE) && GOWORK=off go vet ./...
	@echo "== race tests: $(MODULE)"
	@cd $(MODULE) && GOWORK=off CGO_ENABLED=1 go test -race -count=1 -timeout=600s ./...
	@echo "== govulncheck: $(MODULE)"
	@cd $(MODULE) && GOWORK=off go run golang.org/x/vuln/cmd/govulncheck@latest ./...
	@echo "chip-check passed: $(MODULE)"

# Мутационное тестирование ядра модуля — локально, не в CI: медленно.
# Коэффициент таймаута 20 обязателен: на меньшем прогон врёт зелёным, объявляя
# выживших мутантов «не покрытыми», потому что тест не успел за окно.
mutants:
	@test -n "$(MODULE)" || { echo "нужен MODULE=<каталог>, например: make mutants MODULE=mail"; exit 1; }
	@test -f "$(MODULE)/go.mod" || { echo "$(MODULE)/go.mod не найден"; exit 1; }
	cd $(MODULE) && gremlins unleash --timeout-coefficient 20 --workers 4 $(MUTANTS_EXCLUDE)

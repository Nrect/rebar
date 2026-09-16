# Репозиторий многомодульный: каждый пакет — свой go.mod. Все цели обходят
# .claude/worktrees исключён: в основном checkout'е там лежат рабочие копии
# незавершённых веток, и гейт линтовал бы чужую работу вместо своей.
# модули по одному, как их увидит потребитель; go.work нужен только редактору
# и локальной сборке между модулями.
MODULES := $(shell find . -name go.mod -not -path './.git/*' -not -path './.claude/*' -exec dirname {} \; | sort)

# Цели одного модуля: MODULE=mail. MUTANTS_EXCLUDE — список -E для gremlins
# (адаптеры и cmd мутируются впустую: их держат интеграционные тесты).
MUTANTS_EXCLUDE ?=

# Тесты идут под поясом не UTC, с получасовым смещением и летним временем: код,
# который тайком зависит от пояса машины, падает здесь, а не у потребителя
# (docs/CORRECTNESS.md, §8). Тот же пояс — в CI.
TEST_TZ := America/St_Johns

.PHONY: ci lint test test-unit govulncheck modules hooks fmt lockstep timeguard chip-check mutants

modules:
	@for m in $(MODULES); do echo $$m; done

# Полный гейт — то же, что блокирует CI.
ci: lint govulncheck timeguard
	@for m in $(MODULES); do \
		echo "== race tests: $$m"; \
		(cd $$m && TZ=$(TEST_TZ) CGO_ENABLED=1 go test -race -count=1 -timeout=600s ./...) || exit 1; \
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
		(cd $$m && TZ=$(TEST_TZ) go test -count=1 -cover -timeout=300s ./...) || exit 1; \
	done

# Без Docker: интеграционные тесты пропускаются по -short.
test-unit:
	@for m in $(MODULES); do \
		(cd $$m && TZ=$(TEST_TZ) go test -count=1 -short -timeout=120s ./...) || exit 1; \
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

# Всё время — UTC и параметром: миграции и код прода всех модулей. Тот же джоб
# в CI; исключения с причиной — scripts/timeguard.allow.
timeguard:
	@scripts/timeguard.sh

# Гейт чипа перед сдачей: один модуль так, как его увидит потребитель
# (GOWORK=off — соседний модуль через workspace не подтянется).
chip-check:
	@test -n "$(MODULE)" || { echo "нужен MODULE=<каталог>, например: make chip-check MODULE=mail"; exit 1; }
	@test -f "$(MODULE)/go.mod" || { echo "$(MODULE)/go.mod не найден"; exit 1; }
	@echo "== lint: $(MODULE)"
	@cd $(MODULE) && GOWORK=off golangci-lint run ./...
	@echo "== go vet: $(MODULE)"
	@cd $(MODULE) && GOWORK=off go vet ./...
	@echo "== timeguard"
	@scripts/timeguard.sh
	@echo "== race tests: $(MODULE) (TZ=$(TEST_TZ))"
	@cd $(MODULE) && GOWORK=off TZ=$(TEST_TZ) CGO_ENABLED=1 go test -race -count=1 -timeout=600s ./...
	@echo "== govulncheck: $(MODULE)"
	@cd $(MODULE) && GOWORK=off go run golang.org/x/vuln/cmd/govulncheck@latest ./...
	@echo "chip-check passed: $(MODULE)"

# Мутационное тестирование — только на мощном ПК: не в CI (медленно) и не на
# ноутбуке — там прогон вытесняет остальную работу и врёт таймаутами
# (docs/CHIP.md, «Порог»). HEAVY_PC=1 — подтверждение, что машина та.
# GOWORK=off обязателен: пока модуль не внесён в go.work, прогон падает на
# «directory prefix . does not contain modules listed in go.work», а go.work
# правит арбитр уже при слиянии — то есть у чипа его нет по построению.
# Коэффициент таймаута 20 обязателен: на меньшем прогон врёт зелёным, объявляя
# выживших мутантов «не покрытыми», потому что тест не успел за окно.
mutants:
	@test -n "$(HEAVY_PC)" || { echo "мутанты гоняются только на мощном ПК (docs/CHIP.md, «Порог»); там: make mutants HEAVY_PC=1 MODULE=<каталог>"; exit 1; }
	@test -n "$(MODULE)" || { echo "нужен MODULE=<каталог>, например: make mutants HEAVY_PC=1 MODULE=mail"; exit 1; }
	@test -f "$(MODULE)/go.mod" || { echo "$(MODULE)/go.mod не найден"; exit 1; }
	cd $(MODULE) && GOWORK=off gremlins unleash --timeout-coefficient 20 --workers 4 $(MUTANTS_EXCLUDE)

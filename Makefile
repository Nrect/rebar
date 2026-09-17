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

.PHONY: ci lint test test-unit govulncheck modules hooks fmt lockstep guards timeguard sqlguard codeguard ciguard secrets chip-check mutants

modules:
	@for m in $(MODULES); do echo $$m; done

# Полный гейт — то же, что блокирует CI.
ci: lint govulncheck guards
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

# Стражи правил репозитория — тот же джоб в CI; у каждого исключения с причиной
# в scripts/<страж>.allow, устаревшая запись роняет стража.
guards: timeguard sqlguard codeguard ciguard

# Всё время — UTC и параметром: миграции и код прода (CORRECTNESS §8).
timeguard:
	@scripts/timeguard.sh

# Миграции: COLLATE "C" у текста, lock_timeout, схема меняется на живой базе
# (CORRECTNESS §12 и §13).
sqlguard:
	@scripts/sqlguard.sh

# Код прода: таймауты клиента и сервера, потолок тела, горутины с владельцем
# (CORRECTNESS §14).
codeguard:
	@scripts/codeguard.sh

# CI и дерево: действия по SHA, permissions у джобов, пути без двойников по
# регистру (SECURITY.md, «Цепочка поставки»).
ciguard:
	@scripts/ciguard.sh

# Секреты в дереве — тот же образ и конфиг, что у джоба CI secrets. Нужен
# Docker; каталог должен быть виден демону (у colima — внутри $HOME).
GITLEAKS_IMAGE := ghcr.io/gitleaks/gitleaks:v8.30.1@sha256:c00b6bd0aeb3071cbcb79009cb16a60dd9e0a7c60e2be9ab65d25e6bc8abbb7f
secrets:
	@docker run --rm -v "$(CURDIR):/repo" $(GITLEAKS_IMAGE) dir /repo --config /repo/.gitleaks.toml --redact --no-banner

# Гейт чипа перед сдачей: один модуль так, как его увидит потребитель
# (GOWORK=off — соседний модуль через workspace не подтянется).
chip-check:
	@test -n "$(MODULE)" || { echo "нужен MODULE=<каталог>, например: make chip-check MODULE=mail"; exit 1; }
	@test -f "$(MODULE)/go.mod" || { echo "$(MODULE)/go.mod не найден"; exit 1; }
	@echo "== lint: $(MODULE)"
	@cd $(MODULE) && GOWORK=off golangci-lint run ./...
	@echo "== go vet: $(MODULE)"
	@cd $(MODULE) && GOWORK=off go vet ./...
	@echo "== guards"
	@scripts/timeguard.sh
	@scripts/sqlguard.sh
	@scripts/codeguard.sh
	@scripts/ciguard.sh
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

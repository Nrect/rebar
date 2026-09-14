package pglock

// Разбор мутантов (gremlins, CONVENTIONS §5).
//
// Прогон — из корня репозитория; мутируется только pglock, корень модуля и
// pgtest исключены:
//
//	TEST_DATABASE_URL=postgres://… make mutants MODULE=postgres \
//	  MUTANTS_EXCLUDE='-E "^pgtest/" -E "^[^/]+\.go$$"'
//
// ЛОВУШКА: без TEST_DATABASE_URL каждый мутант поднимает свой контейнер (урок
// корня модуля — mutants_internal_test.go в postgres).
//
// Итог 2026-09-14: Killed 22, Lived 0, Not covered 2, Timed out 0, 28 с.
//
// Два NOT COVERED — артефакт профиля покрытия, а не дыра: константы
// attemptTimeout и releaseTimeout (pglock.go:25, :28) в профиль не попадают, и
// мутанта `*` → `/` gremlins не запускает. Проверено подстановкой руками: оба
// дают бюджет 0 и роняют по девять тестов — попытка истекает сразу
// (TestWrap_ParallelRunsOfOneJob_ExactlyOneEffect), снятие не проходит и прогон
// отдаёт ErrLockLost (TestWrap_ReturnsConnectionToPool).
//
// Один KILLED не настоящий: pglock.go:92:40 — `+` → `-` в keyDomain + name на
// строках не компилируется, а gremlins засчитал сборку убийством.
//
// Мимо прогона: CONDITIONALS_NEGATION не мутирует `!`. Гварды `!acquired`,
// `!unlocked` и `!validName` сторожат TestWrap_ParallelRunsOfOneJob_ExactlyOneEffect,
// TestRelease_NotHeld_DestroysConnectionAndReports и TestWrap_NameIsClosedSet.

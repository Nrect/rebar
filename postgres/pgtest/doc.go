// Package pgtest — настоящий Postgres для интеграционных тестов: свежая база
// на прогон тестового бинаря и своя схема на тест.
//
// Start зовут из TestMain. Если задан TEST_DATABASE_URL, база прогона
// заводится на этом сервере (CREATE DATABASE) — так тесты идут в CI, где
// Postgres уже поднят сервисом; иначе поднимается контейнер testcontainers,
// один на бинарь. Schema даёт каждому тесту свою схему через search_path,
// поэтому тесты можно писать параллельными; SchemaDSN отдаёт ту же схему
// строкой соединения — для теста, который поднимает пул сам, как приложение.
//
// Каталог миграций блока накатывает ApplyUp (секции Up по возрастанию номера)
// и откатывает ApplyDown (Down по убыванию); CheckMigrations сторожит сам
// каталог, SchemaObjects называет, что осталось в схеме после отката.
//
//	func TestMain(m *testing.M) {
//		flag.Parse() // testing.Short() до m.Run требует разобранных флагов
//		if testing.Short() {
//			os.Exit(m.Run()) // интеграционные тесты пропустят себя сами
//		}
//		ctx := context.Background()
//		db, err := pgtest.Start(ctx, pgtest.Options{})
//		if err != nil {
//			fmt.Fprintln(os.Stderr, err)
//			os.Exit(1)
//		}
//		code := m.Run()
//		db.Close(ctx)
//		os.Exit(code)
//	}
//
//	func TestStore(t *testing.T) {
//		pgtest.Short(t)
//		pool := pgtest.Schema(t, db)
//		pgtest.ApplyUp(t, pool, xpg.Migrations()) // один файл — Apply с GooseUp
//		…
//	}
//
//	func TestMigrations(t *testing.T) {
//		pgtest.CheckMigrations(t, xpg.Migrations()) // база не нужна
//		pgtest.Short(t)
//		pool := pgtest.Schema(t, db)
//		pgtest.ApplyUp(t, pool, xpg.Migrations())
//		pgtest.ApplyDown(t, pool, xpg.Migrations())
//		pgtest.ApplyDown(t, pool, xpg.Migrations()) // Down идемпотентен
//		assert.Empty(t, pgtest.SchemaObjects(t, pool))
//	}
//
//	func TestApp(t *testing.T) {
//		pgtest.Short(t)
//		app := start(t, pgtest.SchemaDSN(t, db)) // пул поднимает приложение
//		…
//	}
//
// Пакет импортирует testing в обычных файлах — он для тестов и предназначен;
// в продовый бинарь его подключать незачем.
//
// Безопасность:
//
//  1. СВЕЖАЯ БАЗА НА ПРОГОН. Тесты не наследуют чужих строк, и зелёный
//     результат не зависит от того, что оставил предыдущий прогон.
//  2. ЧУЖИЕ ОБЪЕКТЫ НЕ ТРОГАЮТСЯ. По TEST_DATABASE_URL у разработчика может
//     стоять рабочая база: пакет делает CREATE DATABASE и DROP только своей,
//     TRUNCATE и DROP чужого нет нигде.
//  3. SWEEP ТОЛЬКО СВОЕГО ПРЕФИКСА И ТОЛЬКО ПО РАЗБИРАЕМОЙ МЕТКЕ ВРЕМЕНИ.
//     База с чужим именем или с неразбираемой меткой пропускается, DROP идёт
//     без FORCE — живой параллельный прогон не роняется.
//  4. ИМЯ БАЗЫ ГЕНЕРИРУЕТСЯ ЗДЕСЬ И ПРОВЕРЯЕТСЯ ([a-z0-9_]) перед подстановкой
//     в DDL: имя базы параметром не передать.
//  5. ОБРАЗ ПИНУЕТСЯ DIGEST'ОМ, а пароль контейнера случаен на прогон: порт
//     контейнера открыт на localhost.
//  6. ПОТОЛОК СОЕДИНЕНИЙ НА ПУЛ (5 по умолчанию): сумма пулов параллельных
//     тестов остаётся ниже max_connections сервера.
//  7. -short ПРОПУСКАЕТ БЕЗ DOCKER, PGTEST_KEEP=1 оставляет базу и контейнер
//     уликами упавшего теста.
//  8. КАТАЛОГ С НАХОДКАМИ НЕ НАКАТЫВАЕТСЯ: из дубля номера или файла без номера
//     тест собрал бы не ту последовательность, что применит goose.
//
// Чего нет (решения, не пробелы): раннера миграций и зависимости от goose —
// ApplyUp накатывает секции без таблицы версий, маркеры goose — комментарии
// SQL, а Migrate для Start потребитель передаёт сам; фикстур и снапшотов —
// данные заводит тест, иначе они молча расходятся со схемой; других СУБД.
package pgtest

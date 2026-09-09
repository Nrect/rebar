// Package authpg — Postgres за портами session.Sessions и session.Attempts
// плюс половина порта session.Tokens.
//
// Store — сессии и счётчик попыток целиком: New(pool), WithTx(tx), запросы на
// postgres.Querier. Store.Tokens() (или NewTokens(pool)) — работа с таблицей
// auth_tokens: Insert, ConsumeRow, RevokeOfSubject, PurgeExpired.
//
// Схему накатывает потребитель своим раннером; schema.sql копируется в его
// миграции как есть, CheckSchema на старте сверяет и НИЧЕГО не меняет.
//
// # Порт Tokens дописывает потребитель
//
// Пакет не владеет его таблицей пользователей и транзакции в контексте не
// носит (ADR-0001), поэтому атомарность живёт там, где видны обе таблицы, —
// у него. Это около двадцати пяти строк поверх postgres.Runner:
//
//	type tokens struct {
//		run *postgres.Runner
//		pg  *authpg.Tokens
//	}
//
//	func (t *tokens) Issue(ctx context.Context, row session.OneTimeToken, n session.Notification) error {
//		return t.run.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
//			if _, err := t.pg.WithTx(tx).RevokeOfSubject(ctx, row.Realm, row.SubjectID, row.Purpose, row.CreatedAt); err != nil {
//				return err
//			}
//			if err := t.pg.WithTx(tx).Insert(ctx, row); err != nil {
//				return err
//			}
//			env, err := mail.Prepare(letterOf(n)) // шаблон и адрес — его дело
//			if err != nil {
//				return err
//			}
//			_, err = t.outbox.WithTx(tx).Enqueue(ctx, env) // письмо тем же коммитом
//			return err
//		})
//	}
//
//	func (t *tokens) Consume(ctx context.Context, req session.ConsumeRequest) (session.ConsumeResult, error) {
//		var res session.ConsumeResult
//		err := t.run.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
//			var err error
//			if res, err = t.pg.WithTx(tx).ConsumeRow(ctx, req); err != nil {
//				return err
//			}
//			return t.applyEffect(ctx, tx, req, res) // UPDATE его users: verified, hash, login
//		})
//		return res, err
//	}
//
// Revoke и PurgeExpired делегируются в t.pg напрямую: транзакция им не нужна.
//
// Безопасность:
//
//  1. СОДЕРЖИМОЕ СТРОКИ НЕ ПОПАДАЕТ В ОШИБКУ. В pgconn.PgError.Detail лежит
//     «Failing row contains (…)» — вся строка, то есть ключ сессии и
//     нормализованный логин. Границу держит общий postgres.Sanitize, а не
//     своя копия: пять копий это пять шансов разойтись (errors.go).
//  2. РЕАЛМ СТОИТ В КАЖДОМ WHERE. HMAC под секретом реалма уже разводит хэши,
//     но колонка realm — вторая линия и единственный ключ уборки: запрос без
//     неё в двухреалмовом процессе обслуживает чужие строки, с виду работая.
//     Держится TestSQL_HasRealmInEveryWhere.
//  3. ОДНОРАЗОВОСТЬ РЕШАЕТ БАЗА, А НЕ GO. ConsumeRow — один UPDATE с
//     предикатом used_at IS NULL AND expires_at > $now и RETURNING: пара
//     «прочитал, потом записал» отдала бы токен обоим участникам гонки
//     (TestConsume_IsOnceUnderRace).
//  4. ВРЕМЯ ПРИХОДИТ ПАРАМЕТРОМ, без DEFAULT now() в колонках, которые пишет
//     домен: иначе тесты на управляемых часах проверяют одно, а база пишет
//     другое.
//  5. ВНЕШНИХ КЛЮЧЕЙ НА ТАБЛИЦУ ПОЛЬЗОВАТЕЛЕЙ НЕТ. Пакет не знает её имени;
//     FK добавляет потребитель своей миграцией, если захочет.
//  6. ИМЕНА ОГРАНИЧЕНИЙ И ИНДЕКСОВ — КОНТРАКТ. Они выданы ADR-0003 и
//     проверяются CheckSchema: переименование ломает потребителя молча.
//  7. WithTx — ЕДИНСТВЕННЫЙ ПУТЬ К АТОМАРНОСТИ. Строка токена, письмо и
//     эффект обязаны жить и умирать вместе; вставка мимо транзакции
//     компилируется, проходит тесты и оставляет ссылки в никуда
//     (TestStore_WithTx_IsAtomic).
//  8. ЛОГОВ В БИБЛИОТЕКЕ НЕТ. Что записать о сбое, решает потребитель: у него
//     есть и request_id, и правила ретеншна.
//
// Чего в пакете нет (решения, не пробелы): раннера миграций и автомиграции —
// две правды о схеме, DDL-права у приложения и гонка реплик при выкате;
// таблицы пользователей; реализации Tokens целиком — см. выше; метрик — они
// декоратором у потребителя (CONVENTIONS §6); своих ретраев — их даёт
// postgres.Runner.InTxRetry, и решать, что идемпотентно, обязан вызывающий.
package authpg

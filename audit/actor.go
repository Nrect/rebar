package audit

import "context"

// actorKey — тип ключа свой и неэкспортируемый: чужой пакет не подменит
// актора, положив значение по совпавшему строковому ключу.
type actorKey struct{}

// NewContext кладёт актора в контекст. Зовёт обвязка входа СРАЗУ ПОСЛЕ
// аутентификации: субъект, названный в теле запроса, — это то, что о себе
// сказал проверяемый, и журналу он не годится.
func NewContext(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

// ActorFrom — актор из контекста; ok = false, если его не клали.
//
// ЛОЖНОГО «АНОНИМА» ЗДЕСЬ НЕТ. Забытая обвязка тихо превратила бы весь журнал
// в записи без субъекта, и заметить это было бы нечем: всё работает. Поэтому
// не положенный актор — ErrNoActor в Prepare, а аноним ставится явно
// (ActorAnonymous).
func ActorFrom(ctx context.Context) (Actor, bool) {
	a, ok := ctx.Value(actorKey{}).(Actor)
	return a, ok
}

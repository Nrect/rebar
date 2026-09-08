package outbox

import (
	"maps"
	"slices"
)

// Registry — карта «тип сообщения → хендлер». Собирается на старте, до
// NewWorker: воркер снимает с него список типов один раз и дальше не
// перечитывает.
type Registry struct {
	handlers map[Kind]Handler
}

// NewRegistry — пустой реестр.
func NewRegistry() *Registry {
	return &Registry{handlers: map[Kind]Handler{}}
}

// Register паникует на негодном типе, nil-хендлере и повторной регистрации:
// ошибка сборки обязана падать на старте. Тихая замена хендлера означала бы,
// что порядок вызовов в main решает, какой код выполнит платёж.
func (r *Registry) Register(kind Kind, h Handler) {
	if !kind.valid() {
		panic("outbox.Registry.Register: kind " + string(kind) + " must match [a-z0-9_.]{1,64}")
	}
	if h == nil {
		panic("outbox.Registry.Register: handler for kind " + string(kind) + " must not be nil")
	}
	if _, dup := r.handlers[kind]; dup {
		panic("outbox.Registry.Register: kind " + string(kind) + " is already registered")
	}
	r.handlers[kind] = h
}

// Kinds — зарегистрированные типы в лексикографическом порядке.
func (r *Registry) Kinds() []Kind {
	return slices.Sorted(maps.Keys(r.handlers))
}

func (r *Registry) handler(kind Kind) (Handler, bool) {
	h, ok := r.handlers[kind]
	return h, ok
}

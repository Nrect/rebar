package objectstoretest

import (
	"context"
	"maps"
	"slices"
	"sync"
)

// MemOwned — objectstore.Owned в памяти: набор ключей, на которые у
// потребителя есть строка. Потокобезопасен целиком, включая настройку.
type MemOwned struct {
	mu    sync.Mutex
	owned map[string]bool
	err   error
	// calls — сколько раз спросили; по нему видно, дошёл ли прогон до ключа.
	calls int
}

// NewMemOwned — источник владения для перечисленных ключей.
func NewMemOwned(keys ...string) *MemOwned {
	owned := make(map[string]bool, len(keys))
	for _, key := range keys {
		owned[key] = true
	}
	return &MemOwned{owned: owned}
}

// SetErr — ошибка IsOwned: ею проверяется, что прогон сборщика
// ОСТАНАВЛИВАЕТСЯ, а не считает сиротами всех; nil снимает. Приходит голой:
// источник владения пишет потребитель, класс даёт обёртка сборщика.
func (o *MemOwned) SetErr(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.err = err
}

// IsOwned — есть ли у потребителя строка на этот ключ.
func (o *MemOwned) IsOwned(_ context.Context, key string) (bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls++
	if o.err != nil {
		return false, o.err
	}
	return o.owned[key], nil
}

// Own отмечает ключ принадлежащим потребителю.
func (o *MemOwned) Own(key string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.owned[key] = true
}

// Calls — сколько раз спросили о владении.
func (o *MemOwned) Calls() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.calls
}

// Keys — ключи, числящиеся за потребителем, копией.
func (o *MemOwned) Keys() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Sorted(maps.Keys(o.owned))
}

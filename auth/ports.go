package auth

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Identities — таблица пользователей потребителя за портом. Реализация — его:
// у каждого свой профиль, свои связи и свои внешние ключи, поэтому таблица
// пользователей в пакете отсутствует осознанно (ADR-0003).
//
// Время приходит параметром, без now() внутри адаптера: иначе тесты ядра на
// управляемых часах проверяют одно, а база пишет другое (CONVENTIONS §9).
type Identities interface {
	// ByLogin ищет по логину, ПРОШЕДШЕМУ NormalizeLogin. Своей нормализации
	// адаптер не делает: вторая точка нормализации разъедется с первой, и
	// сохранённое перестанет находиться. Ошибка ErrIdentityNotFound — «нет
	// такой строки», а не сбой: вход отвечает на неё так же, как на неверный
	// пароль.
	ByLogin(ctx context.Context, login string) (Identity, error)
	// ByID ищет по идентификатору; ErrIdentityNotFound — отсутствие строки.
	ByID(ctx context.Context, id uuid.UUID) (Identity, error)
	// Create заводит личность и возвращает её идентификатор. Логин приходит
	// ПРОШЕДШИМ NormalizeLogin, и уникальный индекс потребитель строит на
	// сохранённой колонке, а не на выражении в SQL. Занятый логин —
	// ErrLoginTaken, разобранный по ИМЕНИ уникального индекса, а не по
	// SQLSTATE (CONVENTIONS §9): один SQLSTATE 23505 приходит на любой
	// уникальный индекс таблицы потребителя.
	Create(ctx context.Context, login, passwordHash string, at time.Time) (uuid.UUID, error)
	// SetPasswordHash подменяет хэш. Отзыв сессий и токенов сброса — дело
	// вызывающего: порт видит только таблицу пользователей.
	SetPasswordHash(ctx context.Context, id uuid.UUID, hash string, at time.Time) error
}

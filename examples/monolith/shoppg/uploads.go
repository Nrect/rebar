package shoppg

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/nrect/rebar/objectstore"
	"github.com/nrect/rebar/postgres"
)

const (
	insertUploadSQL = `INSERT INTO shop_uploads
(object_key, subject_id, original_name, content_type, size_bytes, created_at)
VALUES ($1, $2, $3, $4, $5, $6)`

	countUploadSQL = `SELECT EXISTS (SELECT 1 FROM shop_uploads WHERE object_key = $1)`
)

// Uploads — таблица загруженных файлов и порт objectstore.Owned.
//
// ИМЯ ФАЙЛА ПОЛЬЗОВАТЕЛЯ ЛЕЖИТ ЗДЕСЬ, А НЕ В КЛЮЧЕ. Ключ строит objectstore из
// своего uuid; имя — данные, и показывать его надо из этой колонки
// (ADR-0006, инвариант 5).
type Uploads struct {
	db postgres.Querier
}

var _ objectstore.Owned = (*Uploads)(nil)

// NewUploads — адаптер на пуле.
func NewUploads(db *DB) *Uploads { return &Uploads{db: db.Pool} }

// Record запоминает загруженный объект.
func (s *Uploads) Record(ctx context.Context, obj objectstore.Object, subjectID uuid.UUID,
	originalName string, at time.Time,
) error {
	_, err := s.db.Exec(ctx, insertUploadSQL, obj.Key, subjectID, originalName,
		obj.ContentType, obj.Size, utc(at))
	return storeError("запись загрузки", err)
}

// IsOwned — есть ли строка, ссылающаяся на объект.
//
// ОШИБКА ОЗНАЧАЕТ «НЕ ЗНАЮ», А НЕ «СИРОТА»: уборщик, принявший сбой базы за
// отсутствие владельца, вычистил бы живые файлы (objectstore/ports.go).
func (s *Uploads) IsOwned(ctx context.Context, key string) (bool, error) {
	var owned bool
	if err := s.db.QueryRow(ctx, countUploadSQL, key).Scan(&owned); err != nil {
		return false, storeError("владелец объекта", err)
	}
	return owned, nil
}

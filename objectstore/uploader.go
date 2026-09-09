package objectstore

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/google/uuid"
)

// UploadRequest — то, что прислал клиент. Всё, кроме тела, — подсказки.
type UploadRequest struct {
	Body io.Reader
	// Size — заявленный размер; -1 означает «неизвестен». Заявке верят только
	// в сторону отказа: слишком большой размер даёт ErrTooLarge, не читая тело.
	Size int64
	// ContentType — заголовок клиента. НА РЕШЕНИЕ НЕ ВЛИЯЕТ (инвариант 2);
	// поле оставлено, чтобы вызывающему было куда его положить и чтобы было
	// видно, что тип определяется не им.
	ContentType string
	// Filename — имя файла пользователя. ДАННЫЕ, А НЕ ПУТЬ: в ключ не попадает
	// никогда и не логируется. Нужно показывать исходное имя — потребитель
	// хранит его у себя в колонке (инвариант 5).
	Filename string
}

// Uploader — единственный вопрос «можно ли это принять»: потолок размера, тип
// по содержимому, отказ SVG, ключ нашей постройки.
type Uploader struct {
	store Store
	cfg   UploaderConfig
	newID func() uuid.UUID
}

// NewUploader паникует на nil store и негодном Config: ошибка конфигурации
// обязана падать на старте, а не на первой загрузке.
func NewUploader(store Store, cfg UploaderConfig) *Uploader {
	if store == nil {
		panic("objectstore.NewUploader: store must not be nil")
	}
	if err := cfg.validate(); err != nil {
		panic("objectstore.NewUploader: " + err.Error())
	}
	return &Uploader{store: store, cfg: cfg, newID: uuid.New}
}

// SetIDs подменяет источник ключей; только для тестов, до начала обслуживания.
func (u *Uploader) SetIDs(newID func() uuid.UUID) { u.newID = newID }

// Upload принимает файл и кладёт его под ключом нашей постройки.
//
// Ошибки: ErrEmptyBody, ErrTooLarge, ErrSVGRejected, ErrUnsupportedType,
// ErrUnavailable. Ни одна не несёт имени файла и ключа.
func (u *Uploader) Upload(ctx context.Context, req UploadRequest) (Object, error) {
	body, err := u.readBody(req)
	if err != nil {
		return Object{}, err
	}
	head := body
	if len(head) > SniffLen {
		head = head[:SniffLen]
	}
	if looksLikeSVG(head) {
		return Object{}, ErrSVGRejected
	}
	// Расширение и белый список спрашиваются одной веткой: тип без расширения
	// принять нечем, и отдельная ветка на него была бы кодом, до которого
	// нельзя дойти.
	contentType := detect(head)
	ext, known := contentType.Ext()
	if !known || !u.cfg.accepts(contentType) {
		return Object{}, fmt.Errorf("%w: %s", ErrUnsupportedType, contentType)
	}

	obj, err := u.store.Put(ctx, PutRequest{
		Key:         buildKey(u.cfg.Prefix, u.newID(), ext),
		ContentType: string(contentType),
		Body:        bytes.NewReader(body),
		Size:        int64(len(body)),
	})
	if err != nil {
		// Ошибка адаптера может нести ключ и путь — наружу уходит класс.
		return Object{}, fmt.Errorf("%w: put", ErrUnavailable)
	}
	return obj, nil
}

// readBody — тело целиком, но не больше MaxSize+1 байта.
//
// ПОТОЛОК ДЕЙСТВУЕТ ДО ЧТЕНИЯ, А НЕ ПОСЛЕ. Заявленный размер отсекает лишнее,
// не тронув тело вовсе; всё остальное читается через io.LimitReader на
// MaxSize+1, и превышение узнаётся по лишнему байту. Проверка после чтения —
// это проверка после того, как память уже занята (ADR-0006, инвариант 1).
func (u *Uploader) readBody(req UploadRequest) ([]byte, error) {
	if req.Body == nil {
		return nil, ErrEmptyBody
	}
	if req.Size > u.cfg.MaxSize {
		return nil, fmt.Errorf("%w: declared %d bytes, max is %d", ErrTooLarge, req.Size, u.cfg.MaxSize)
	}
	// Ошибка источника не заворачивается: у сетевого тела и у *os.File в её
	// тексте лежит путь, то есть имя файла пользователя.
	body, err := io.ReadAll(io.LimitReader(req.Body, u.cfg.MaxSize+1))
	if err != nil {
		return nil, fmt.Errorf("%w: body is unreadable", ErrUnavailable)
	}
	if int64(len(body)) > u.cfg.MaxSize {
		return nil, fmt.Errorf("%w: body is over %d bytes", ErrTooLarge, u.cfg.MaxSize)
	}
	if len(body) == 0 {
		return nil, ErrEmptyBody
	}
	return body, nil
}

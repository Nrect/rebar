package s3

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/nrect/rebar/objectstore"
)

// Store — objectstore.Store поверх S3-совместимого хранилища на stdlib.
type Store struct {
	cfg    Config
	sign   signer
	client *http.Client
	now    func() time.Time
}

// New паникует на негодном Config: ошибка конфигурации обязана падать на
// старте, а не на первой загрузке.
func New(cfg Config) *Store {
	if err := cfg.validate(); err != nil {
		panic("objectstore/s3.New: " + err.Error())
	}
	return &Store{
		cfg: cfg,
		sign: signer{
			accessKeyID: cfg.AccessKeyID,
			secret:      cfg.SecretKey,
			region:      cfg.Region,
			service:     service,
		},
		client: cfg.client(),
		now:    func() time.Time { return time.Now().UTC() },
	}
}

// SetClock подменяет источник времени; только для тестов, до начала работы.
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// Put кладёт объект, перезаписывая существующий.
//
// РАЗМЕР ОБЯЗАН БЫТЬ ИЗВЕСТЕН: подпись считается по sha256 тела, то есть тело
// приходится прочитать целиком, а «читать сколько дадут» у чужого источника —
// это отказ по памяти. Потолок держит objectstore.Uploader, он же и приходит
// сюда с готовым размером.
func (s *Store) Put(ctx context.Context, req objectstore.PutRequest) (objectstore.Object, error) {
	if err := objectstore.CheckKey(req.Key); err != nil {
		return objectstore.Object{}, err
	}
	if req.Size < 0 {
		return objectstore.Object{}, objectstore.ErrSizeUnknown
	}
	body, err := readExactly(req.Body, req.Size)
	if err != nil {
		return objectstore.Object{}, err
	}

	httpReq, err := s.newRequest(ctx, http.MethodPut, s.objectURL(req.Key), bytes.NewReader(body))
	if err != nil {
		return objectstore.Object{}, err
	}
	if req.ContentType != "" {
		httpReq.Header.Set(headerContentType, req.ContentType)
	}
	httpReq.ContentLength = req.Size
	resp, err := s.do(httpReq, sha256Hex(body), "put")
	if err != nil {
		return objectstore.Object{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return objectstore.Object{}, statusError("put", resp)
	}
	return objectstore.Object{
		Key:         req.Key,
		Size:        req.Size,
		ContentType: req.ContentType,
		ETag:        resp.Header.Get("ETag"),
		ModifiedAt:  responseDate(resp, s.now()),
	}, nil
}

// Delete удаляет объект; отсутствие объекта — не ошибка (S3 отвечает 204).
func (s *Store) Delete(ctx context.Context, key string) error {
	if err := objectstore.CheckKey(key); err != nil {
		return err
	}
	req, err := s.newRequest(ctx, http.MethodDelete, s.objectURL(key), http.NoBody)
	if err != nil {
		return err
	}
	resp, err := s.do(req, emptyHash, "delete")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return statusError("delete", resp)
	}
	return nil
}

// listResult — ответ ListObjectsV2.
type listResult struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	IsTruncated           bool     `xml:"IsTruncated"`
	NextContinuationToken string   `xml:"NextContinuationToken"`
	Contents              []struct {
		Key          string    `xml:"Key"`
		Size         int64     `xml:"Size"`
		LastModified time.Time `xml:"LastModified"`
		ETag         string    `xml:"ETag"`
	} `xml:"Contents"`
}

// List — ListObjectsV2 по префиксу. Непозитивный limit — пустая страница без
// ошибки: у пагинации «нечего отдать» не сбой.
func (s *Store) List(ctx context.Context, prefix, cursor string, limit int) (objectstore.Page, error) {
	if limit <= 0 {
		return objectstore.Page{}, nil
	}
	u := s.bucketURL()
	query := url.Values{
		"list-type": {"2"},
		"max-keys":  {strconv.Itoa(limit)},
	}
	if prefix != "" {
		query.Set("prefix", prefix)
	}
	if cursor != "" {
		query.Set("continuation-token", cursor)
	}
	u.RawQuery = canonicalQuery(query)

	parsed, err := s.listPage(ctx, u)
	if err != nil {
		return objectstore.Page{}, err
	}
	page := objectstore.Page{Objects: make([]objectstore.Object, 0, len(parsed.Contents))}
	for _, item := range parsed.Contents {
		page.Objects = append(page.Objects, objectstore.Object{
			Key: item.Key, Size: item.Size, ETag: item.ETag, ModifiedAt: item.LastModified.UTC(),
		})
	}
	if parsed.IsTruncated {
		page.Cursor = parsed.NextContinuationToken
	}
	return page, nil
}

func (s *Store) listPage(ctx context.Context, u *url.URL) (listResult, error) {
	req, err := s.newRequest(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return listResult{}, err
	}
	resp, err := s.do(req, emptyHash, "list")
	if err != nil {
		return listResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return listResult{}, statusError("list", resp)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxListBody))
	if err != nil {
		return listResult{}, fmt.Errorf("%w: list: body is unreadable", objectstore.ErrUnavailable)
	}
	var parsed listResult
	if err = xml.Unmarshal(body, &parsed); err != nil {
		return listResult{}, fmt.Errorf("%w: list: response is not ListBucketResult", objectstore.ErrUnavailable)
	}
	return parsed, nil
}

// maxListBody — потолок ответа List: страница ограничена max-keys, но читать
// у чужой стороны неограниченно нельзя.
const maxListBody = 8 << 20

// Presign — ссылка, подписанная query-строкой.
func (s *Store) Presign(_ context.Context, key string, method objectstore.Method, ttl time.Duration) (string, error) {
	if err := objectstore.CheckKey(key); err != nil {
		return "", err
	}
	if !method.Valid() {
		return "", objectstore.ErrBadMethod
	}
	if ttl <= 0 || ttl > objectstore.MaxPresignTTL {
		return "", objectstore.ErrBadTTL
	}
	verb := http.MethodGet
	if method == objectstore.MethodPut {
		verb = http.MethodPut
	}
	return s.sign.presign(verb, s.objectURL(key), ttl, s.now()), nil
}

// PublicURL — постоянная ссылка. Она не делает объект публичным: права бакета
// — работа инфраструктуры (ADR-0006, «Чего нет»).
func (s *Store) PublicURL(key string) string {
	return s.cfg.publicBase() + "/" + (&url.URL{Path: key}).EscapedPath()
}

// emptyHash — sha256 пустого тела.
const emptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func (s *Store) newRequest(ctx context.Context, method string, u *url.URL, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		// Адрес собран нами из проверенного Config и проверенного ключа.
		return nil, fmt.Errorf("%w: request could not be built", objectstore.ErrUnavailable)
	}
	return req, nil
}

// do подписывает запрос и отправляет его. ПОДПИСЬ ПЕРВОЙ: неподписанный запрос
// не должен стоить нам соединения.
func (s *Store) do(req *http.Request, payloadHash, op string) (*http.Response, error) {
	req.Header.Set(headerContentSHA256, payloadHash)
	s.sign.sign(req, payloadHash, s.now())
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, transportError(op)
	}
	return resp, nil
}

func (s *Store) bucketURL() *url.URL {
	u, _ := url.Parse(s.cfg.Endpoint)
	u.Path = "/" + s.cfg.Bucket
	return u
}

func (s *Store) objectURL(key string) *url.URL {
	u := s.bucketURL()
	u.Path = "/" + s.cfg.Bucket + "/" + key
	return u
}

// readExactly читает ровно size байт. Больше — тело врёт о размере и подпись
// разойдётся с телом; меньше — тоже.
func readExactly(body io.Reader, size int64) ([]byte, error) {
	if body == nil || size == 0 {
		return nil, objectstore.ErrEmptyBody
	}
	buf, err := io.ReadAll(io.LimitReader(body, size+1))
	if err != nil {
		return nil, fmt.Errorf("%w: body is unreadable", objectstore.ErrUnavailable)
	}
	if int64(len(buf)) != size {
		return nil, fmt.Errorf("%w: body is %d bytes, PutRequest.Size says %d",
			objectstore.ErrSizeUnknown, len(buf), size)
	}
	return buf, nil
}

// responseDate — Date ответа; при его отсутствии — наши часы.
func responseDate(resp *http.Response, fallback time.Time) time.Time {
	if parsed, err := http.ParseTime(resp.Header.Get("Date")); err == nil {
		return parsed.UTC()
	}
	return fallback.UTC()
}

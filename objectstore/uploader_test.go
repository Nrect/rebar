package objectstore_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/objectstore"
	"github.com/nrect/rebar/objectstore/objectstoretest"
)

// ИНВАРИАНТ 1. Потолок размера действует ДО чтения, а не после: тело
// оборачивается io.LimitReader(body, MaxSize+1), и превышение узнаётся по
// лишнему байту. Проверка после чтения — это проверка после того, как память
// уже занята (ADR-0006).
func TestUploader_RejectsOversizeBeforeReadingBody(t *testing.T) {
	t.Parallel()
	up, store := newUploader(t, testUploaderConfig())
	source := &countingReader{limit: 100 * testMaxSize}

	_, err := up.Upload(t.Context(), objectstore.UploadRequest{Body: source, Size: -1})

	require.ErrorIs(t, err, objectstore.ErrTooLarge)
	assert.LessOrEqual(t, source.Count(), int64(testMaxSize+1),
		"прочитано %d байт при потолке %d: тело читается целиком, и отказ приходит после того, как память занята",
		source.Count(), testMaxSize)
	assert.Empty(t, store.Keys(), "отвергнутое тело в хранилище не попадает")
}

// Заявленный размер отсекает лишнее, не тронув тело ВОВСЕ: у клиента,
// честно назвавшего размер, читать нечего.
func TestUploader_RejectsDeclaredOversizeWithoutReadingAtAll(t *testing.T) {
	t.Parallel()
	up, _ := newUploader(t, testUploaderConfig())
	source := &countingReader{limit: 100 * testMaxSize}

	_, err := up.Upload(t.Context(), objectstore.UploadRequest{Body: source, Size: testMaxSize + 1})

	require.ErrorIs(t, err, objectstore.ErrTooLarge)
	assert.Zero(t, source.Count(), "тело читалось, хотя размер объявлен и он больше потолка")
}

// Тело ровно в потолок проходит: сдвиг границы внутрь запрещает законную
// загрузку, наружу — пропускает то, ради чего потолок и заведён.
func TestUploader_ExactlyAtLimitIsAccepted(t *testing.T) {
	t.Parallel()
	up, store := newUploader(t, testUploaderConfig())

	obj, err := up.Upload(t.Context(), objectstore.UploadRequest{
		Body: bytes.NewReader(objectstoretest.PNG(testMaxSize)), Size: -1,
	})

	require.NoError(t, err)
	assert.Equal(t, int64(testMaxSize), obj.Size)
	assert.Len(t, store.Keys(), 1)
}

func TestUploader_OneByteOverLimitIsRejected(t *testing.T) {
	t.Parallel()
	up, _ := newUploader(t, testUploaderConfig())

	_, err := up.Upload(t.Context(), objectstore.UploadRequest{
		Body: bytes.NewReader(objectstoretest.PNG(testMaxSize + 1)), Size: -1,
	})

	assert.ErrorIs(t, err, objectstore.ErrTooLarge)
}

// ИНВАРИАНТ 2. Тип определяется по содержимому; заголовок клиента — подсказка.
func TestUploader_TypeComesFromBodyNotHeader(t *testing.T) {
	t.Parallel()
	up, store := newUploader(t, testUploaderConfig())

	// Клиент утверждает PDF (его нет в Accept), в теле PNG — принимаем как PNG.
	obj, err := up.Upload(t.Context(), objectstore.UploadRequest{
		Body: bytes.NewReader(objectstoretest.PNG(128)), Size: -1, ContentType: "application/pdf",
	})

	require.NoError(t, err)
	assert.Equal(t, string(objectstore.ContentTypePNG), obj.ContentType)
	assert.True(t, strings.HasSuffix(keyOf(t, store), ".png"), "расширение — из определённого типа")

	// Обратный случай: клиент утверждает PNG, в теле обычный текст — отказ.
	_, err = up.Upload(t.Context(), objectstore.UploadRequest{
		Body: bytes.NewReader(objectstoretest.Text(128)), Size: -1, ContentType: "image/png",
	})
	assert.ErrorIs(t, err, objectstore.ErrUnsupportedType)
}

// ИНВАРИАНТ 3. SVG отвергается ВСЕГДА — это документ со скриптами, а не
// картинка; отдав его со своего домена, мы отдаём чужой JS в своей origin.
// Отказ не обходится ни заголовком, ни сдвигом тега вглубь файла.
func TestUploader_RejectsSVGRegardlessOfHeader(t *testing.T) {
	t.Parallel()
	headers := []string{"", "image/png", "image/jpeg", "image/svg+xml", "text/plain"}
	bodies := map[string][]byte{
		"голый":             objectstoretest.SVG(),
		"за прологом":       objectstoretest.SVGAfterProlog(),
		"верхним регистром": bytes.ToUpper(objectstoretest.SVG()),
	}
	for name, body := range bodies {
		for _, header := range headers {
			t.Run(name+"/"+header, func(t *testing.T) {
				t.Parallel()
				up, store := newUploader(t, objectstore.UploaderConfig{
					Prefix: testPrefix, MaxSize: testMaxSize,
					// Даже когда потребитель принимает всё, что пакет умеет.
					Accept: objectstore.AllContentTypes,
				})

				_, err := up.Upload(t.Context(), objectstore.UploadRequest{
					Body: bytes.NewReader(body), Size: -1, ContentType: header,
				})

				require.ErrorIs(t, err, objectstore.ErrSVGRejected,
					"SVG прошёл с заголовком %q: инвариант обходится подсказкой клиента", header)
				assert.Empty(t, store.Keys())
			})
		}
	}
}

// Поля «принимать SVG» в конфиге нет, и добавить его списком тоже нельзя:
// image/svg+xml не входит в AllContentTypes, а Config паникует на чужом типе.
func TestUploaderConfig_SVGCannotBeAcceptedByConfiguration(t *testing.T) {
	t.Parallel()

	assert.NotContains(t, objectstore.AllContentTypes, objectstore.ContentType("image/svg+xml"))
	assert.PanicsWithValue(t,
		"objectstore.NewUploader: UploaderConfig.Accept: type \"image/svg+xml\" must be one of "+
			"[image/jpeg image/png image/gif image/webp application/pdf]",
		func() {
			objectstore.NewUploader(objectstoretest.NewMemStore(), objectstore.UploaderConfig{
				Prefix: testPrefix, MaxSize: testMaxSize,
				Accept: []objectstore.ContentType{"image/svg+xml"},
			})
		})
}

// ИНВАРИАНТЫ 4 И 5. Ключ строим мы: <prefix>/<uuid>.<ext>. Имя файла
// пользователя не попадает в ключ никогда — в нём бывают ../, символы,
// ломающие подпись, и персональные данные.
func TestUploader_KeyNeverContainsUserFilename(t *testing.T) {
	t.Parallel()
	filenames := []string{
		"../../etc/passwd.png",
		"Иванов Иван, паспорт 4506 №123456.png",
		"photo.png",
		strings.Repeat("a", 300) + ".png",
		"..%2f..%2fetc%2fshadow.png",
	}
	for _, filename := range filenames {
		t.Run(filename[:min(len(filename), 24)], func(t *testing.T) {
			t.Parallel()
			up, store := newUploader(t, testUploaderConfig())

			obj, err := up.Upload(t.Context(), objectstore.UploadRequest{
				Body: bytes.NewReader(objectstoretest.PNG(128)), Size: -1, Filename: filename,
			})

			require.NoError(t, err)
			want := testPrefix + "/" + fixedID.String() + ".png"
			assert.Equal(t, want, obj.Key, "ключ строим мы, и только мы")
			assert.Equal(t, want, keyOf(t, store))
			assert.False(t, containsAny(obj.Key, filename, strings.TrimSuffix(filename, ".png"), "passwd", "Иванов", "photo"),
				"имя файла пользователя просочилось в ключ %q", obj.Key)
		})
	}
}

// Имя файла не попадает и в ТЕКСТ ОШИБКИ: ошибка уходит в лог, а имя файла
// бывает персональными данными (CORRECTNESS §10).
func TestUploader_ErrorNeverContainsUserFilename(t *testing.T) {
	t.Parallel()
	up, _ := newUploader(t, testUploaderConfig())
	const filename = "Иванов Иван, паспорт 4506 №123456.svg"

	_, err := up.Upload(t.Context(), objectstore.UploadRequest{
		Body: bytes.NewReader(objectstoretest.SVG()), Size: -1, Filename: filename,
	})

	require.Error(t, err)
	assert.NotContains(t, err.Error(), filename)
	assert.NotContains(t, err.Error(), "Иванов")
}

func TestUploader_AcceptListIsAWhitelist(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		body    []byte
		wantExt string
		wantErr error
	}{
		"png принят":        {body: objectstoretest.PNG(128), wantExt: ".png"},
		"jpeg принят":       {body: objectstoretest.JPEG(128), wantExt: ".jpg"},
		"gif не в Accept":   {body: objectstoretest.GIF(128), wantErr: objectstore.ErrUnsupportedType},
		"webp не в Accept":  {body: objectstoretest.WebP(128), wantErr: objectstore.ErrUnsupportedType},
		"pdf не в Accept":   {body: objectstoretest.PDF(128), wantErr: objectstore.ErrUnsupportedType},
		"текст не картинка": {body: objectstoretest.Text(128), wantErr: objectstore.ErrUnsupportedType},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			up, store := newUploader(t, testUploaderConfig())

			obj, err := up.Upload(t.Context(), objectstore.UploadRequest{Body: bytes.NewReader(tc.body), Size: -1})

			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				assert.Empty(t, store.Keys())
				return
			}
			require.NoError(t, err)
			assert.True(t, strings.HasSuffix(obj.Key, tc.wantExt), "ключ %q, ожидалось расширение %q", obj.Key, tc.wantExt)
		})
	}
}

func TestUploader_RejectsEmptyBody(t *testing.T) {
	t.Parallel()
	up, store := newUploader(t, testUploaderConfig())

	// Цикл, а не параллельные подтесты: проверка ПОСЛЕ него — про состояние
	// хранилища, и она обязана выполниться последней.
	for name, req := range map[string]objectstore.UploadRequest{
		"nil":            {Body: nil, Size: -1},
		"пустое":         {Body: bytes.NewReader(nil), Size: -1},
		"нулевой размер": {Body: bytes.NewReader(nil), Size: 0},
	} {
		_, err := up.Upload(t.Context(), req)

		require.ErrorIsf(t, err, objectstore.ErrEmptyBody, "тело %s", name)
	}
	assert.Empty(t, store.Keys())
}

// Сбой хранилища не рассказывает, что именно лежало: наружу уходит класс.
func TestUploader_StoreFailureHidesDetails(t *testing.T) {
	t.Parallel()
	store := objectstoretest.NewMemStore()
	store.Err = errors.New("dial tcp 10.0.0.7:9000: connect: connection refused, key=uploads/secret.png")
	up := objectstore.NewUploader(store, testUploaderConfig())

	_, err := up.Upload(t.Context(), objectstore.UploadRequest{
		Body: bytes.NewReader(objectstoretest.PNG(128)), Size: -1,
	})

	require.ErrorIs(t, err, objectstore.ErrUnavailable)
	assert.NotContains(t, err.Error(), "secret.png")
	assert.NotContains(t, err.Error(), "10.0.0.7")
}

func TestNewUploader_PanicsOnNilStore(t *testing.T) {
	t.Parallel()

	assert.PanicsWithValue(t, "objectstore.NewUploader: store must not be nil", func() {
		objectstore.NewUploader(nil, testUploaderConfig())
	})
}

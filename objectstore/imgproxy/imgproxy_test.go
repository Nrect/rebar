package imgproxy_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/objectstore/imgproxy"
)

const (
	testKey  = "943b421c9eb07c830af81030552c86009268de4e532ba2ee2eab8247c6da0881"
	testSalt = "520f986b998545b4785e0defbc4f3c1203f22de2374a3d53cb7a7fe9fea309c5"
	testObj  = "uploads/11111111-2222-3333-4444-555555555555.png"
	// Источник в base64url — то, что уходит в путь: s3://catalog/uploads/...
	testSource = "czM6Ly9jYXRhbG9nL3VwbG9hZHMvMTExMTExMTEtMjIyMi0zMzMzLTQ0NDQtNTU1NTU1NTU1NTU1LnBuZw"
)

func newSigner(t *testing.T) *imgproxy.Signer {
	t.Helper()
	return imgproxy.New(imgproxy.Config{
		BaseURL: "https://img.example.com/", SourceBase: "s3://catalog/",
		Key: testKey, Salt: testSalt,
	})
}

// Полная ссылка целиком, включая подпись: ожидание посчитано openssl, а не
// этим кодом.
func TestSignedURL_PinsTheWholeLink(t *testing.T) {
	t.Parallel()
	s := newSigner(t)

	link := s.SignedURL(testObj, imgproxy.Options{
		Resize: imgproxy.ResizeFill, Width: 300, Height: 400, Quality: 80, Extension: "webp",
	})

	assert.Equal(t,
		"https://img.example.com/nnh_N0f8JvmPMLLsIXAfBEfeZrNA9hj6M5mc5JrC4yA"+
			"/rs:fill:300:400:0/q:80/"+testSource+".webp",
		link)
}

// Нулевые Options — ссылка на исходник, а не молчаливый ресайз наугад.
func TestSignedURL_ZeroOptionsMeanNoTransform(t *testing.T) {
	t.Parallel()
	s := newSigner(t)

	link := s.SignedURL(testObj, imgproxy.Options{})

	assert.Equal(t, "https://img.example.com/w3FmM8ikoqFCka6r3Ps39IrFyZ5-12VA_Chuw1-Ow5s/"+testSource, link)
	assert.NotContains(t, link, "rs:")
	assert.NotContains(t, link, "q:")
}

// ПОДПИСЬ ПОКРЫВАЕТ ВЕСЬ ПУТЬ. Иначе получатель ссылки подставил бы в неё
// чужой источник и превратил бы наш imgproxy в открытый прокси, ходящий по
// внутренней сети.
func TestSignedURL_SignatureCoversSourceAndOptions(t *testing.T) {
	t.Parallel()
	s := newSigner(t)
	base := signatureOf(t, s.SignedURL(testObj, imgproxy.Options{Resize: imgproxy.ResizeFit, Width: 100, Height: 100}))

	changes := map[string]string{
		"другой объект":   s.SignedURL("uploads/other.png", imgproxy.Options{Resize: imgproxy.ResizeFit, Width: 100, Height: 100}),
		"другой размер":   s.SignedURL(testObj, imgproxy.Options{Resize: imgproxy.ResizeFit, Width: 101, Height: 100}),
		"другой режим":    s.SignedURL(testObj, imgproxy.Options{Resize: imgproxy.ResizeFill, Width: 100, Height: 100}),
		"другой формат":   s.SignedURL(testObj, imgproxy.Options{Resize: imgproxy.ResizeFit, Width: 100, Height: 100, Extension: "jpg"}),
		"другое качество": s.SignedURL(testObj, imgproxy.Options{Resize: imgproxy.ResizeFit, Width: 100, Height: 100, Quality: 50}),
	}
	for name, link := range changes {
		assert.NotEqualf(t, base, signatureOf(t, link), "%s: подпись не изменилась", name)
	}
}

// Ключ и соль — секреты: в ссылку они не попадают ни в каком виде.
func TestSignedURL_NeverLeaksKeyOrSalt(t *testing.T) {
	t.Parallel()
	s := newSigner(t)

	link := s.SignedURL(testObj, imgproxy.Options{Resize: imgproxy.ResizeFit, Width: 300, Height: 300})

	assert.NotContains(t, link, testKey)
	assert.NotContains(t, link, testSalt)
}

func TestSignedURL_PanicsOnImpossibleOptions(t *testing.T) {
	t.Parallel()
	s := newSigner(t)
	cases := map[string]imgproxy.Options{
		"чужой режим":          {Resize: "crop"},
		"отрицательная ширина": {Resize: imgproxy.ResizeFit, Width: -1},
		"ширина за потолком":   {Resize: imgproxy.ResizeFit, Width: imgproxy.MaxDimension + 1},
		"высота за потолком":   {Resize: imgproxy.ResizeFit, Height: imgproxy.MaxDimension + 1},
		"качество за сотней":   {Quality: 101},
		"качество ниже нуля":   {Quality: -1},
		"расширение с точкой":  {Extension: ".webp"},
		"расширение со слэшем": {Extension: "web/p"},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Panics(t, func() { s.SignedURL(testObj, opts) })
		})
	}
}

func TestNew_PanicsOnBadConfig(t *testing.T) {
	t.Parallel()
	good := imgproxy.Config{
		BaseURL: "https://img.example.com", SourceBase: "s3://catalog", Key: testKey, Salt: testSalt,
	}
	cases := map[string]func(*imgproxy.Config){
		"нулевой конфиг":  func(c *imgproxy.Config) { *c = imgproxy.Config{} },
		"пустая база":     func(c *imgproxy.Config) { c.BaseURL = "" },
		"база без схемы":  func(c *imgproxy.Config) { c.BaseURL = "img.example.com" },
		"пустой источник": func(c *imgproxy.Config) { c.SourceBase = "" },
		"пустой ключ":     func(c *imgproxy.Config) { c.Key = "" },
		"ключ не hex":     func(c *imgproxy.Config) { c.Key = "не-шестнадцатеричный" },
		"пустая соль":     func(c *imgproxy.Config) { c.Salt = "" },
		"соль не hex":     func(c *imgproxy.Config) { c.Salt = "zzz" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := good
			mutate(&cfg)

			assert.Panics(t, func() { imgproxy.New(cfg) })
		})
	}
}

// Текст паники не печатает значений ключа и соли: паника уходит в лог.
func TestNew_PanicDoesNotPrintSecrets(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		require.NotNil(t, r)
		assert.NotContains(t, r, testKey)
		assert.NotContains(t, r, testSalt)
	}()

	imgproxy.New(imgproxy.Config{
		BaseURL: "https://img.example.com", SourceBase: "s3://catalog",
		Key: testKey + "zz", Salt: testSalt,
	})
}

func TestAllResizes_IsComplete(t *testing.T) {
	t.Parallel()

	assert.Equal(t,
		[]imgproxy.Resize{imgproxy.ResizeFit, imgproxy.ResizeFill, imgproxy.ResizeAuto},
		imgproxy.AllResizes)
}

// signatureOf — подпись из ссылки: первый сегмент после базы.
func signatureOf(t *testing.T, link string) string {
	t.Helper()
	rest, ok := strings.CutPrefix(link, "https://img.example.com/")
	require.True(t, ok, "ссылка %q не начинается с базы", link)
	signature, _, ok := strings.Cut(rest, "/")
	require.True(t, ok)
	return signature
}

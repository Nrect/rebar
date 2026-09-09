package imgproxy

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Resize — режим вписывания. Закрытый набор: значение уходит в подписанный
// путь, а оттуда в чужие кэши и логи.
type Resize string

const (
	// ResizeFit — вписать целиком, сохранив пропорции.
	ResizeFit Resize = "fit"
	// ResizeFill — заполнить кадр, обрезав лишнее.
	ResizeFill Resize = "fill"
	// ResizeAuto — imgproxy выбирает сам по соотношению сторон.
	ResizeAuto Resize = "auto"
)

// AllResizes — полный список; держит guard-тест.
var AllResizes = []Resize{ResizeFit, ResizeFill, ResizeAuto}

func (r Resize) valid() bool { return r == ResizeFit || r == ResizeFill || r == ResizeAuto }

// MaxDimension — потолок стороны в пикселях. Не украшение: сторона из внешнего
// мира без потолка — это заказ на отрисовку картинки в гигапиксель.
const MaxDimension = 10000

// Config — адрес imgproxy, база источников и ключи подписи.
type Config struct {
	// BaseURL — адрес imgproxy: https://img.example.com.
	BaseURL string
	// SourceBase — база, из которой imgproxy берёт исходник: s3://bucket или
	// публичная база бакета. Ключ объекта приписывается к ней.
	SourceBase string
	// Key и Salt — hex, как их принимает сам imgproxy (IMGPROXY_KEY,
	// IMGPROXY_SALT). СЕКРЕТЫ: не логируются и не попадают в ошибки.
	Key  string
	Salt string
}

func (c Config) validate() error {
	if c.BaseURL == "" {
		return errors.New("Config.BaseURL must not be empty")
	}
	if !strings.HasPrefix(c.BaseURL, "http://") && !strings.HasPrefix(c.BaseURL, "https://") {
		return errors.New("Config.BaseURL must start with http:// or https://")
	}
	if c.SourceBase == "" {
		return errors.New("Config.SourceBase must not be empty")
	}
	if c.Key == "" {
		return errors.New("Config.Key must not be empty")
	}
	if c.Salt == "" {
		return errors.New("Config.Salt must not be empty")
	}
	if _, err := hex.DecodeString(c.Key); err != nil {
		// Текст называет поле и требование, но не значение: значение — секрет.
		return errors.New("Config.Key must be hex, as IMGPROXY_KEY")
	}
	if _, err := hex.DecodeString(c.Salt); err != nil {
		return errors.New("Config.Salt must be hex, as IMGPROXY_SALT")
	}
	return nil
}

// Options — что сделать с картинкой. Нулевое значение — «ничего не делать»:
// ссылка на исходник, а не молчаливый ресайз наугад.
type Options struct {
	Resize Resize
	Width  int
	Height int
	// Quality — 1..100; ноль означает «как настроен imgproxy».
	Quality int
	// Extension — формат на выходе ("webp", "jpg"); пусто — как у исходника.
	Extension string
}

func (o Options) validate() error {
	if o.Resize != "" && !o.Resize.valid() {
		return fmt.Errorf("Options.Resize must be one of %v", AllResizes)
	}
	if o.Width < 0 || o.Width > MaxDimension {
		return fmt.Errorf("Options.Width must be between 0 and %d", MaxDimension)
	}
	if o.Height < 0 || o.Height > MaxDimension {
		return fmt.Errorf("Options.Height must be between 0 and %d", MaxDimension)
	}
	if o.Quality < 0 || o.Quality > 100 {
		return errors.New("Options.Quality must be between 0 and 100")
	}
	if o.Extension != "" && !isBareWord(o.Extension) {
		return errors.New("Options.Extension must be a bare word like webp or jpg")
	}
	return nil
}

// Signer — подписывает ссылки на преобразование.
type Signer struct {
	baseURL    string
	sourceBase string
	key        []byte
	salt       []byte
}

// New паникует на негодном Config: ошибка конфигурации обязана падать на
// старте, а не на первой ссылке.
func New(cfg Config) *Signer {
	if err := cfg.validate(); err != nil {
		panic("objectstore/imgproxy.New: " + err.Error())
	}
	key, _ := hex.DecodeString(cfg.Key)
	salt, _ := hex.DecodeString(cfg.Salt)
	return &Signer{
		baseURL:    strings.TrimSuffix(cfg.BaseURL, "/"),
		sourceBase: strings.TrimSuffix(cfg.SourceBase, "/"),
		key:        key,
		salt:       salt,
	}
}

// SignedURL — подписанная ссылка на преобразование объекта key.
//
// ПОДПИСЬ ПОКРЫВАЕТ ВЕСЬ ПУТЬ, включая источник и параметры: иначе получатель
// ссылки подставил бы в неё чужой адрес и превратил бы наш imgproxy в
// открытый прокси, ходящий по внутренней сети.
//
// Паникует на негодных Options: они приходят из кода, а не от пользователя, и
// молчаливая правка «наугад» дала бы не ту картинку без единого признака.
func (s *Signer) SignedURL(key string, opts Options) string {
	if err := opts.validate(); err != nil {
		panic("objectstore/imgproxy.SignedURL: " + err.Error())
	}
	path := s.path(key, opts)
	return s.baseURL + "/" + s.sign(path) + path
}

// sign — HMAC-SHA256 по salt и пути, base64url без набивки. Схема — та же, что
// в эталонной реализации самого imgproxy (examples/signature.go в его
// репозитории); её и сторожит вектор в тестах.
func (s *Signer) sign(path string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write(s.salt)
	mac.Write([]byte(path))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// path — часть ссылки после подписи; источник кодируется base64url, чтобы его
// собственные слэши и query не разъезжались с разбором imgproxy.
func (s *Signer) path(key string, opts Options) string {
	var b strings.Builder
	if opts.Resize != "" || opts.Width > 0 || opts.Height > 0 {
		mode := opts.Resize
		if mode == "" {
			mode = ResizeFit
		}
		b.WriteString("/rs:" + string(mode) + ":" + strconv.Itoa(opts.Width) + ":" + strconv.Itoa(opts.Height) + ":0")
	}
	if opts.Quality > 0 {
		b.WriteString("/q:" + strconv.Itoa(opts.Quality))
	}
	b.WriteString("/" + base64.RawURLEncoding.EncodeToString([]byte(s.sourceBase+"/"+key)))
	if opts.Extension != "" {
		b.WriteString("." + opts.Extension)
	}
	return b.String()
}

// isBareWord — только буквы и цифры: расширение уходит в путь, и точка или
// слэш в нём меняли бы разбор ссылки.
func isBareWord(s string) bool {
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return s != ""
}

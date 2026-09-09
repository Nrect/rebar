package imgproxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Вектор — эталонная реализация самого imgproxy
// (github.com/imgproxy/imgproxy, examples/signature.go): те же ключ, соль и
// путь. Ожидаемая подпись посчитана независимо от этого кода — openssl по
// схеме HMAC-SHA256(key, salt‖path), base64url без набивки.
//
// Пин нужен потому, что расхождение здесь не падает, а МОЛЧА ОТДАЁТ 403: у
// потребителя это выглядит как «картинки перестали грузиться», и искать будут
// где угодно, кроме подписи.
const (
	refKey       = "943b421c9eb07c830af81030552c86009268de4e532ba2ee2eab8247c6da0881"
	refSalt      = "520f986b998545b4785e0defbc4f3c1203f22de2374a3d53cb7a7fe9fea309c5"
	refPath      = "/rs:fit:300:300/plain/http://img.example.com/pretty/image.jpg"
	refSignature = "m3k5QADfcKPDj-SDI2AIogZbC3FlAXszuwhtWXYqavc"
)

func TestSign_MatchesImgproxyReference(t *testing.T) {
	t.Parallel()
	s := New(Config{
		BaseURL: "https://img.example.com", SourceBase: "s3://catalog",
		Key: refKey, Salt: refSalt,
	})

	assert.Equal(t, refSignature, s.sign(refPath))
}

// Соль — часть подписи, а не украшение: без неё чужой imgproxy с тем же ключом
// принимал бы наши ссылки.
func TestSign_DependsOnKeyAndSalt(t *testing.T) {
	t.Parallel()
	base := New(Config{BaseURL: "https://i", SourceBase: "s3://b", Key: refKey, Salt: refSalt})
	otherKey := New(Config{BaseURL: "https://i", SourceBase: "s3://b", Key: refSalt, Salt: refSalt})
	otherSalt := New(Config{BaseURL: "https://i", SourceBase: "s3://b", Key: refKey, Salt: refKey})

	first, again := base.sign(refPath), base.sign(refPath)
	assert.NotEqual(t, first, otherKey.sign(refPath))
	assert.NotEqual(t, first, otherSalt.sign(refPath))
	assert.Equal(t, first, again, "подпись детерминирована")
}

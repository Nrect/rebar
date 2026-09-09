package s3

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"

	"github.com/nrect/rebar/objectstore"
)

// maxErrorBody — сколько байт ответа разбирать: тело ошибки у S3 короткое, а
// читать неограниченно у чужой стороны нельзя.
const maxErrorBody = 8 << 10

// errorResult — из тела ошибки берётся ТОЛЬКО Code.
//
// Message и Key провайдер заполняет содержательно («The specified key does not
// exist», Key целиком), и в лог это уносило бы ключ объекта — то есть
// персональные данные потребителя (CORRECTNESS §10). Code — закрытый словарь
// провайдера, он безопасен.
type errorResult struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
}

// statusError — ошибка по ответу провайдера: класс, HTTP-код и Code.
// Ни ключа, ни URL, ни подписи в тексте нет.
func statusError(op string, resp *http.Response) error {
	code := errorCode(resp)
	kind := objectstore.ErrUnavailable
	if resp.StatusCode == http.StatusNotFound {
		kind = objectstore.ErrNotFound
	}
	if code == "" {
		return fmt.Errorf("%w: %s: http %d", kind, op, resp.StatusCode)
	}
	return fmt.Errorf("%w: %s: http %d (%s)", kind, op, resp.StatusCode, code)
}

func errorCode(resp *http.Response) string {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if err != nil {
		return ""
	}
	var parsed errorResult
	if xml.Unmarshal(body, &parsed) != nil {
		return ""
	}
	return parsed.Code
}

// transportError — сбой до ответа. Ошибка http.Client — это *url.Error, и в
// её тексте лежит адрес запроса вместе с ключом объекта; наружу уходит класс.
func transportError(op string) error {
	return fmt.Errorf("%w: %s: no response", objectstore.ErrUnavailable, op)
}

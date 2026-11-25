package signing

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// SigningInput собирает канонический вход для подписи HTTP-запроса федерации.
//
// Спека Server-Server API: подписывается канонический JSON объекта
// {"method","uri","origin","destination","content"} (content — тело запроса,
// для пустого тела {}), а не склейка строк. Разделители-переводы строк
// Synapse не принимает (401 BadSignature) — проверено на интероп-стенде.
func SigningInput(origin, destination, method, uri string, body []byte) ([]byte, error) {
	// Synapse кладёт "content" в подписываемый объект только когда тело есть:
	// для GET без тела ключа content нет вовсе, и с "content":{} подпись не сходится
	// (проверено на make_join).
	object := map[string]any{
		"method":      strings.ToUpper(method),
		"uri":         uri,
		"origin":      origin,
		"destination": destination,
	}

	if len(bytes.TrimSpace(body)) > 0 {
		object["content"] = jsonAny(body)
	}

	// Тело запроса - не событие: числа канонизируются как у Synapse, без
	// ограничений спецификации для PDU.
	return canonicalJSONSynapse(object)
}

// MatrixAuthHeader собирает заголовок Authorization: X-Matrix.
func MatrixAuthHeader(key Key, origin, destination, method, uri string, body []byte) (string, error) {
	input, err := SigningInput(origin, destination, method, uri, body)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("X-Matrix origin=%q,destination=%q,key=%q,sig=%q",
		origin, destination, key.KeyID, key.Sign(input)), nil
}

// MatrixAuth — разобранный заголовок X-Matrix.
type MatrixAuth struct {
	Origin      string
	Destination string
	KeyID       string
	Signature   string
}

// ParseMatrixAuth разбирает значение заголовка Authorization.
func ParseMatrixAuth(header string) (MatrixAuth, error) {
	value := strings.TrimSpace(header)
	if len(value) < len("X-Matrix ") || !strings.EqualFold(value[:len("X-Matrix ")], "X-Matrix ") {
		return MatrixAuth{}, ErrBadMatrixAuth
	}

	var auth MatrixAuth

	for _, part := range strings.Split(value[len("X-Matrix "):], ",") {
		part = strings.TrimSpace(part)
		name, rest, found := strings.Cut(part, "=")
		if !found {
			return MatrixAuth{}, ErrBadMatrixAuth
		}

		field := strings.Trim(strings.TrimSpace(rest), `"`)
		switch strings.TrimSpace(name) {
		case "origin":
			auth.Origin = field
		case "destination":
			auth.Destination = field
		case "key":
			auth.KeyID = field
		case "sig":
			auth.Signature = field
		default:
			return MatrixAuth{}, ErrBadMatrixAuth
		}
	}

	if auth.Origin == "" || auth.Destination == "" || auth.KeyID == "" || auth.Signature == "" {
		return MatrixAuth{}, ErrBadMatrixAuth
	}

	return auth, nil
}

// VerifyRequest проверяет X-Matrix у входящего запроса: destination = наш server_name,
// ключ разрешается функцией keyLookup (кэш/запрос), подпись — по каноническому входу.
func VerifyRequest(
	r *http.Request,
	body []byte,
	ourServerName string,
	keyLookup func(origin, keyID string) (publicKey string, validUntil int64, err error),
) (MatrixAuth, error) {
	auth, err := ParseMatrixAuth(r.Header.Get("Authorization"))
	if err != nil {
		return MatrixAuth{}, err
	}

	if auth.Destination != ourServerName {
		return MatrixAuth{}, fmt.Errorf("%w: destination %q != %q", ErrBadMatrixAuth, auth.Destination, ourServerName)
	}

	publicKey, validUntil, err := keyLookup(auth.Origin, auth.KeyID)
	if err != nil {
		return MatrixAuth{}, err
	}

	if validUntil > 0 && time.Now().UnixMilli() > validUntil {
		return MatrixAuth{}, ErrKeyExpired
	}

	// Synapse подписывает request.uri — ровно тот URI, что пришёл на провод
	// (с %-эскейпингом и query), поэтому берём RequestURI().
	input, err := SigningInput(auth.Origin, auth.Destination, r.Method, r.URL.RequestURI(), body)
	if err != nil {
		return MatrixAuth{}, err
	}

	if err := VerifyEd25519(publicKey, string(input), auth.Signature); err == nil {
		return auth, nil
	}

	// Диагностика: какие варианты URI/тела могли быть подписаны Synapse.
	decodedURI := r.URL.Path
	escapedNoQuery := r.URL.EscapedPath()
	decodedNoQuery := r.URL.Path

	variants := map[string]string{
		"decoded+query":   decodedURI + querySuffix(r),
		"escaped-noquery": escapedNoQuery,
		"decoded-noquery": decodedNoQuery,
	}

	marker := "none"

	for name, uri := range variants {
		alt, altErr := SigningInput(auth.Origin, auth.Destination, r.Method, uri, body)
		if altErr == nil && VerifyEd25519(publicKey, string(alt), auth.Signature) == nil {
			marker = name

			break
		}
	}

	return MatrixAuth{}, fmt.Errorf("%w (variant=%s sig=%s input=%s)", ErrInvalidSig, marker,
		auth.Signature,
		base64.RawStdEncoding.EncodeToString(input))
}

func querySuffix(r *http.Request) string {
	if r.URL.RawQuery == "" {
		return ""
	}

	return "?" + r.URL.RawQuery
}

// ContentHash считает content hash события (hashes.sha256) по каноническому JSON
// без полей hashes/signatures/unsigned — как требует спека S2S. Synapse отвергает
// PDU без hashes («Malformed invite event»).
func ContentHash(object map[string]any) (string, error) {
	unsigned := make(map[string]any, len(object))
	for k, v := range object {
		switch k {
		case "hashes", "signatures", "unsigned":
			continue
		}

		unsigned[k] = v
	}

	canonical, err := CanonicalJSON(unsigned)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(canonical)

	return base64.RawStdEncoding.EncodeToString(sum[:]), nil
}

// ReferenceHash считает reference-hash события (room v4+): event_id обязан
// совпадать с тем, что вычислит получатель, иначе Synapse отвечает
// «Invite event ID must match».
//
// Берётся канонический JSON **с** hashes, но без signatures/unsigned/event_id (origin для v1-v10 остается);
// результат — "$" + base64url без паддинга от sha256.
func ReferenceHash(object map[string]any) (string, error) {
	unsigned := make(map[string]any, len(object))

	for k, v := range object {
		switch k {
		// origin не вырезается: для комнат v1-v10 он входит в redacted-событие и
		// reference hash, какие поля оставить - решает core.EventID по версии.
		case "signatures", "unsigned", "event_id":
			continue
		}

		unsigned[k] = v
	}

	canonical, err := CanonicalJSON(unsigned)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(canonical)

	return "$" + base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// SignJSON подписывает объект и добавляет подпись в поле signatures:
// signatures[server_name][key_id] = base64(sig) над каноническим JSON без signatures.
func SignJSON(key Key, serverName string, object map[string]any) (map[string]any, error) {
	// signedjson выкидывает из подписываемого объекта signatures и unsigned
	// (например, unsigned.age, который добавляет Synapse при пересылке).
	unsigned := make(map[string]any, len(object))
	for k, v := range object {
		if k == "signatures" || k == "unsigned" {
			continue
		}

		unsigned[k] = v
	}

	canonical, err := CanonicalJSON(unsigned)
	if err != nil {
		return nil, err
	}

	signature := key.Sign(canonical)
	object["signatures"] = map[string]any{
		serverName: map[string]string{key.KeyID: signature},
	}

	return object, nil
}

// PublicKeyBase64 — публичная часть произвольного ed25519-ключа (для тестов/утилит).
func PublicKeyBase64(key ed25519.PublicKey) string {
	return base64.RawStdEncoding.EncodeToString(key)
}

// ErrNoSignature — объект не подписан (для проверки чужих ответов).
var ErrNoSignature = errors.New("signing: no signatures")

// VerifyJSONWithoutEventID — как VerifyJSON, но не включает event_id в подписываемый
// объект: в room v3+ его нет в PDU, а мы восстанавливаем его сами.
func VerifyJSONWithoutEventID(object map[string]any, serverName string, keyLookup func(origin, keyID string) (string, int64, error)) error {
	clone := make(map[string]any, len(object))

	for k, v := range object {
		if k == "event_id" {
			continue
		}

		clone[k] = v
	}

	return VerifyJSON(clone, serverName, keyLookup)
}

// VerifyJSON проверяет подпись объекта, подписанного serverName ключом keyID.
//
// Подпись считается по каноническому JSON объекта без поля signatures.
// keyLookup возвращает публичный ключ и valid_until (0 — без срока). Срок ключа
// сверяется с origin_server_ts объекта (событие подписано тогда, спека: ключ
// должен быть действителен на момент события), без него - с текущим временем.
func VerifyJSON(object map[string]any, serverName string, keyLookup func(origin, keyID string) (string, int64, error)) error {
	rawSignatures, ok := object["signatures"]
	if !ok {
		return ErrNoSignature
	}

	signatures, ok := rawSignatures.(map[string]any)
	if !ok {
		return ErrNoSignature
	}

	serverSigs, ok := signatures[serverName].(map[string]any)
	if !ok || len(serverSigs) == 0 {
		return fmt.Errorf("%w: no signature from %s", ErrNoSignature, serverName)
	}

	// signedjson выкидывает из подписываемого объекта signatures и unsigned
	// (например, unsigned.age, который добавляет Synapse при пересылке).
	unsigned := make(map[string]any, len(object))
	for k, v := range object {
		if k == "signatures" || k == "unsigned" {
			continue
		}

		unsigned[k] = v
	}

	canonical, err := CanonicalJSON(unsigned)
	if err != nil {
		return err
	}

	at := signedAt(object)

	var lastErr error

	for keyID, sigValue := range serverSigs {
		signature, ok := sigValue.(string)
		if !ok {
			continue
		}

		publicKey, validUntil, err := keyLookup(serverName, keyID)
		if err != nil {
			lastErr = err

			continue
		}

		if validUntil > 0 && at > validUntil {
			lastErr = ErrKeyExpired

			continue
		}

		err = VerifyEd25519(publicKey, string(canonical), signature)
		if err == nil {
			return nil
		}

		lastErr = err
	}

	if lastErr != nil {
		return lastErr
	}

	return ErrInvalidSig
}

// signedAt - момент, на который проверяется срок ключа: origin_server_ts
// события или текущее время.
func signedAt(object map[string]any) int64 {
	switch ts := object["origin_server_ts"].(type) {
	case float64:
		if ts > 0 {
			return int64(ts)
		}
	case int64:
		if ts > 0 {
			return ts
		}
	case json.Number:
		if parsed, err := ts.Int64(); err == nil && parsed > 0 {
			return parsed
		}
	}

	return time.Now().UnixMilli()
}

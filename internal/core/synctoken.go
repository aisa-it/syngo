package core

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ErrUnknownPos — токен /sync не разбирается, версия не та или подпись не сходится.
// Транспорт отображает его в 400 M_UNKNOWN_POS (как в Synapse).
var ErrUnknownPos = errors.New("core: unknown sync position")

// synapseSyncToken - формат since-токена Synapse (StreamToken.to_string, Synapse
// 1.165): ключи потоков через "_". Первый - RoomStreamToken: "s<поток>" или
// "m<поток>~<writer>.<pos>..." при нескольких writer событий. Остальные - число
// или MultiWriterStreamToken "m<поток>~<writer>.<pos>..." (receipts, account
// data, to-device, device lists и др. при нескольких writer).
// Топологический "t<topo>-<поток>" - токен пагинации, не since /sync.
var synapseSyncToken = regexp.MustCompile(`^[sm]\d+(?:~\d+\.\d+)*(?:_(?:\d+|m\d+(?:~\d+\.\d+)*))*$`)

// IsSynapseSyncToken сообщает, что since выдан Synapse до переезда:
// клиент, переживший переключение со своим токеном доступа, присылает и старый
// since. Позицию Synapse перевести нельзя - такой запрос обслуживается как initial
// sync без событий timeline (limited, prev_batch на текущую позицию):
// иначе клиент бесконечно получал бы M_UNKNOWN_POS, а последние сообщения комнат
// боты принимали бы за новые.
func IsSynapseSyncToken(token string) bool {
	return len(token) <= maxTokenLength && synapseSyncToken.MatchString(token)
}

// syncTokenVersion — версия формата токена. Меняется только при несовместимом
// изменении семантики позиции (старые токены после этого невалидны — это осознанно).
const syncTokenVersion = "v1"

// maxTokenLength ограничивает входной токен, чтобы не разбирать мусор произвольной длины.
const maxTokenLength = 256

// SyncTokenCodec кодирует и проверяет токены /sync (ADR-0005 §4).
//
// Формат: v1.e<stream_ordering>[.t<to-device>][.r<эфемерные>].<mac>, где mac - base64url(HMAC-SHA256(secret, "v1.e<n>"))[:16].
// Позиция — глобальный events.stream_ordering, одно число покрывает и обычные события,
// и state: любое новое событие увеличивает сквозной порядок.
type SyncTokenCodec struct {
	serverName string
	secret     []byte
}

// NewSyncTokenCodec создаёт кодек токенов. Пустой секрет допустим только для разработки:
// токены будут недействительны после перезапуска процесса.
func NewSyncTokenCodec(serverName string, secret []byte) *SyncTokenCodec {
	return &SyncTokenCodec{serverName: serverName, secret: secret}
}

// Encode собирает токен для позиции stream_ordering (без to-device закладки).
func (c *SyncTokenCodec) Encode(position int64) string {
	return c.Encode2(position, 0)
}

// Encode2 собирает токен с двумя компонентами: позиция событий и отдельная
// закладка to-device очереди.
//
// Одно число на всё не работает: при очереди to-device больше пачки (100) токен
// приходилось опускать ниже глобальной позиции, и клиент повторно получал уже
// виденный таймлайн. Отдельная закладка убирает эту связь.
func (c *SyncTokenCodec) Encode2(position, toDevice int64) string {
	return c.Encode3(position, toDevice, -1)
}

// Encode3 - токен с версией эфемерного потока (typing, receipts): по ней
// incremental /sync отдает только комнаты, где что-то изменилось.
// ephemeral < 0 - версия неизвестна, компонент не пишется.
func (c *SyncTokenCodec) Encode3(position, toDevice, ephemeral int64) string {
	payload := fmt.Sprintf("%s.e%d", syncTokenVersion, position)
	if toDevice > 0 {
		payload += fmt.Sprintf(".t%d", toDevice)
	}

	if ephemeral >= 0 {
		payload += fmt.Sprintf(".r%d", ephemeral)
	}

	return payload + "." + c.mac(payload)
}

// Decode проверяет подпись и возвращает позицию событий.
func (c *SyncTokenCodec) Decode(token string) (int64, error) {
	position, _, err := c.Decode2(token)

	return position, err
}

// Decode2 проверяет подпись и возвращает позицию событий и закладку to-device.
// Токены без `t`-компонента (v1, выданные раньше) читаются как to-device = 0:
// клиент получит очередь с начала, дублей не будет — недоставленное ещё не удалено.
func (c *SyncTokenCodec) Decode2(token string) (position, toDevice int64, err error) {
	position, toDevice, _, err = c.Decode3(token)

	return position, toDevice, err
}

// Decode3 - как Decode2, плюс версия эфемерного потока. Токены без `r`-компонента
// (старого формата) читаются с ephemeral = -1: версия неизвестна, и /sync
// отдает все комнаты, как раньше.
func (c *SyncTokenCodec) Decode3(token string) (position, toDevice, ephemeral int64, err error) {
	if token == "" || len(token) > maxTokenLength {
		return 0, 0, 0, ErrUnknownPos
	}

	// Подпись отделена последней точкой: в payload тоже есть точка (v1.e<n>).
	lastDot := strings.LastIndex(token, ".")
	if lastDot <= 0 || lastDot == len(token)-1 {
		return 0, 0, 0, ErrUnknownPos
	}

	payload, mac := token[:lastDot], token[lastDot+1:]

	// Сравнение подписи — за постоянное время, как положено для MAC.
	expected := c.mac(payload)
	if !hmac.Equal([]byte(mac), []byte(expected)) {
		return 0, 0, 0, ErrUnknownPos
	}

	version, rest, found := strings.Cut(payload, ".e")
	if !found || version != syncTokenVersion {
		return 0, 0, 0, ErrUnknownPos
	}

	rest, ephemeralPart, hasEphemeral := strings.Cut(rest, ".r")
	eventsPart, toDevicePart, hasToDevice := strings.Cut(rest, ".t")

	parsed, parseErr := strconv.ParseInt(eventsPart, 10, 64)
	if parseErr != nil || parsed < 0 {
		return 0, 0, 0, ErrUnknownPos
	}

	var toDevicePos int64

	if hasToDevice {
		toDevicePos, parseErr = strconv.ParseInt(toDevicePart, 10, 64)
		if parseErr != nil || toDevicePos < 0 {
			return 0, 0, 0, ErrUnknownPos
		}
	}

	ephemeralPos := int64(-1)

	if hasEphemeral {
		ephemeralPos, parseErr = strconv.ParseInt(ephemeralPart, 10, 64)
		if parseErr != nil || ephemeralPos < 0 {
			return 0, 0, 0, ErrUnknownPos
		}
	}

	return parsed, toDevicePos, ephemeralPos, nil
}

// mac считает подпись для payload с привязкой к имени сервера (токены одного сервера
// невалидны на другом, даже если секрет совпал).
func (c *SyncTokenCodec) mac(payload string) string {
	mac := hmac.New(sha256.New, c.secret)
	mac.Write([]byte(c.serverName))
	mac.Write([]byte{0})
	mac.Write([]byte(payload))

	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:16])
}

// SlidingPos - позиция pos simplified sliding sync: позиция
// сохраненного состояния соединения, потока событий и эфемерных событий.
type SlidingPos struct {
	// Conn - позиция состояния соединения в БД; 0 - состояние не сохранялось.
	Conn int64
	// Stream - позиция потока событий, to-device, account data и ключей.
	Stream int64
	// Ephemeral - версия typing и receipts.
	Ephemeral int64
}

// slidingTokenVersion - версия формата pos; отличается от since обычного /sync,
// чтобы токены одного протокола не принимались другим.
const slidingTokenVersion = "s1"

// EncodeSliding собирает pos: s1.c<conn>.e<stream>.r<ephemeral>.<mac>.
func (c *SyncTokenCodec) EncodeSliding(pos SlidingPos) string {
	payload := fmt.Sprintf("%s.c%d.e%d.r%d", slidingTokenVersion, max(pos.Conn, 0), max(pos.Stream, 0), max(pos.Ephemeral, 0))

	return payload + "." + c.mac(payload)
}

// DecodeSliding проверяет подпись pos. Чужой, поврежденный или выданный Synapse
// токен - ErrUnknownPos: клиент начинает соединение заново.
func (c *SyncTokenCodec) DecodeSliding(token string) (SlidingPos, error) {
	if token == "" || len(token) > maxTokenLength {
		return SlidingPos{}, ErrUnknownPos
	}

	parts := strings.Split(token, ".")
	if len(parts) != 5 || parts[0] != slidingTokenVersion {
		return SlidingPos{}, ErrUnknownPos
	}

	payload := strings.Join(parts[:4], ".")
	if !hmac.Equal([]byte(parts[4]), []byte(c.mac(payload))) {
		return SlidingPos{}, ErrUnknownPos
	}

	var values [3]int64

	for i, prefix := range []string{"c", "e", "r"} {
		raw, ok := strings.CutPrefix(parts[i+1], prefix)
		if !ok {
			return SlidingPos{}, ErrUnknownPos
		}

		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 0 {
			return SlidingPos{}, ErrUnknownPos
		}

		values[i] = value
	}

	return SlidingPos{Conn: values[0], Stream: values[1], Ephemeral: values[2]}, nil
}

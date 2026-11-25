package core

import (
	"context"
	"regexp"
	"strconv"
	"strings"
)

// Токены пагинации комнаты (from в /messages, /relations, /threads).
//
// Наш формат - "s<stream_ordering>" (эксклюзивный курсор). После переезда с
// Synapse клиенты присылают и закэшированные токены Synapse:
//   - RoomStreamToken topological "t<depth>-<stream>": depth - topological_ordering
//     события (импорт сохраняет depth), stream - stream_ordering Synapse;
//   - live "s<stream>" и "m<stream>~<writer>.<pos>~...";
//   - StreamToken целиком: ключи потоков через "_", первый - позиция комнаты.
//
// Голый "s<n>" от Synapse неотличим от нашего и читается как наш курсор: импорт
// сохраняет stream_ordering Synapse, так что позиция та же (с точностью до
// включительности границы).
type pageTokenKind int

const (
	// pageTokenStream - наш курсор "s<stream_ordering>".
	pageTokenStream pageTokenKind = iota
	// pageTokenTopological - topological-токен Synapse "t<depth>-<stream>".
	pageTokenTopological
	// pageTokenLive - live-токен Synapse (StreamToken "s<n>_..." или "m<n>~...").
	pageTokenLive
)

// pageToken - разобранный токен пагинации.
type pageToken struct {
	kind   pageTokenKind
	depth  int64
	stream int64
}

// maxPageTokenLength - длиннее токенов не бывает ни у нас, ни у Synapse.
const maxPageTokenLength = 512

// synapseStreamKeys - хвост StreamToken Synapse после ключа комнаты: целые и
// multi-writer позиции "m<n>~<id>.<pos>".
var synapseStreamKeys = regexp.MustCompile(`^[0-9m~._]*$`)

// synapseWriters - хвост multi-writer токена после "m<n>": "~<id>.<pos>" повторами.
var synapseWriters = regexp.MustCompile(`^(~(\d+\.\d+)?)*$`)

// errPageToken - неверный токен пагинации: 400 M_INVALID_PARAM, как у Synapse.
func errPageToken() error {
	return requestError(ErrInvalidParam, "Invalid pagination token")
}

// parsePageToken разбирает from пагинации комнаты.
func parsePageToken(token string) (pageToken, error) {
	if token == "" || len(token) > maxPageTokenLength {
		return pageToken{}, errPageToken()
	}

	room, rest, full := strings.Cut(token, "_")
	if full && !synapseStreamKeys.MatchString(rest) {
		return pageToken{}, errPageToken()
	}

	if room == "" {
		return pageToken{}, errPageToken()
	}

	switch room[0] {
	case 's':
		stream, err := strconv.ParseInt(room[1:], 10, 64)
		if err != nil {
			return pageToken{}, errPageToken()
		}

		if full {
			return pageToken{kind: pageTokenLive, stream: stream}, nil
		}

		return pageToken{kind: pageTokenStream, stream: stream}, nil
	case 't':
		rawDepth, rawStream, ok := strings.Cut(room[1:], "-")
		if !ok {
			return pageToken{}, errPageToken()
		}

		depth, err := strconv.ParseInt(rawDepth, 10, 64)
		if err != nil {
			return pageToken{}, errPageToken()
		}

		stream, err := strconv.ParseInt(rawStream, 10, 64)
		if err != nil {
			return pageToken{}, errPageToken()
		}

		return pageToken{kind: pageTokenTopological, depth: depth, stream: stream}, nil
	case 'm':
		rawStream, writers := room[1:], ""
		if i := strings.IndexByte(rawStream, '~'); i >= 0 {
			rawStream, writers = rawStream[:i], rawStream[i:]
		}

		stream, err := strconv.ParseInt(rawStream, 10, 64)
		if err != nil || !synapseWriters.MatchString(writers) {
			return pageToken{}, errPageToken()
		}

		return pageToken{kind: pageTokenLive, stream: stream}, nil
	default:
		return pageToken{}, errPageToken()
	}
}

// IsSynapsePageToken - токен выдан Synapse до переезда (topological или live),
// а не нашим сервером. Голый "s<n>" считается нашим.
func IsSynapsePageToken(token string) bool {
	parsed, err := parsePageToken(token)

	return err == nil && parsed.kind != pageTokenStream
}

// PageCursor переводит from пагинации комнаты в наш эксклюзивный курсор
// stream_ordering: при backward берутся события с позицией меньше курсора,
// иначе - больше. Неверный токен - ErrInvalidParam.
//
// Правило для topological-токена Synapse "t<D>-<S>" - граница Synapse: при
// dir=b события с (depth, stream) <= (D, S) (событие токена входит, end Synapse
// для b уже указывает на stream-1), при dir=f - с (depth, stream) > (D, S). При
// равной depth порядок решает stream: импорт сохраняет stream_ordering Synapse.
// Дальше пагинация идет по нашему stream_ordering, поэтому курсор - крайняя
// позиция среди событий по нужную сторону границы: max(stream_ordering) + 1
// для b, min(stream_ordering) - 1 для f. Где порядок по depth и по stream
// совпадает (обычная история), страницы совпадают с Synapse без повторов и
// пропусков; где расходится (позднее пришедшие события федерации), возможны
// повторы уже виденного (клиент их отбрасывает по event_id), но не пропуски.
//
// Live-токен Synapse - позиция «сейчас» на старом сервере: листаем от текущего
// конца комнаты (b - вся история с самого нового события, f - пусто).
func (s *RoomService) PageCursor(ctx context.Context, roomID, token string, backward bool) (int64, error) {
	parsed, err := parsePageToken(token)
	if err != nil {
		return 0, err
	}

	if parsed.kind == pageTokenStream {
		return parsed.stream, nil
	}

	minStream, maxStream, err := s.store.MinMaxStream(ctx, roomID)
	if err != nil {
		return 0, err
	}

	if parsed.kind == pageTokenLive {
		if backward {
			return maxStream + 1, nil
		}

		return maxStream, nil
	}

	pos, found, err := s.store.StreamAtDepth(ctx, roomID, parsed.depth, parsed.stream, backward)
	if err != nil {
		return 0, err
	}

	switch {
	case !found && backward:
		// До границы событий нет: страница пустая.
		return minStream, nil
	case !found:
		return maxStream, nil
	case backward:
		return pos + 1, nil
	default:
		return pos - 1, nil
	}
}

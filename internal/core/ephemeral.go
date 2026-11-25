package core

import (
	"sync"
	"sync/atomic"
	"time"
)

// Эфемерные события: presence, typing, receipts.
//
// По модели Matrix они не попадают в DAG и не хранятся в БД как события: сервер
// держит их в памяти и отдаёт в /sync через rooms.join.*.ephemeral. Поэтому число
// запросов к БД на /sync не меняется — это важное условие SLO (ADR-0005).

// Константы поведения.
const (
	// TypingTimeout — через сколько «печатает» перестаёт быть правдой (как в Synapse).
	TypingTimeout = 30 * time.Second
	// PresenceTimeout — через сколько offline, если статус не обновляли.
	PresenceTimeout = 5 * time.Minute
)

// EphemeralStore — хранилище эфемерных состояний в памяти процесса.
// Межинстансовое распространение появится вместе с репликацией (Redis/LISTEN/NOTIFY).
type EphemeralStore struct {
	mu       sync.Mutex
	presence map[string]presenceEntry           // userID -> состояние
	typing   map[string]map[string]time.Time    // roomID -> userID -> когда обновлено
	receipts map[string]map[string]receiptEntry // roomID -> receiptKey -> receipt
	// typingChanged — когда в комнате последний раз менялся список печатающих:
	// после остановки набора клиенту нужно отдать пустой m.typing, иначе индикатор
	// у собеседников не гаснет. Нужен только токенам без версии
	// эфемерных событий (старого формата).
	typingChanged map[string]time.Time
	// typingVersion, receiptVersion и privateVersion - версия последнего изменения
	// typing, публичных receipts комнаты и m.read.private пользователя в комнате
	// (ключ комната + пользователь): /sync отдает комнату, только если что-то из
	// этого новее версии в токене клиента.
	typingVersion  map[string]int64
	receiptVersion map[string]int64
	privateVersion map[string]int64
	now            func() time.Time
	// version растет при каждом изменении typing и receipts - это поток эфемерных
	// событий /sync (как typing и receipts stream в Synapse). Начинается с времени
	// старта процесса в микросекундах: версия из токена, выданного до рестарта,
	// меньше любой версии нового процесса, и изменения не теряются.
	version atomic.Int64
	// startVersion - версия на старте: ее получают receipts, загруженные из БД.
	startVersion int64
	// activity - версия последнего изменения typing и receipts (без presence).
	activity atomic.Int64
}

// Version возвращает текущую версию эфемерного состояния.
func (s *EphemeralStore) Version() int64 {
	return s.version.Load()
}

// ActivityVersion - версия последнего изменения typing или receipts (без
// presence): presence касается только участников общих комнат, и его изменение
// не должно досрочно отвечать на long-poll всех остальных.
func (s *EphemeralStore) ActivityVersion() int64 {
	return s.activity.Load()
}

// bumpActivity поднимает общую версию и версию typing/receipts.
func (s *EphemeralStore) bumpActivity() int64 {
	version := s.version.Add(1)

	for {
		current := s.activity.Load()
		if current >= version || s.activity.CompareAndSwap(current, version) {
			return version
		}
	}
}

// Now - текущее время хранилища (подменяется в тестах).
func (s *EphemeralStore) Now() time.Time {
	return s.now()
}

// Типы receipts (спецификация, «Receipts»). m.fully_read - не receipt, а room
// account data; через /receipt он тоже принимается.
const (
	ReceiptRead        = "m.read"
	ReceiptReadPrivate = "m.read.private"
	ReceiptFullyRead   = "m.fully_read"
)

// receiptKey — receipt хранится на (пользователь, тип, тред): приватный не
// затирает публичный, отметка в треде - отметку основной ленты.
func receiptKey(userID, receiptType, threadID string) string {
	return userID + "\x00" + receiptType + "\x00" + threadID
}

type presenceEntry struct {
	Presence string
	Status   string
	Updated  time.Time
	// Version - версия эфемерного потока, с которой действует этот статус: /sync
	// отдает presence, изменившийся после версии из токена.
	Version int64
}

type receiptEntry struct {
	UserID      string
	EventID     string
	ReceiptType string
	ThreadID    string
	Updated     time.Time
	// Version - версия эфемерного потока, с которой receipt действует.
	Version int64
}

// PresenceEvent — событие presence для /sync.
type PresenceEvent struct {
	UserID    string
	Presence  string
	StatusMsg string
	// LastActiveMS - время последней активности (unix ms), 0 - неизвестно.
	LastActiveMS int64
	// Version - версия эфемерного потока изменения статуса.
	Version int64
}

// TypingEvent — событие m.typing для комнаты.
type TypingEvent struct {
	RoomID  string
	UserIDs []string
}

// ReceiptEvent — событие m.receipt для комнаты.
type ReceiptEvent struct {
	RoomID   string
	EventID  string
	UserID   string
	Type     string
	ThreadID string
	TS       int64
}

// NewEphemeralStore создаёт хранилище эфемерных событий.
func NewEphemeralStore() *EphemeralStore {
	s := &EphemeralStore{
		presence:       map[string]presenceEntry{},
		typing:         map[string]map[string]time.Time{},
		receipts:       map[string]map[string]receiptEntry{},
		typingChanged:  map[string]time.Time{},
		typingVersion:  map[string]int64{},
		receiptVersion: map[string]int64{},
		privateVersion: map[string]int64{},
		now:            time.Now,
		startVersion:   time.Now().UnixMicro(),
	}
	s.version.Store(s.startVersion)
	s.activity.Store(s.startVersion)

	return s
}

// WithClock подменяет время (тесты).
func (s *EphemeralStore) WithClock(now func() time.Time) *EphemeralStore {
	s.now = now

	return s
}

// Значения presence (спека, «Presence»).
const (
	PresenceOnline      = "online"
	PresenceOffline     = "offline"
	PresenceUnavailable = "unavailable"
)

// ValidPresence - допустимое значение presence.
func ValidPresence(presence string) bool {
	switch presence {
	case PresenceOnline, PresenceOffline, PresenceUnavailable:
		return true
	}

	return false
}

// SetPresence обновляет статус присутствия пользователя (PUT /presence). Версия
// растет, только если статус или status_msg изменились (или прежний статус
// истек): /sync других участников будится лишь на реальные изменения. Возвращает,
// изменилось ли что-то.
func (s *EphemeralStore) SetPresence(userID, presence, status string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if presence == "" {
		presence = PresenceOnline
	}

	return s.setPresenceLocked(userID, presence, status)
}

// SyncPresence - статус от /sync (set_presence, по умолчанию online, как в
// Synapse). status_msg сохраняется. Синк без явного set_presence не снимает
// явно выставленный unavailable; set_presence=offline статус не трогает.
// Возвращает, изменилось ли что-то.
func (s *EphemeralStore) SyncPresence(userID, setPresence string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	current, ok := s.presence[userID]
	active := ok && !s.expiredLocked(current)

	switch setPresence {
	case PresenceOffline:
		return false
	case "":
		if active && current.Presence == PresenceUnavailable {
			current.Updated = s.now()
			s.presence[userID] = current

			return false
		}

		setPresence = PresenceOnline
	}

	status := ""
	if ok {
		status = current.Status
	}

	return s.setPresenceLocked(userID, setPresence, status)
}

func (s *EphemeralStore) setPresenceLocked(userID, presence, status string) bool {
	current, ok := s.presence[userID]
	if ok && !s.expiredLocked(current) && current.Presence == presence && current.Status == status {
		current.Updated = s.now()
		s.presence[userID] = current

		return false
	}

	s.presence[userID] = presenceEntry{
		Presence: presence, Status: status, Updated: s.now(), Version: s.version.Add(1),
	}

	return true
}

// expiredLocked - статус online/unavailable без обновления дольше PresenceTimeout
// считается offline.
func (s *EphemeralStore) expiredLocked(entry presenceEntry) bool {
	return entry.Presence != PresenceOffline && s.now().Sub(entry.Updated) > PresenceTimeout
}

// Presence возвращает актуальный статус (истёкший считается offline).
func (s *EphemeralStore) Presence(userID string) PresenceEvent {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.presence[userID]
	if !ok {
		return PresenceEvent{UserID: userID, Presence: PresenceOffline}
	}

	return s.presenceEventLocked(userID, entry)
}

func (s *EphemeralStore) presenceEventLocked(userID string, entry presenceEntry) PresenceEvent {
	if s.expiredLocked(entry) {
		return PresenceEvent{UserID: userID, Presence: PresenceOffline, StatusMsg: entry.Status, Version: entry.Version}
	}

	return PresenceEvent{
		UserID:       userID,
		Presence:     entry.Presence,
		StatusMsg:    entry.Status,
		LastActiveMS: entry.Updated.UnixMilli(),
		Version:      entry.Version,
	}
}

// PresenceChangedSince - статусы, изменившиеся после версии since; since < 0 -
// все известные статусы (initial sync).
func (s *EphemeralStore) PresenceChangedSince(since int64) []PresenceEvent {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]PresenceEvent, 0)

	for userID, entry := range s.presence {
		if since < 0 || entry.Version > since {
			out = append(out, s.presenceEventLocked(userID, entry))
		}
	}

	return out
}

// SetTyping фиксирует «печатает»; typing=false убирает пользователя.
func (s *EphemeralStore) SetTyping(roomID, userID string, typing bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.typingChanged[roomID] = s.now()
	s.typingVersion[roomID] = s.bumpActivity()

	if !typing {
		delete(s.typing[roomID], userID)

		return
	}

	if s.typing[roomID] == nil {
		s.typing[roomID] = map[string]time.Time{}
	}

	s.typing[roomID][userID] = s.now()
}

// TypingChanged сообщает, нужно ли отдать m.typing комнаты: список не пуст или
// менялся недавно (остановка набора явная или по таймауту). Пустой m.typing
// гасит индикатор у клиентов.
func (s *EphemeralStore) TypingChanged(roomID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	changed, ok := s.typingChanged[roomID]
	if !ok {
		return false
	}

	// Окно: таймаут набора (истечение без явного typing=false) плюс запас на
	// клиентов, которые пропустили один /sync.
	if s.now().Sub(changed) > 2*TypingTimeout {
		delete(s.typingChanged, roomID)

		return false
	}

	return true
}

// Typing возвращает список печатающих в комнате (без истёкших).
func (s *EphemeralStore) Typing(roomID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	users := make([]string, 0, len(s.typing[roomID]))

	for userID, updated := range s.typing[roomID] {
		if now.Sub(updated) > TypingTimeout {
			delete(s.typing[roomID], userID)
			// Истечение набора - тоже изменение: клиентам нужен пустой m.typing.
			s.typingChanged[roomID] = now
			s.typingVersion[roomID] = s.bumpActivity()

			continue
		}

		users = append(users, userID)
	}

	return users
}

// SetReceipt сохраняет последний receipt пользователя в комнате для типа и треда
// (threadID пустой - без треда, "main" - основная лента) и возвращает его для
// записи в БД.
func (s *EphemeralStore) SetReceipt(roomID, userID, receiptType, threadID, eventID string) ReceiptEvent {
	receipt := ReceiptEvent{
		RoomID: roomID, EventID: eventID, UserID: userID, Type: receiptType, ThreadID: threadID,
		TS: s.now().UnixMilli(),
	}

	s.StoreReceipt(receipt)

	return receipt
}

// StoreReceipt сохраняет готовый receipt и поднимает версию потока. Обработчик
// /receipt вызывает его после записи в БД и сброса счетчиков: иначе /sync мог
// увидеть новую версию раньше нового счетчика и больше не отдать комнату.
func (s *EphemeralStore) StoreReceipt(r ReceiptEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.receipts[r.RoomID] == nil {
		s.receipts[r.RoomID] = map[string]receiptEntry{}
	}

	version := s.bumpActivity()
	s.receipts[r.RoomID][receiptKey(r.UserID, r.Type, r.ThreadID)] = receiptEntry{
		UserID: r.UserID, EventID: r.EventID, ReceiptType: r.Type, ThreadID: r.ThreadID,
		Updated: time.UnixMilli(r.TS), Version: version,
	}

	s.bumpReceipt(r.RoomID, r.UserID, r.Type, version)
}

// bumpReceipt отмечает изменение receipts комнаты: приватный receipt виден
// только владельцу, и комнату он меняет только для него. Вызывается под mu.
func (s *EphemeralStore) bumpReceipt(roomID, userID, receiptType string, version int64) {
	if receiptType == ReceiptReadPrivate {
		s.privateVersion[roomID+"\x00"+userID] = version

		return
	}

	s.receiptVersion[roomID] = version
}

// LoadReceipts заполняет память сохраненными отметками (старт процесса).
func (s *EphemeralStore) LoadReceipts(receipts []ReceiptEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, r := range receipts {
		if s.receipts[r.RoomID] == nil {
			s.receipts[r.RoomID] = map[string]receiptEntry{}
		}

		s.receipts[r.RoomID][receiptKey(r.UserID, r.Type, r.ThreadID)] = receiptEntry{
			UserID: r.UserID, EventID: r.EventID, ReceiptType: r.Type, ThreadID: r.ThreadID,
			Updated: time.UnixMilli(r.TS), Version: s.startVersion,
		}

		// Версия старта: токен, выданный до рестарта, старше нее, и такой клиент
		// получит отметки заново - вдруг последнюю он не успел получить.
		s.bumpReceipt(r.RoomID, r.UserID, r.Type, s.startVersion)
	}
}

// Receipts возвращает последние receipts комнаты, которые видит viewer: m.read.private -
// только свои (раньше приватная отметка рассылалась всей комнате). Окна по времени нет:
// отметки сохраняются в БД и, как в Synapse, отдаются независимо от возраста.
func (s *EphemeralStore) Receipts(roomID, viewer string) []ReceiptEvent {
	return s.ReceiptsSince(roomID, viewer, -1)
}

// ReceiptsSince - receipts комнаты, изменившиеся после версии since (как
// Receipts; since < 0 - все). Incremental /sync отдает только новые отметки,
// как Synapse.
func (s *EphemeralStore) ReceiptsSince(roomID, viewer string, since int64) []ReceiptEvent {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]ReceiptEvent, 0, len(s.receipts[roomID]))

	for _, entry := range s.receipts[roomID] {
		if entry.ReceiptType == ReceiptReadPrivate && entry.UserID != viewer {
			continue
		}

		if since >= 0 && entry.Version <= since {
			continue
		}

		out = append(out, ReceiptEvent{
			RoomID: roomID, EventID: entry.EventID, UserID: entry.UserID, Type: entry.ReceiptType,
			ThreadID: entry.ThreadID, TS: entry.Updated.UnixMilli(),
		})
	}

	return out
}

// UserReceipts возвращает отметки прочтения (m.read и m.read.private) самого
// пользователя в комнате без ограничения по давности: по ним считаются
// unread_notifications.
func (s *EphemeralStore) UserReceipts(roomID, userID string) []ReceiptEvent {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []ReceiptEvent

	for _, entry := range s.receipts[roomID] {
		if entry.UserID != userID || (entry.ReceiptType != ReceiptRead && entry.ReceiptType != ReceiptReadPrivate) {
			continue
		}

		out = append(out, ReceiptEvent{
			RoomID: roomID, EventID: entry.EventID, UserID: entry.UserID, Type: entry.ReceiptType,
			ThreadID: entry.ThreadID, TS: entry.Updated.UnixMilli(),
		})
	}

	return out
}

// TypingSince - печатающие в комнате и признак, что список изменился после
// версии since (тогда m.typing отдается и пустым - гаснет индикатор).
func (s *EphemeralStore) TypingSince(roomID string, since int64) (users []string, changed bool) {
	users = s.Typing(roomID)

	s.mu.Lock()
	defer s.mu.Unlock()

	return users, s.typingVersion[roomID] > since
}

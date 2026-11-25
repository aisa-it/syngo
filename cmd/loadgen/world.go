package main

import (
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"
)

// agent - пользователь пула: одно устройство, свой /sync и свои действия.
type agent struct {
	idx       int
	localpart string
	userID    string
	token     string
	deviceID  string
	filterID  string
	// curveKey - фиктивный curve25519 устройства (sender_key m.room.encrypted).
	curveKey string

	// syncing - initial sync выполнен: пользователь получает события и входит в
	// ожидаемых получателей доставки.
	syncing atomic.Bool
	txn     atomic.Int64
	otk     atomic.Int64
	// pausedUntil - unix-нс, до которых действия ждут после 429 (retry_after_ms).
	pausedUntil atomic.Int64

	mu  sync.Mutex
	rng *rand.Rand
	// rooms - комнаты, где пользователь состоит: последнее событие (для receipt) и
	// prev_batch (для /messages назад).
	rooms map[string]*agentRoom
	// invites - приглашения, которые уже принимаются (sync повторяет их до join).
	invites map[string]bool
}

type agentRoom struct {
	lastEvent string
	prevBatch string
}

func newAgent(idx int, seed int64) *agent {
	return &agent{
		idx:     idx,
		rng:     rand.New(rand.NewPCG(uint64(seed), uint64(idx)+1)), //nolint:gosec // зерно - не секрет, переполнение не важно
		rooms:   map[string]*agentRoom{},
		invites: map[string]bool{},
	}
}

// float - случайное число [0, 1) из генератора пользователя.
func (a *agent) float() float64 {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.rng.Float64()
}

// intn - случайное число [0, n).
func (a *agent) intn(n int) int {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.rng.IntN(n)
}

// think - пауза перед следующим действием.
func (a *agent) think(mean time.Duration) time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()

	return thinkTime(a.rng, mean)
}

// randomRoom - случайная комната пользователя ("" - ни одной).
func (a *agent) randomRoom() (string, *agentRoom) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if len(a.rooms) == 0 {
		return "", nil
	}

	n := a.rng.IntN(len(a.rooms))
	for id, r := range a.rooms {
		if n == 0 {
			copied := *r

			return id, &copied
		}

		n--
	}

	return "", nil
}

// roomCount - число комнат пользователя.
func (a *agent) roomCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()

	return len(a.rooms)
}

// inRoom - состоит ли пользователь в комнате.
func (a *agent) inRoom(roomID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	_, ok := a.rooms[roomID]

	return ok
}

// noteTimeline запоминает последнее событие и prev_batch комнаты из /sync.
func (a *agent) noteTimeline(roomID, lastEvent, prevBatch string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	r := a.rooms[roomID]
	if r == nil {
		r = &agentRoom{}
		a.rooms[roomID] = r
	}

	if lastEvent != "" {
		r.lastEvent = lastEvent
	}

	if prevBatch != "" {
		r.prevBatch = prevBatch
	}
}

// claimInvite - true, если приглашение еще не принимается.
func (a *agent) claimInvite(roomID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.invites[roomID] {
		return false
	}

	a.invites[roomID] = true

	return true
}

func (a *agent) releaseInvite(roomID string) {
	a.mu.Lock()
	delete(a.invites, roomID)
	a.mu.Unlock()
}

// pause откладывает действия на d (429).
func (a *agent) pause(d time.Duration) {
	a.pausedUntil.Store(time.Now().Add(d).UnixNano())
}

// roomInfo - комната с точки зрения генератора.
type roomInfo struct {
	encrypted bool
	members   map[int]bool
}

// mediaRing - сколько последних mxc держим для скачивания.
const mediaRing = 1024

// world - общее состояние прогона: пользователи, комнаты, членства, медиа.
// Членство ведет генератор сам (по успешным join/leave и /sync): по нему
// считается, кто должен получить сообщение.
type world struct {
	agents []*agent

	mu           sync.RWMutex
	rooms        map[string]*roomInfo
	profileRooms []string
	media        []string
	mediaNext    int
	active       []*agent
}

func newWorld(agents []*agent) *world {
	return &world{agents: agents, rooms: map[string]*roomInfo{}}
}

// addRoom регистрирует комнату (повторно - без изменений).
func (w *world) addRoom(roomID string, encrypted, profile bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if _, ok := w.rooms[roomID]; ok {
		return
	}

	w.rooms[roomID] = &roomInfo{encrypted: encrypted, members: map[int]bool{}}
	if profile {
		w.profileRooms = append(w.profileRooms, roomID)
	}
}

// join отмечает вход пользователя в комнату.
func (w *world) join(a *agent, roomID string) {
	w.mu.Lock()

	r := w.rooms[roomID]
	if r == nil {
		r = &roomInfo{members: map[int]bool{}}
		w.rooms[roomID] = r
	}

	r.members[a.idx] = true
	w.mu.Unlock()

	a.noteTimeline(roomID, "", "")
}

// leave отмечает выход.
func (w *world) leave(a *agent, roomID string) {
	w.mu.Lock()
	if r := w.rooms[roomID]; r != nil {
		delete(r.members, a.idx)
	}
	w.mu.Unlock()

	a.mu.Lock()
	delete(a.rooms, roomID)
	a.mu.Unlock()
}

// isEncrypted - комната с m.room.encryption.
func (w *world) isEncrypted(roomID string) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()

	r := w.rooms[roomID]

	return r != nil && r.encrypted
}

// expectedRecipients - сколько участников комнаты (кроме отправителя) уже
// синхронизируются и должны увидеть событие.
func (w *world) expectedRecipients(roomID string, sender int) int {
	w.mu.RLock()
	defer w.mu.RUnlock()

	r := w.rooms[roomID]
	if r == nil {
		return 0
	}

	n := 0

	for idx := range r.members {
		if idx != sender && w.agents[idx].syncing.Load() {
			n++
		}
	}

	return n
}

// members - до limit user_id участников комнаты (для /keys/query).
func (w *world) members(roomID string, limit int) []string {
	w.mu.RLock()
	defer w.mu.RUnlock()

	r := w.rooms[roomID]
	if r == nil {
		return nil
	}

	out := make([]string, 0, min(limit, len(r.members)))
	for idx := range r.members {
		if len(out) >= limit {
			break
		}

		out = append(out, w.agents[idx].userID)
	}

	return out
}

// isMember - состоит ли пользователь idx в комнате.
func (w *world) isMember(roomID string, idx int) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()

	r := w.rooms[roomID]

	return r != nil && r.members[idx]
}

// randomProfileRoom - комната профиля по случайному числу u в [0, 1).
func (w *world) randomProfileRoom(u float64) string {
	w.mu.RLock()
	defer w.mu.RUnlock()

	if len(w.profileRooms) == 0 {
		return ""
	}

	return w.profileRooms[min(int(u*float64(len(w.profileRooms))), len(w.profileRooms)-1)]
}

// addMedia запоминает mxc для скачиваний (кольцо из mediaRing последних).
func (w *world) addMedia(mxc string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if len(w.media) < mediaRing {
		w.media = append(w.media, mxc)

		return
	}

	w.media[w.mediaNext] = mxc
	w.mediaNext = (w.mediaNext + 1) % mediaRing
}

// randomMedia - случайный mxc ("" - еще нет).
func (w *world) randomMedia(u float64) string {
	w.mu.RLock()
	defer w.mu.RUnlock()

	if len(w.media) == 0 {
		return ""
	}

	return w.media[min(int(u*float64(len(w.media))), len(w.media)-1)]
}

// activate добавляет пользователя в активные (получает действия в mode rps).
func (w *world) activate(a *agent) {
	w.mu.Lock()
	w.active = append(w.active, a)
	w.mu.Unlock()
}

// randomActive - случайный активный пользователь (nil - нет активных).
func (w *world) randomActive(u float64) *agent {
	w.mu.RLock()
	defer w.mu.RUnlock()

	if len(w.active) == 0 {
		return nil
	}

	return w.active[min(int(u*float64(len(w.active))), len(w.active)-1)]
}

// randomOther - случайный пользователь, кроме idx.
func (w *world) randomOther(u float64, idx int) *agent {
	n := len(w.agents)
	if n < 2 {
		return nil
	}

	pick := min(int(u*float64(n-1)), n-2)
	if pick >= idx {
		pick++
	}

	return w.agents[pick]
}

// roomStats - число комнат и членств (для отчета).
func (w *world) roomStats() (rooms, memberships int) {
	w.mu.RLock()
	defer w.mu.RUnlock()

	for _, r := range w.rooms {
		memberships += len(r.members)
	}

	return len(w.rooms), memberships
}

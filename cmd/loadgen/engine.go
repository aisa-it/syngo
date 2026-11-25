package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// engine - один прогон: подготовка, нагрузка, замер, drain.
type engine struct {
	prof   Profile
	target string
	runID  string
	log    *slog.Logger

	setup setupOptions
	cache *tokenCache

	// c - клиент замера (recorder прогона), setupC - подготовки (отдельная сводка).
	c       *client
	setupC  *client
	rec     *recorder
	setRec  *recorder
	tracker *deliveryTracker
	w       *world
	picker  *picker

	// payload - случайные байты для медиа (срез нужной длины без генерации на лету).
	payload []byte

	windowMu     sync.RWMutex
	measureStart time.Time
	measureEnd   time.Time

	inflight atomic.Int64

	// onWindow вызывается в начале и в конце окна замера (снимки метрик сервера).
	onWindowStart func()
	onWindowEnd   func()
}

// syncEvent - событие timeline в /sync (нужные поля).
type syncEvent struct {
	EventID string `json:"event_id"`
	Sender  string `json:"sender"`
	Type    string `json:"type"`
	Content struct {
		Marker *loadMarker `json:"io.syngo.loadgen"`
	} `json:"content"`
}

// syncResponse - ответ /sync (нужные поля).
type syncResponse struct {
	NextBatch string `json:"next_batch"`
	Rooms     struct {
		Join map[string]struct {
			Timeline struct {
				Events    []syncEvent `json:"events"`
				Limited   bool        `json:"limited"`
				PrevBatch string      `json:"prev_batch"`
			} `json:"timeline"`
		} `json:"join"`
		Invite map[string]json.RawMessage `json:"invite"`
		Leave  map[string]json.RawMessage `json:"leave"`
	} `json:"rooms"`
}

// inWindow - момент попадает в окно замера.
func (e *engine) inWindow(t time.Time) bool {
	e.windowMu.RLock()
	defer e.windowMu.RUnlock()

	return !e.measureStart.IsZero() && !t.Before(e.measureStart) && (e.measureEnd.IsZero() || t.Before(e.measureEnd))
}

// syncLoop - long-poll /sync пользователя до отмены ctx.
func (e *engine) syncLoop(ctx, actionsCtx context.Context, a *agent) {
	since := ""
	timeoutMS := e.prof.SyncTimeout.D().Milliseconds()

	for ctx.Err() == nil {
		endpoint := "sync_longpoll"
		q := url.Values{}
		q.Set("filter", a.filterID)

		if since == "" {
			endpoint = "sync_initial"

			q.Set("timeout", "0")
		} else {
			q.Set("since", since)
			q.Set("timeout", strconv.FormatInt(timeoutMS, 10))
		}

		var resp syncResponse

		err := e.c.call(ctx, request{
			endpoint: endpoint, method: http.MethodGet, path: "/_matrix/client/v3/sync?" + q.Encode(),
			token: a.token, out: &resp,
		})
		now := time.Now()

		if err != nil {
			if ctx.Err() != nil {
				return
			}

			wait := time.Second

			var ae *apiError
			if asAPIError(err, &ae) && ae.RetryAfter > 0 {
				wait = min(ae.RetryAfter, 10*time.Second)
			}

			e.log.Debug("sync", "user", a.localpart, "error", err)
			sleepCtx(ctx, wait)

			continue
		}

		e.handleSync(actionsCtx, a, &resp, now, since == "")

		if resp.NextBatch != "" {
			since = resp.NextBatch
		}

		a.syncing.Store(true)
	}
}

// handleSync разбирает ответ: доставка меток, последние события, приглашения, выходы.
func (e *engine) handleSync(ctx context.Context, a *agent, resp *syncResponse, now time.Time, initial bool) {
	for roomID, room := range resp.Rooms.Join {
		if room.Timeline.Limited && !initial {
			e.rec.count("sync_limited_timelines", 1)
		}

		last := ""

		for _, ev := range room.Timeline.Events {
			last = ev.EventID

			if ev.Content.Marker != nil && ev.Sender != a.userID {
				e.tracker.seen(ev.EventID, *ev.Content.Marker, now)
			}
		}

		if !a.inRoom(roomID) {
			e.w.join(a, roomID)
		}

		a.noteTimeline(roomID, last, room.Timeline.PrevBatch)
	}

	for roomID := range resp.Rooms.Leave {
		e.w.leave(a, roomID)
	}

	for roomID := range resp.Rooms.Invite {
		if ctx.Err() != nil || !a.claimInvite(roomID) {
			continue
		}

		go e.acceptInvite(ctx, a, roomID)
	}
}

// acceptInvite - вход по приглашению (как пользователь, нажавший «принять»).
func (e *engine) acceptInvite(ctx context.Context, a *agent, roomID string) {
	defer a.releaseInvite(roomID)

	err := e.c.call(ctx, request{
		endpoint: "join_invite", method: http.MethodPost,
		path:  "/_matrix/client/v3/rooms/" + pathEscape(roomID) + "/join",
		token: a.token, body: map[string]any{},
	})
	if err == nil {
		e.w.join(a, roomID)
	}
}

// userLoop - закрытая модель: действие, пауза think time, действие...
func (e *engine) userLoop(ctx context.Context, a *agent) {
	for {
		if !sleepCtx(ctx, a.think(e.prof.ThinkTime.D())) {
			return
		}

		e.waitPause(ctx, a)

		if ctx.Err() != nil {
			return
		}

		e.runAction(ctx, a, e.picker.pick(a.float()))
	}
}

// rpsLoop - открытая модель: действия по расписанию fireAt случайным активным
// пользователям; сверх max_inflight действие пропускается (dropped).
func (e *engine) rpsLoop(ctx context.Context, start time.Time) {
	var (
		wg  sync.WaitGroup
		rng = newAgent(-1, e.prof.Seed)
	)

	defer wg.Wait()

	for k := int64(1); ; k++ {
		if !sleepCtx(ctx, time.Until(start.Add(fireAt(k, e.prof.TargetRPS, e.prof.Ramp.D())))) {
			return
		}

		a := e.w.randomActive(rng.float())
		if a == nil || time.Now().UnixNano() < a.pausedUntil.Load() {
			e.rec.count("rps_skipped", 1)

			continue
		}

		if e.inflight.Load() >= int64(e.prof.MaxInflight) {
			e.rec.count("rps_dropped_inflight", 1)

			continue
		}

		e.inflight.Add(1)
		wg.Add(1)

		go func() {
			defer wg.Done()
			defer e.inflight.Add(-1)

			e.runAction(ctx, a, e.picker.pick(rng.float()))
		}()
	}
}

// waitPause ждет окончания паузы после 429.
func (e *engine) waitPause(ctx context.Context, a *agent) {
	if until := a.pausedUntil.Load(); until > 0 {
		sleepCtx(ctx, time.Until(time.Unix(0, until)))
	}
}

// runAction выполняет действие; 429 откладывает следующие действия пользователя.
func (e *engine) runAction(ctx context.Context, a *agent, name string) {
	// Счетчики пишутся только в окне замера (recorder.count).
	e.rec.count("actions", 1)
	e.rec.count("action_"+name, 1)

	var err error

	switch name {
	case actSend:
		err = e.actSend(ctx, a)
	case actTyping:
		err = e.actTyping(ctx, a)
	case actReceipt:
		err = e.actReceipt(ctx, a)
	case actMessages:
		err = e.actMessages(ctx, a)
	case actKeysQuery:
		err = e.actKeysQuery(ctx, a)
	case actKeysUpload:
		err = e.actKeysUpload(ctx, a)
	case actMediaUpload:
		err = e.uploadMedia(ctx, e.c, a)
	case actMediaDownload:
		err = e.actMediaDownload(ctx, a)
	case actCreateRoom:
		err = e.actCreateRoom(ctx, a)
	case actJoinLeave:
		err = e.actJoinLeave(ctx, a)
	case actInvite:
		err = e.actInvite(ctx, a)
	}

	if err == nil {
		return
	}

	var ae *apiError
	if asAPIError(err, &ae) && ae.Status == http.StatusTooManyRequests {
		a.pause(max(ae.RetryAfter, time.Second))
	}

	if ctx.Err() == nil {
		e.log.Debug("action", "action", name, "user", a.localpart, "error", err)
	}
}

// actSend - сообщение (в шифрованной комнате - m.room.encrypted) и учет доставки.
func (e *engine) actSend(ctx context.Context, a *agent) error {
	roomID, _ := a.randomRoom()
	if roomID == "" {
		return nil
	}

	start := time.Now()
	content := map[string]any{markerKey: loadMarker{Run: e.runID, TS: start.UnixMicro()}}
	evType, endpoint := "m.room.message", "send_text"

	if e.w.isEncrypted(roomID) {
		evType, endpoint = "m.room.encrypted", "send_encrypted"
		content["algorithm"] = "m.megolm.v1.aes-sha2"
		content["ciphertext"] = randomKey(e.prof.MessageBytes)
		content["sender_key"] = a.curveKey
		content["device_id"] = a.deviceID
		content["session_id"] = "loadgen" + e.runID
	} else {
		content["msgtype"] = "m.text"
		content["body"] = e.messageBody(a)
	}

	txn := fmt.Sprintf("lg%s.%d", e.runID, a.txn.Add(1))

	var out struct {
		EventID string `json:"event_id"`
	}

	err := e.c.call(ctx, request{
		endpoint: endpoint, method: http.MethodPut,
		path:  "/_matrix/client/v3/rooms/" + pathEscape(roomID) + "/send/" + evType + "/" + pathEscape(txn),
		token: a.token, body: content, out: &out,
	})
	if err != nil {
		return err
	}

	ack := time.Now()
	e.tracker.acked(out.EventID, start, ack, e.w.expectedRecipients(roomID, a.idx), e.inWindow(start))

	return nil
}

// messageBody - текст сообщения длиной message_bytes.
func (e *engine) messageBody(a *agent) string {
	const words = "lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod tempor "

	n := e.prof.MessageBytes
	offset := a.intn(len(words))

	var b strings.Builder

	b.Grow(n)

	for b.Len() < n {
		b.WriteString(words[offset:])
		offset = 0
	}

	return b.String()[:n]
}

func (e *engine) actTyping(ctx context.Context, a *agent) error {
	roomID, _ := a.randomRoom()
	if roomID == "" {
		return nil
	}

	err := e.c.call(ctx, request{
		endpoint: "typing", method: http.MethodPut,
		path:  "/_matrix/client/v3/rooms/" + pathEscape(roomID) + "/typing/" + pathEscape(a.userID),
		token: a.token, body: map[string]any{"typing": true, "timeout": 5000},
	})

	return err
}

func (e *engine) actReceipt(ctx context.Context, a *agent) error {
	roomID, room := a.randomRoom()
	if roomID == "" || room.lastEvent == "" {
		return nil
	}

	err := e.c.call(ctx, request{
		endpoint: "receipt", method: http.MethodPost,
		path:  "/_matrix/client/v3/rooms/" + pathEscape(roomID) + "/receipt/m.read/" + pathEscape(room.lastEvent),
		token: a.token, body: map[string]any{},
	})

	return err
}

// actMessages - история назад от prev_batch из /sync (как прокрутка вверх).
func (e *engine) actMessages(ctx context.Context, a *agent) error {
	roomID, room := a.randomRoom()
	if roomID == "" {
		return nil
	}

	q := url.Values{}
	q.Set("dir", "b")
	q.Set("limit", "30")
	q.Set("filter", `{"lazy_load_members":true}`)

	if room.prevBatch != "" {
		q.Set("from", room.prevBatch)
	}

	err := e.c.call(ctx, request{
		endpoint: "messages", method: http.MethodGet,
		path:  "/_matrix/client/v3/rooms/" + pathEscape(roomID) + "/messages?" + q.Encode(),
		token: a.token,
	})

	return err
}

// actKeysQuery - ключи участников комнаты (Element делает это при открытии комнаты).
func (e *engine) actKeysQuery(ctx context.Context, a *agent) error {
	roomID, _ := a.randomRoom()
	if roomID == "" {
		return nil
	}

	query := map[string][]string{}
	for _, u := range e.w.members(roomID, 20) {
		query[u] = []string{}
	}

	err := e.c.call(ctx, request{
		endpoint: "keys_query", method: http.MethodPost, path: "/_matrix/client/v3/keys/query",
		token: a.token, body: map[string]any{"device_keys": query, "timeout": 10000},
	})

	return err
}

func (e *engine) actKeysUpload(ctx context.Context, a *agent) error {
	err := e.c.call(ctx, request{
		endpoint: "keys_upload", method: http.MethodPost, path: "/_matrix/client/v3/keys/upload",
		token: a.token, body: map[string]any{"one_time_keys": e.oneTimeKeys(a, 5)},
	})

	return err
}

// uploadMedia - файл media_kb ±50%; mxc запоминается для скачиваний.
func (e *engine) uploadMedia(ctx context.Context, c *client, a *agent) error {
	size := int(float64(e.prof.MediaKB*1024) * (0.5 + a.float()))
	size = min(max(size, 1), len(e.payload))

	var out struct {
		ContentURI string `json:"content_uri"`
	}

	err := c.call(ctx, request{
		endpoint: "media_upload", method: http.MethodPost, path: "/_matrix/media/v3/upload?filename=loadgen.bin",
		token: a.token, raw: e.payload[:size], contentType: "application/octet-stream", out: &out,
	})
	if err != nil {
		return err
	}

	if out.ContentURI != "" {
		e.w.addMedia(out.ContentURI)
	}

	return nil
}

// actMediaDownload - аутентифицированное скачивание (/_matrix/client/v1/media).
func (e *engine) actMediaDownload(ctx context.Context, a *agent) error {
	server, mediaID, ok := strings.Cut(strings.TrimPrefix(e.w.randomMedia(a.float()), "mxc://"), "/")
	if !ok {
		return nil
	}

	err := e.c.call(ctx, request{
		endpoint: "media_download", method: http.MethodGet,
		path:  "/_matrix/client/v1/media/download/" + pathEscape(server) + "/" + pathEscape(mediaID),
		token: a.token,
	})

	return err
}

// actCreateRoom - приватная комната с двумя приглашенными (они примут из /sync).
func (e *engine) actCreateRoom(ctx context.Context, a *agent) error {
	// Без повторов: повтор в invite давал 500 у syngod (materialized state 8/9).
	invite := []string{}

	for range 2 {
		other := e.w.randomOther(a.float(), a.idx)
		if other != nil && other.userID != "" && (len(invite) == 0 || invite[0] != other.userID) {
			invite = append(invite, other.userID)
		}
	}

	var out struct {
		RoomID string `json:"room_id"`
	}

	err := e.c.call(ctx, request{
		endpoint: "create_room", method: http.MethodPost, path: "/_matrix/client/v3/createRoom",
		token: a.token, body: map[string]any{"preset": "private_chat", "name": "loadgen " + e.runID, "invite": invite},
		out: &out,
	})
	if err != nil {
		return err
	}

	e.w.addRoom(out.RoomID, false, false)
	e.w.join(a, out.RoomID)

	return nil
}

// actJoinLeave держит число комнат пользователя около rooms_per_user: больше -
// выход из случайной, иначе вход в случайную комнату профиля.
func (e *engine) actJoinLeave(ctx context.Context, a *agent) error {
	if a.roomCount() > e.prof.RoomsPerUser {
		roomID, _ := a.randomRoom()

		err := e.c.call(ctx, request{
			endpoint: "leave", method: http.MethodPost,
			path:  "/_matrix/client/v3/rooms/" + pathEscape(roomID) + "/leave",
			token: a.token, body: map[string]any{},
		})
		if err == nil {
			e.w.leave(a, roomID)
		}

		return err
	}

	roomID := e.w.randomProfileRoom(a.float())
	if roomID == "" || a.inRoom(roomID) {
		return nil
	}

	err := e.c.call(ctx, request{
		endpoint: "join", method: http.MethodPost, path: "/_matrix/client/v3/join/" + pathEscape(roomID),
		token: a.token, body: map[string]any{},
	})
	if err == nil {
		e.w.join(a, roomID)
	}

	return err
}

// actInvite - приглашение случайного пользователя в свою комнату.
func (e *engine) actInvite(ctx context.Context, a *agent) error {
	roomID, _ := a.randomRoom()
	other := e.w.randomOther(a.float(), a.idx)

	if roomID == "" || other == nil || other.userID == "" || e.w.isMember(roomID, other.idx) {
		return nil
	}

	err := e.c.call(ctx, request{
		endpoint: "invite", method: http.MethodPost,
		path:  "/_matrix/client/v3/rooms/" + pathEscape(roomID) + "/invite",
		token: a.token, body: map[string]any{"user_id": other.userID},
	})

	return err
}

// runLoad - нагрузка по профилю: подключение пользователей за ramp, окно замера
// [warmup, warmup+duration), затем drain (действия стоят, /sync ждет доставки).
func (e *engine) runLoad(ctx context.Context) {
	var wg sync.WaitGroup

	// Порядок defer: сначала отмена, потом ожидание горутин.
	defer wg.Wait()

	syncCtx, stopSync := context.WithCancel(ctx)
	defer stopSync()

	actCtx, stopActions := context.WithCancel(ctx)
	defer stopActions()

	start := time.Now()

	wg.Add(1)

	go func() {
		defer wg.Done()

		for i, a := range e.w.agents {
			if !sleepCtx(actCtx, time.Until(start.Add(activationOffset(i, len(e.w.agents), e.prof.Ramp.D())))) {
				return
			}

			e.w.activate(a)
			wg.Add(1)

			go func() {
				defer wg.Done()
				e.syncLoop(syncCtx, actCtx, a)
			}()

			if e.prof.Mode == modeUsers {
				wg.Add(1)

				go func() {
					defer wg.Done()
					e.userLoop(actCtx, a)
				}()
			}
		}
	}()

	if e.prof.Mode == modeRPS {
		wg.Add(1)

		go func() {
			defer wg.Done()
			e.rpsLoop(actCtx, start)
		}()
	}

	if !sleepCtx(ctx, time.Until(start.Add(e.prof.Warmup.D()))) {
		return
	}

	e.windowMu.Lock()
	e.measureStart = time.Now()
	e.windowMu.Unlock()
	e.rec.measuring.Store(true)

	if e.onWindowStart != nil {
		e.onWindowStart()
	}

	e.log.Info("замер начат", "duration", e.prof.Duration.D())

	if !sleepCtx(ctx, e.prof.Duration.D()) {
		return
	}

	e.windowMu.Lock()
	e.measureEnd = time.Now()
	e.windowMu.Unlock()
	e.rec.measuring.Store(false)

	if e.onWindowEnd != nil {
		e.onWindowEnd()
	}

	stopActions()
	e.log.Info("замер окончен, drain", "drain", e.prof.Drain.D())
	sleepCtx(ctx, e.prof.Drain.D())
}

// window - фактическая длительность окна замера.
func (e *engine) window() time.Duration {
	e.windowMu.RLock()
	defer e.windowMu.RUnlock()

	if e.measureStart.IsZero() {
		return 0
	}

	end := e.measureEnd
	if end.IsZero() {
		end = time.Now()
	}

	return end.Sub(e.measureStart)
}

// asAPIError - errors.As для *apiError.
func asAPIError(err error, target **apiError) bool { return errors.As(err, target) }

// sleepCtx спит d; false - ctx отменен.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}

	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/coretest"
	"github.com/aisa-it/syngo/internal/pushers"
)

const pushRoom = "!push:test"

// memMemberships - комнаты пользователя для досчета старых событий.
type memMemberships struct {
	core.MembershipStore

	rooms map[string][]core.RoomMembership
	err   error
}

func (m memMemberships) MembershipRooms(_ context.Context, userID string) ([]core.RoomMembership, error) {
	return m.rooms[userID], m.err
}

func pushMsg(id, sender string, stream int64, content string) core.Event {
	return core.Event{
		EventID: id, RoomID: pushRoom, Type: "m.room.message", Sender: sender,
		StreamOrdering: stream, Content: json.RawMessage(content),
	}
}

// newPushStore - комната alice и bob (вошел на позиции 2) с тремя сообщениями
// alice: обычным, с упоминанием bob и ответом в треде первого.
func newPushStore() *coretest.MemPushActionsStore {
	st := coretest.NewMemPushActionsStore()
	st.Members[pushRoom] = []core.PushRecipient{{UserID: alice, JoinStream: 1}, {UserID: bob, JoinStream: 2}}
	st.Events = []core.Event{
		pushMsg("$e1", alice, 10, `{"msgtype":"m.text","body":"один"}`),
		pushMsg("$e2", alice, 11, `{"msgtype":"m.text","body":"два","m.mentions":{"user_ids":["`+bob+`"]}}`),
		pushMsg("$e3", alice, 12, `{"msgtype":"m.text","body":"тред","m.relates_to":{"rel_type":"m.thread","event_id":"$e1"}}`),
	}

	return st
}

// Хранимые действия: разбор до позиции, счетчики по
// тредам с highlight, сброс по receipt основной ленты и без треда, бейдж push.
func TestPushActionsServiceCounts(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newPushStore()
	svc := core.NewPushActionsService(st, core.PushActionsOptions{})

	if err := svc.CatchUp(ctx, 12); err != nil {
		t.Fatal(err)
	}

	counts, ok := svc.UnreadCounts(ctx, bob, 12, []string{pushRoom})
	if !ok || counts[pushRoom][""] != (core.PushCounts{Notification: 2, Highlight: 1}) ||
		counts[pushRoom]["$e1"].Notification != 1 {
		t.Fatalf("счетчики bob: %v %v", counts, ok)
	}

	if counts, _ := svc.UnreadCounts(ctx, alice, 12, []string{pushRoom}); len(counts) != 0 {
		t.Fatalf("свои события посчитаны: %v", counts)
	}

	// m.fully_read счетчики не сбрасывает; receipt основной ленты - только ее.
	for _, receipt := range []core.ReceiptEvent{
		{RoomID: pushRoom, UserID: bob, EventID: "$e2", Type: core.ReceiptFullyRead},
		{RoomID: pushRoom, UserID: bob, EventID: "$e2", Type: core.ReceiptRead, ThreadID: "main"},
	} {
		if err := svc.ApplyReceipt(ctx, receipt); err != nil {
			t.Fatal(err)
		}
	}

	if counts, _ = svc.UnreadCounts(ctx, bob, 12, []string{pushRoom}); len(counts[pushRoom]) != 1 ||
		counts[pushRoom]["$e1"].Notification != 1 {
		t.Fatalf("после receipt основной ленты: %v", counts)
	}

	if rooms, err := svc.UnreadRooms(ctx, bob); err != nil || rooms != 1 {
		t.Fatalf("бейдж: %d %v", rooms, err)
	}

	if err := svc.ApplyReceipt(ctx, core.ReceiptEvent{RoomID: pushRoom, UserID: bob, EventID: "$e3", Type: core.ReceiptReadPrivate}); err != nil {
		t.Fatal(err)
	}

	if rooms, _ := svc.UnreadRooms(ctx, bob); rooms != 0 {
		t.Fatalf("receipt без треда не сбросил все: %d", rooms)
	}

	// Свое событие - неявное прочтение, выход снимает уведомления комнаты.
	st.Events = append(st.Events,
		pushMsg("$e4", alice, 13, `{"msgtype":"m.text","body":"четыре"}`),
		pushMsg("$e5", bob, 14, `{"msgtype":"m.text","body":"ответ"}`),
		pushMsg("$e6", alice, 15, `{"msgtype":"m.text","body":"шесть"}`))

	if err := svc.CatchUpNow(ctx); err != nil {
		t.Fatal(err)
	}

	if counts, _ = svc.UnreadCounts(ctx, bob, 15, []string{pushRoom}); counts[pushRoom][""].Notification != 1 {
		t.Fatalf("после своего события: %v", counts)
	}

	leave := "@bob:test"
	st.Events = append(st.Events, core.Event{
		EventID: "$leave", RoomID: pushRoom, Type: "m.room.member", Sender: bob, StateKey: &leave,
		StreamOrdering: 16, Content: json.RawMessage(`{"membership":"leave"}`),
	})

	if err := svc.CatchUp(ctx, 16); err != nil {
		t.Fatal(err)
	}

	if rooms, _ := svc.UnreadRooms(ctx, bob); rooms != 0 {
		t.Fatalf("после выхода: %d", rooms)
	}
}

// Разбор порциями по 500 событий; большое отставание на старте - история
// (позиция переносится без разбора); сбой хранилища - ошибка и без счетчиков.
func TestPushActionsServiceBatchesAndFailures(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newPushStore()
	st.Events = st.Events[:0]

	for i := range 501 {
		st.Events = append(st.Events, pushMsg(fmt.Sprintf("$m%d", i), alice, int64(10+i), `{"msgtype":"m.text","body":"x"}`))
	}

	svc := core.NewPushActionsService(st, core.PushActionsOptions{})
	if err := svc.CatchUpNow(ctx); err != nil {
		t.Fatal(err)
	}

	if st.Applied != 2 || st.Processed != 510 || len(st.Actions) != 501 {
		t.Fatalf("порции: applied=%d processed=%d actions=%d", st.Applied, st.Processed, len(st.Actions))
	}

	backlog := newPushStore()
	backlog.Events = append(backlog.Events, pushMsg("$late", alice, 20000, `{"body":"x"}`))

	var (
		warnMu sync.Mutex
		warned []string
	)

	// Сбои фонового разбора и досчета пишутся из своих горутин.
	warn := func(_ context.Context, msg string, _ error, _ ...any) {
		warnMu.Lock()
		defer warnMu.Unlock()

		warned = append(warned, msg)
	}

	svc = core.NewPushActionsService(backlog, core.PushActionsOptions{Warn: warn})

	if err := svc.CatchUp(ctx, 20000); err != nil {
		t.Fatal(err)
	}

	if backlog.Processed != 20000 || backlog.BackfillUpto != 20000 || len(backlog.Actions) != 0 {
		t.Fatalf("история разобрана по событиям: %+v", backlog.Actions)
	}

	failing := newPushStore()
	failing.Err = errors.New("db down")
	svc = core.NewPushActionsService(failing, core.PushActionsOptions{Warn: warn})

	if _, ok := svc.UnreadCounts(ctx, bob, 12, []string{pushRoom}); ok {
		t.Fatal("сбой хранилища отдал счетчики")
	}

	if _, err := svc.UnreadRooms(ctx, bob); err == nil {
		t.Fatal("сбой хранилища без ошибки бейджа")
	}

	if err := svc.ApplyReceipt(ctx, core.ReceiptEvent{RoomID: pushRoom, UserID: bob, EventID: "$e1", Type: core.ReceiptRead}); err == nil {
		t.Fatal("сбой хранилища без ошибки receipt")
	}

	if targets, err := svc.PushTargets(ctx, st.Events[0]); targets != nil || err != nil {
		t.Fatalf("сбой выборки pushers: %v %v", targets, err)
	}

	// Досчет и разбор - в фоне: /sync ждет их и видит сбой разбора, бейдж
	// повторяет досчет.
	want := []string{
		"push actions backlog treated as history", "push actions backfill failed", "push actions failed",
		"push actions backfill failed",
	}

	warnMu.Lock()
	defer warnMu.Unlock()

	if fmt.Sprint(warned) != fmt.Sprint(want) {
		t.Fatalf("журнал: %v", warned)
	}
}

// Досчет старых событий (до миграции 0039): один раз на пользователя, от входа
// и m.fully_read, свои события поднимают отметку своей ленты.
func TestPushActionsServiceBackfill(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newPushStore()
	st.Processed, st.BackfillUpto = 12, 12
	st.FullyRead[bob] = map[string][]byte{pushRoom: []byte(`{"event_id":"$e1"}`)}
	members := memMemberships{rooms: map[string][]core.RoomMembership{
		bob: {{RoomID: pushRoom, Membership: "join", LeaveStream: 2}, {RoomID: "!left:test", Membership: "leave"}},
	}}

	svc := core.NewPushActionsService(st, core.PushActionsOptions{Members: members, Ephemeral: core.NewEphemeralStore()})

	counts, ok := svc.UnreadCounts(ctx, bob, 12, []string{pushRoom})
	if !ok || counts[pushRoom][""].Notification != 1 || counts[pushRoom]["$e1"].Notification != 1 || !st.Backfilled[bob] {
		t.Fatalf("досчет: %v %v", counts, st.Backfilled)
	}

	// Повторно (в том числе новым процессом) досчет не выполняется.
	applied := st.Applied
	if err := svc.EnsureBackfilled(ctx, bob); err != nil || st.Applied != applied {
		t.Fatalf("повторный досчет: %v %d", err, st.Applied)
	}

	again := core.NewPushActionsService(st, core.PushActionsOptions{Members: members})
	if err := again.EnsureBackfilled(ctx, bob); err != nil || st.Applied != applied {
		t.Fatalf("досчет новым процессом: %v %d", err, st.Applied)
	}

	// Без комнат - пустой досчет; сбой чтения комнат - ошибка.
	if err := again.EnsureBackfilled(ctx, alice); err != nil || !st.Backfilled[alice] {
		t.Fatalf("без комнат: %v", err)
	}

	broken := core.NewPushActionsService(st, core.PushActionsOptions{Members: memMemberships{err: errors.New("db down")}})
	if err := broken.EnsureBackfilled(ctx, "@carol:test"); err == nil {
		t.Fatal("сбой чтения комнат без ошибки")
	}
}

// Получатели push: только с notify, все их pushers;
// приглашенный получает push о приглашении.
func TestPushActionsServicePushTargets(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newPushStore()
	st.Pushers = []pushers.Pusher{
		{UserID: bob, AppID: "app", Pushkey: "k1"},
		{UserID: bob, AppID: "app", Pushkey: "k2"},
		{UserID: alice, AppID: "app", Pushkey: "a"},
		{UserID: "@carol:test", AppID: "app", Pushkey: "c"},
	}

	svc := core.NewPushActionsService(st, core.PushActionsOptions{})

	targets, err := svc.PushTargets(ctx, st.Events[1])
	if err != nil || len(targets) != 1 || targets[0].UserID != bob || len(targets[0].Pushers) != 2 || !targets[0].Result.Highlight {
		t.Fatalf("получатели сообщения: %+v %v", targets, err)
	}

	carol := "@carol:test"
	invite := core.Event{
		EventID: "$inv", RoomID: pushRoom, Type: "m.room.member", Sender: alice, StateKey: &carol,
		StreamOrdering: 13, Content: json.RawMessage(`{"membership":"invite"}`),
	}

	targets, err = svc.PushTargets(ctx, invite)
	if err != nil || len(targets) != 1 || targets[0].UserID != carol {
		t.Fatalf("получатели приглашения: %+v %v", targets, err)
	}

	// Без pushers получателей нет.
	if targets, err = core.NewPushActionsService(newPushStore(), core.PushActionsOptions{}).PushTargets(ctx, invite); err != nil || targets != nil {
		t.Fatalf("без pushers: %+v %v", targets, err)
	}
}

// /notifications: от новых к старым по страницам,
// only=highlight, поле read по receipts в памяти.
func TestNotificationsService(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newPushStore()
	ephemeral := core.NewEphemeralStore()
	ephemeral.SetReceipt(pushRoom, bob, core.ReceiptRead, "", "$e2")

	svc := core.NewNotificationsService(st, core.NewReadStates(st, ephemeral, nil))

	page, err := svc.List(ctx, bob, 0, 2, false)
	if err != nil || len(page.Items) != 2 || page.Exhausted || page.Next != 11 ||
		page.Items[0].Event.EventID != "$e3" || page.Items[0].Read || !page.Items[1].Read {
		t.Fatalf("первая страница: %+v %v", page, err)
	}

	page, err = svc.List(ctx, bob, page.Next, 2, false)
	if err != nil || len(page.Items) != 1 || !page.Exhausted || page.Items[0].Event.EventID != "$e1" {
		t.Fatalf("вторая страница: %+v %v", page, err)
	}

	page, err = svc.List(ctx, bob, 0, 10, true)
	if err != nil || len(page.Items) != 1 || page.Items[0].Event.EventID != "$e2" || !page.Exhausted {
		t.Fatalf("only=highlight: %+v %v", page, err)
	}

	if page, err = svc.List(ctx, alice, 0, 10, false); err != nil || len(page.Items) != 0 || !page.Exhausted {
		t.Fatalf("свои события: %+v %v", page, err)
	}

	st.Err = errors.New("db down")
	if _, err := svc.List(ctx, bob, 0, 10, false); err == nil {
		t.Fatal("сбой хранилища без ошибки")
	}
}

// Отметки прочтения после рестарта: без receipts - m.fully_read; сбои чтения -
// пустые отметки и запись в журнал.
func TestReadStates(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newPushStore()
	st.FullyRead[bob] = map[string][]byte{pushRoom: []byte(`{"event_id":"$e2"}`), "!other:test": []byte(`{}`)}

	states := core.NewReadStates(st, nil, nil).For(ctx, bob, []string{pushRoom, "!other:test"})
	if !states[pushRoom].IsRead(11, "") || !states[pushRoom].IsRead(11, "$e1") || states[pushRoom].IsRead(12, "") ||
		states["!other:test"].IsRead(1, "") {
		t.Fatalf("m.fully_read: %+v", states)
	}

	ephemeral := core.NewEphemeralStore()
	ephemeral.SetReceipt(pushRoom, bob, core.ReceiptRead, "main", "$e1")
	ephemeral.SetReceipt(pushRoom, bob, core.ReceiptRead, "$e1", "$e3")

	states = core.NewReadStates(st, ephemeral, nil).For(ctx, bob, []string{pushRoom})
	if !states[pushRoom].IsRead(10, "") || states[pushRoom].IsRead(11, "") || !states[pushRoom].IsRead(12, "$e1") {
		t.Fatalf("receipts тредов: %+v", states[pushRoom])
	}

	var warned []string

	st.Err = errors.New("db down")
	states = core.NewReadStates(st, ephemeral, func(_ context.Context, msg string, _ error) {
		warned = append(warned, msg)
	}).For(ctx, bob, []string{pushRoom})

	if states[pushRoom].IsRead(10, "") || fmt.Sprint(warned) != "[fully_read markers failed receipt positions failed]" {
		t.Fatalf("сбой: %+v %v", states[pushRoom], warned)
	}
}

// slowPushStore - хранилище с медленной записью порции (INSERT в
// event_push_actions под нагрузкой); gate задерживает первую запись до
// закрытия, entered закрывается при входе в нее.
type slowPushStore struct {
	*coretest.MemPushActionsStore

	delay   time.Duration
	entered chan struct{}
	gate    chan struct{}
	once    sync.Once
}

func (s *slowPushStore) ApplyPushBatch(ctx context.Context, b core.PushBatch) error {
	if s.gate != nil {
		s.once.Do(func() {
			close(s.entered)
			<-s.gate
		})
	}

	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return ctx.Err()
	}

	return s.MemPushActionsStore.ApplyPushBatch(ctx, b)
}

// Отправки и /sync не сериализуются на разборе. Раньше каждый /sync
// догонял до своей позиции под общим мьютексом, и N запросов с растущими
// позициями делали N порций подряд (N * delay); теперь отправка только
// сигналит, разбор - один фоновый проход, /sync ждет не дольше SyncWait.
func TestPushActionsServiceConcurrentSendAndSync(t *testing.T) {
	t.Parallel()

	const (
		n     = 40
		delay = 100 * time.Millisecond
	)

	ctx := context.Background()
	st := newPushStore()

	for i := range n {
		st.Events = append(st.Events, pushMsg(fmt.Sprintf("$c%d", i), alice, int64(13+i), `{"msgtype":"m.text","body":"x"}`))
	}

	svc := core.NewPushActionsService(&slowPushStore{MemPushActionsStore: st, delay: delay},
		core.PushActionsOptions{SyncWait: 20 * time.Millisecond})
	t.Cleanup(func() { _ = svc.Close(context.Background()) })

	start := time.Now()

	var wg sync.WaitGroup

	for i := range n {
		wg.Add(2)

		go func() {
			defer wg.Done()

			svc.Signal()
		}()

		go func() {
			defer wg.Done()

			if _, ok := svc.UnreadCounts(ctx, bob, int64(13+i), []string{pushRoom}); !ok {
				t.Error("счетчики не отданы")
			}
		}()
	}

	wg.Wait()

	// Последовательный догон - не меньше n*delay (4 с); здесь - ожидание /sync,
	// ограниченное SyncWait, с запасом на -race.
	if elapsed := time.Since(start); elapsed > n*delay/4 {
		t.Fatalf("отправки и /sync сериализованы: %v на %d пар", elapsed, n)
	}

	// После разбора счетчики полные: 2 + n в основной ленте, 1 в треде.
	svc.SetSyncWait(10 * time.Second)

	counts, ok := svc.UnreadCounts(ctx, bob, int64(12+n), []string{pushRoom})
	if !ok || counts[pushRoom][""] != (core.PushCounts{Notification: 2 + n, Highlight: 1}) ||
		counts[pushRoom]["$e1"].Notification != 1 {
		t.Fatalf("счетчики после отправок: %v %v", counts, ok)
	}
}

// /sync ждет разбора не дольше SyncWait и отдает уже разобранное;
// Close прерывает фоновый проход, Signal после Close ничего не запускает.
func TestPushActionsServiceBoundedWait(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newPushStore()
	svc := core.NewPushActionsService(&slowPushStore{MemPushActionsStore: st, delay: time.Minute},
		core.PushActionsOptions{SyncWait: 50 * time.Millisecond})

	start := time.Now()

	counts, ok := svc.UnreadCounts(ctx, bob, 12, []string{pushRoom})
	if !ok || len(counts) != 0 {
		t.Fatalf("до разбора: %v %v", counts, ok)
	}

	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("/sync ждал разбора %v", elapsed)
	}

	closeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := svc.Close(closeCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	svc.Signal()

	if err := svc.Close(closeCtx); err != nil || st.Applied != 0 {
		t.Fatalf("после Close: %v applied=%d", err, st.Applied)
	}
}

// receipt, сохраненный, пока порция разбирается (отметки прочтения
// уже прочитаны, строки еще не записаны), не оставляет прочитанных уведомлений:
// ApplyReceipt не ждет разбора, а применяется повторно после записи порции.
func TestPushActionsServiceReceiptDuringBatch(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newPushStore()
	slow := &slowPushStore{MemPushActionsStore: st, entered: make(chan struct{}), gate: make(chan struct{})}
	svc := core.NewPushActionsService(slow, core.PushActionsOptions{SyncWait: 10 * time.Second})
	t.Cleanup(func() { _ = svc.Close(context.Background()) })

	svc.Signal()
	<-slow.entered

	done := make(chan error, 1)

	go func() {
		done <- svc.ApplyReceipt(ctx, core.ReceiptEvent{RoomID: pushRoom, UserID: bob, EventID: "$e3", Type: core.ReceiptRead})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("receipt ждет разбора порции")
	}

	close(slow.gate)

	if counts, ok := svc.UnreadCounts(ctx, bob, 12, []string{pushRoom}); !ok || len(counts) != 0 {
		t.Fatalf("прочитанное посчитано: %v %v", counts, ok)
	}
}

// Досчет старых событий идет в фоне при старте, без /sync.
func TestPushActionsServiceBackfillOnStart(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newPushStore()
	st.Processed, st.BackfillUpto = 12, 12
	st.Pending = []string{bob}
	members := memMemberships{rooms: map[string][]core.RoomMembership{
		bob: {{RoomID: pushRoom, Membership: "join", LeaveStream: 2}},
	}}

	svc := core.NewPushActionsService(st, core.PushActionsOptions{Members: members})
	t.Cleanup(func() { _ = svc.Close(context.Background()) })

	svc.Signal()

	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if done, err := st.PushBackfilled(ctx, bob); err != nil || done {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("досчет при старте не выполнен")
		}
	}

	counts, err := st.PushCountsFor(ctx, bob, []string{pushRoom})
	if err != nil || counts[pushRoom][""].Notification != 2 || counts[pushRoom]["$e1"].Notification != 1 {
		t.Fatalf("досчет: %v %v", counts, err)
	}
}

// receipt не удаляет строки в пути запроса, а поднимает позицию
// прочтения сводки; прочитанные строки удаляет фоновая чистка, счетчики при
// этом не меняются. Тред и highlight считаются раздельно.
func TestPushActionsServiceSummaryPrune(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newPushStore()
	svc := core.NewPushActionsService(st, core.PushActionsOptions{PruneEvery: time.Hour})
	t.Cleanup(func() { _ = svc.Close(context.Background()) })

	if err := svc.CatchUp(ctx, 12); err != nil {
		t.Fatal(err)
	}

	if err := svc.ApplyReceipt(ctx, core.ReceiptEvent{RoomID: pushRoom, UserID: bob, EventID: "$e2", Type: core.ReceiptRead}); err != nil {
		t.Fatal(err)
	}

	want := map[string]core.PushCounts{"$e1": {Notification: 1}}

	counts, ok := svc.UnreadCounts(ctx, bob, 12, []string{pushRoom})
	if !ok || len(counts[pushRoom]) != 1 || counts[pushRoom]["$e1"] != want["$e1"] || len(st.Actions) != 3 {
		t.Fatalf("после receipt: %v, строк %d", counts, len(st.Actions))
	}

	if err := svc.PruneNow(ctx); err != nil {
		t.Fatal(err)
	}

	if len(st.Actions) != 1 || st.Actions[0].EventID != "$e3" {
		t.Fatalf("после чистки: %+v", st.Actions)
	}

	if counts, _ = svc.UnreadCounts(ctx, bob, 12, []string{pushRoom}); counts[pushRoom]["$e1"] != want["$e1"] {
		t.Fatalf("чистка изменила счетчики: %v", counts)
	}

	// Новое упоминание в треде - highlight треда, основная лента пуста.
	st.Events = append(st.Events, pushMsg("$e4", alice, 13,
		`{"msgtype":"m.text","body":"тред","m.mentions":{"user_ids":["`+bob+`"]},"m.relates_to":{"rel_type":"m.thread","event_id":"$e1"}}`))

	if err := svc.CatchUpNow(ctx); err != nil {
		t.Fatal(err)
	}

	if counts, _ = svc.UnreadCounts(ctx, bob, 13, []string{pushRoom}); len(counts[pushRoom]) != 1 ||
		counts[pushRoom]["$e1"] != (core.PushCounts{Notification: 2, Highlight: 1}) {
		t.Fatalf("тред с упоминанием: %v", counts)
	}

	// Receipt треда до $e3 оставляет только упоминание.
	if err := svc.ApplyReceipt(ctx, core.ReceiptEvent{RoomID: pushRoom, UserID: bob, EventID: "$e3", Type: core.ReceiptRead, ThreadID: "$e1"}); err != nil {
		t.Fatal(err)
	}

	if counts, _ = svc.UnreadCounts(ctx, bob, 13, []string{pushRoom}); counts[pushRoom]["$e1"] != (core.PushCounts{Notification: 1, Highlight: 1}) {
		t.Fatalf("receipt треда: %v", counts)
	}
}

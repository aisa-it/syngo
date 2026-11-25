package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aisa-it/syngo/internal/pushers"
	"github.com/aisa-it/syngo/internal/pushrules"
)

// Хранимые действия push-правил и счетчики непрочитанного, аналог
// event_push_actions Synapse: строка - событие, для которого правила получателя
// дали notify. Счетчики - сводка на пользователя, комнату и тред (аналог
// event_push_summary). Логика перенесена из HTTP-обработчиков.

// PushAction - уведомление о событии для получателя.
type PushAction struct {
	UserID    string
	RoomID    string
	EventID   string
	Stream    int64
	ThreadID  string
	Highlight bool
}

// PushReadUpTo - неявная отметка прочтения (свое событие): в комнате у
// пользователя прочитан тред ThreadID ("" - основная лента) до Stream
// включительно. Для выходов из комнат и фоновой чистки - только UserID и RoomID.
type PushReadUpTo struct {
	UserID   string
	RoomID   string
	ThreadID string
	Stream   int64
}

// PushRecipient - локальный участник комнаты и позиция его входа: события до
// входа ему не считаются.
type PushRecipient struct {
	UserID     string
	JoinStream int64
}

// PushReadMark - сохраненная отметка прочтения (m.read, m.read.private) с
// позицией события.
type PushReadMark struct {
	RoomID   string
	UserID   string
	ThreadID string
	Stream   int64
}

// PushBatch - результат разбора порции событий: новые действия, неявные
// отметки прочтения (свое событие), удаленные (redaction) события, выходы из
// комнат и новая позиция обработки. Записывается одним запросом.
type PushBatch struct {
	Actions     []PushAction
	ReadUpTo    []PushReadUpTo
	RedactedBy  []string
	Left        []PushReadUpTo
	Processed   int64
	BackfillFor string
}

// PushCounts - счетчики уведомлений одного треда.
type PushCounts struct {
	Notification int
	Highlight    int
}

// PushActionsStore - хранимые действия push-правил, позиция их разбора и данные
// для разбора (правила, состояние комнат, отметки прочтения, pushers).
type PushActionsStore interface {
	PushEvalStore
	ReadPositionStore

	// PushStreamState - позиция обработки, граница досчета старых событий и
	// последняя позиция событий.
	PushStreamState(ctx context.Context) (processed, backfillUpto, latest int64, err error)
	// ResetPushStream переносит позицию обработки и границу досчета на position.
	ResetPushStream(ctx context.Context, position int64) error
	// PushEventsAfter - события с позицией в (after, upper], не больше limit.
	PushEventsAfter(ctx context.Context, after, upper int64, limit int) ([]Event, error)
	// PushRecipients - локальные участники комнат с позицией входа.
	PushRecipients(ctx context.Context, roomIDs []string) (map[string][]PushRecipient, error)
	// PushReadMarks - сохраненные отметки прочтения пользователей в комнатах.
	PushReadMarks(ctx context.Context, roomIDs, userIDs []string) ([]PushReadMark, error)
	// ApplyPushBatch записывает результат разбора порции одним запросом.
	ApplyPushBatch(ctx context.Context, b PushBatch) error
	// MarkPushRead поднимает позицию прочтения сводки до события eventID и
	// пересчитывает счетчики выше нее; force - пересчет и для уже прочитанных.
	MarkPushRead(ctx context.Context, userID, roomID, threadID, eventID string, unthreaded, force bool) error
	// PrunePushActions удаляет прочитанные строки в комнатах пользователей.
	PrunePushActions(ctx context.Context, keys []PushReadUpTo) error
	// PushBackfilled - досчитаны ли старые события пользователя.
	PushBackfilled(ctx context.Context, userID string) (bool, error)
	// PushCountsFor - счетчики комнат по тредам ("" - основная лента) из сводки.
	PushCountsFor(ctx context.Context, userID string, roomIDs []string) (map[string]map[string]PushCounts, error)
	// PushUnreadRooms - число комнат с непрочитанными уведомлениями.
	PushUnreadRooms(ctx context.Context, userID string) (int, error)
	// UnreadCandidates - последние perRoom событий комнат с позицией в (low, upper].
	UnreadCandidates(ctx context.Context, low map[string]int64, upper int64, perRoom int) (map[string][]Event, error)
	// SyncPosition - безопасная позиция потока: все события до нее видны.
	SyncPosition(ctx context.Context) (int64, error)
	// PushBackfillPending - пользователи с комнатами без отметки досчета старых
	// событий (досчет при старте).
	PushBackfillPending(ctx context.Context) ([]string, error)
	// RoomPushers - pushers участников комнаты, кроме exceptUser, и extra.
	RoomPushers(ctx context.Context, roomID, exceptUser string, extra ...string) ([]pushers.Pusher, error)
}

// PushActionsOptions - зависимости сервиса хранимых действий.
type PushActionsOptions struct {
	// Members - комнаты пользователя (досчет старых событий).
	Members MembershipStore
	// Ephemeral - receipts в памяти процесса (может быть nil).
	Ephemeral *EphemeralStore
	// Lock - замок пользователя (досчет и receipts); nil - общий мьютекс.
	Lock func(userID string) (unlock func())
	// Warn - сбои, после которых работа продолжается (nil - без журнала).
	Warn func(ctx context.Context, msg string, err error, args ...any)
	// SyncWait - сколько /sync ждет фонового разбора до своей позиции и досчета
	// пользователя (0 - pushActionsSyncWait). Дальше счетчики отдаются по уже
	// разобранным событиям, следующий /sync их поправит.
	SyncWait time.Duration
	// PruneEvery - как часто фон удаляет прочитанные строки (0 -
	// pushActionsPruneEvery).
	PruneEvery time.Duration
}

// pushActionsBatch - сколько событий разбирается за один проход.
const pushActionsBatch = 500

// pushActionsMaxBacklog - отставание обработки, после которого события не
// разбираются по одному, а считаются историей (досчет по пользователю).
const pushActionsMaxBacklog = 10000

// pushActionsSyncWait - ожидание разбора в /sync по умолчанию.
const pushActionsSyncWait = 200 * time.Millisecond

// pushActionsPruneEvery - период фоновой чистки прочитанных строк.
const pushActionsPruneEvery = 30 * time.Second

// pushActionsPruneBatch - сколько комнат пользователей чистится одним запросом.
const pushActionsPruneBatch = 1000

// pushActionsBadgeWait - ожидание разбора для бейджа push: рассылка идет в фоне
// и может подождать дольше /sync.
const pushActionsBadgeWait = 2 * time.Second

// PushActionsService - хранимые действия push-правил и счетчики непрочитанного.
//
// Раньше unread_notifications считались на каждом /sync: до 100 последних
// событий каждой комнаты с вычислением правил (initial sync 6,4 с у
// пользователя с 1000 комнат), а для incremental держался кэш в памяти.
// Теперь события разбираются один раз, по позиции потока: для
// каждого локального участника вычисляются правила, notify сохраняется строкой
// event_push_actions. Счетчики - сводка на пользователя, комнату и
// тред, как event_push_summary Synapse: порция увеличивает ее в той же
// транзакции, что вставляет строки, receipt и свое событие поднимают позицию
// прочтения и пересчитывают только строки выше нее, прочитанные строки
// удаляются фоновой чисткой (раньше - удалением в пути receipt, а /sync считал
// count(*) по всем непрочитанным строкам). /sync читает сводку одним запросом
// O(комнат).
//
// Разбор идет по stream_ordering до безопасной позиции (SyncPosition): все
// события до нее видны, и пропусков нет. Разбор - фоновый: пути
// записи событий только сигналят (Signal), один проход за раз забирает все
// накопившиеся события одной порцией. Раньше запись события и каждый /sync
// догоняли синхронно под общим мьютексом: под нагрузкой сотни запросов ждали
// друг друга, запись шла 20-30 с. /sync ждет разбора до своей верхней границы
// не дольше SyncWait и замок разбора не берет, receipts и досчет сериализуются
// только по пользователю. События, записанные в обход API (федерация, импорт),
// разбираются по сигналу первого /sync. syngod - один экземпляр на базу
// (ADR-0009).
type PushActionsService struct {
	store    PushActionsStore
	members  MembershipStore
	reads    *ReadStates
	lock     func(userID string) func()
	warn     func(ctx context.Context, msg string, err error, args ...any)
	syncWait atomic.Int64

	// bg - контекст фоновой работы (разбор, досчет), отменяется Close.
	bg   context.Context
	stop context.CancelFunc
	wg   sync.WaitGroup

	// loadMu - чтение позиции обработки из БД (один раз).
	loadMu sync.Mutex
	// runMu - один проход разбора за раз. Пути запросов его не берут.
	runMu sync.Mutex

	// mu - короткий замок состояния ниже, без обращений к БД.
	mu        sync.Mutex
	loaded    bool
	processed int64
	// backfillUpto - позиция на момент миграции 0039: действия для более старых
	// событий досчитываются по пользователю (в фоне при старте и по /sync).
	backfillUpto int64
	// progress закрывается после каждого сдвига позиции и каждого прохода.
	progress chan struct{}
	// passes и passErr - число завершенных фоновых проходов и ошибка последнего.
	passes  uint64
	passErr error
	// running - фоновый проход идет; dirty - после него нужен еще один.
	running, dirty, closed bool
	// backfillJobs - идущие досчеты пользователей.
	backfillJobs map[string]*pushBackfillJob

	// rmu - журнал receipts, пришедших во время разбора порции (ApplyReceipt).
	rmu      sync.Mutex
	rActive  bool
	rJournal []ReceiptEvent
	// rgate - receipts в работе держат чтение, порция берет запись на включение
	// журнала: receipt вне журнала завершен до чтения отметок порции.
	rgate sync.RWMutex

	// pruneKeys - комнаты пользователей с поднятой позицией прочтения: их
	// прочитанные строки удалит фоновая чистка (под mu).
	pruneKeys  map[PushReadUpTo]struct{}
	prunedAt   time.Time
	pruneEvery time.Duration
	pruning    bool

	// backfilled - пользователи, для которых досчет уже сделан (кэш таблицы
	// event_push_backfill).
	backfilled sync.Map
}

// pushBackfillJob - досчет пользователя в фоне: done закрывается по окончании.
type pushBackfillJob struct {
	done chan struct{}
}

// NewPushActionsService создает сервис поверх хранилища.
func NewPushActionsService(store PushActionsStore, opts PushActionsOptions) *PushActionsService {
	s := &PushActionsService{
		store: store, members: opts.Members, lock: opts.Lock, warn: opts.Warn,
		progress: make(chan struct{}), backfillJobs: map[string]*pushBackfillJob{},
		pruneKeys: map[PushReadUpTo]struct{}{}, pruneEvery: opts.PruneEvery,
	}
	if s.pruneEvery <= 0 {
		s.pruneEvery = pushActionsPruneEvery
	}
	s.bg, s.stop = context.WithCancel(context.Background())
	s.SetSyncWait(opts.SyncWait)

	if s.warn == nil {
		s.warn = func(context.Context, string, error, ...any) {}
	}

	if s.lock == nil {
		var mu sync.Mutex

		s.lock = func(string) func() {
			mu.Lock()

			return mu.Unlock
		}
	}

	s.reads = NewReadStates(store, opts.Ephemeral, func(ctx context.Context, msg string, err error) {
		s.warn(ctx, msg, err)
	})

	return s
}

// SetSyncWait задает ожидание разбора в /sync (0 - по умолчанию).
func (s *PushActionsService) SetSyncWait(d time.Duration) {
	if d <= 0 {
		d = pushActionsSyncWait
	}

	s.syncWait.Store(int64(d))
}

// ReadStates - отметки прочтения пользователя (общие с /notifications).
func (s *PushActionsService) ReadStates() *ReadStates {
	return s.reads
}

// Close останавливает фоновую работу и ждет ее завершения (или ctx). Signal
// после Close ничего не запускает.
func (s *PushActionsService) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.stop()

	done := make(chan struct{})

	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// spawnLocked запускает фоновую работу (под mu); false - сервис закрыт.
func (s *PushActionsService) spawnLocked(name string, fn func(ctx context.Context) error) bool {
	if s.closed {
		return false
	}

	s.wg.Add(1)

	go func() {
		defer s.wg.Done()

		err := func() (err error) {
			// Паника в фоне не должна ронять homeserver.
			defer func() {
				if rec := recover(); rec != nil {
					err = fmt.Errorf("%s: panic: %v", name, rec)
				}
			}()

			return fn(s.bg)
		}()
		if err != nil && s.bg.Err() == nil {
			s.warn(s.bg, name, err)
		}
	}()

	return true
}

// broadcastLocked будит ожидающих разбора (под mu).
func (s *PushActionsService) broadcastLocked() {
	close(s.progress)
	s.progress = make(chan struct{})
}

// load читает позицию обработки (один раз). Большое отставание на старте - это
// история, записанная в обход syngod (импорт из Synapse после миграции):
// разбирать ее по событию - часы в первом запросе и счетчики за всю историю.
// Такие события досчитываются по пользователю, как старые до миграции 0039.
func (s *PushActionsService) load(ctx context.Context) error {
	s.loadMu.Lock()
	defer s.loadMu.Unlock()

	s.mu.Lock()
	loaded := s.loaded
	s.mu.Unlock()

	if loaded {
		return nil
	}

	processed, upto, latest, err := s.store.PushStreamState(ctx)
	if err != nil {
		return err
	}

	if latest-processed > pushActionsMaxBacklog {
		s.warn(ctx, "push actions backlog treated as history", nil, "processed", processed, "latest", latest)

		if err := s.store.ResetPushStream(ctx, latest); err != nil {
			return err
		}

		processed, upto = latest, latest
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.processed, s.backfillUpto, s.loaded = processed, upto, true
	s.broadcastLocked()

	// Досчет старых событий - в фоне при старте, а не в первом /sync.
	if upto > 0 {
		s.spawnLocked("push actions backfill failed", s.backfillPending)
	}

	return nil
}

// CatchUp разбирает события до позиции upper синхронно (фоновый проход и
// тесты). upper должна быть безопасной (SyncPosition): события до нее уже видны.
func (s *PushActionsService) CatchUp(ctx context.Context, upper int64) error {
	s.runMu.Lock()
	defer s.runMu.Unlock()

	if err := s.load(ctx); err != nil {
		return err
	}

	for {
		s.mu.Lock()
		processed := s.processed
		s.mu.Unlock()

		if processed >= upper {
			return nil
		}

		events, err := s.store.PushEventsAfter(ctx, processed, upper, pushActionsBatch)
		if err != nil {
			return err
		}

		next := upper
		if len(events) == pushActionsBatch {
			next = events[len(events)-1].StreamOrdering
		}

		if len(events) > 0 {
			if err := s.applyEvents(ctx, events, next); err != nil {
				return err
			}
		}

		// Пустая порция позицию в БД не двигает: после рестарта тот же диапазон
		// просматривается заново одним запросом по индексу.
		s.mu.Lock()
		s.processed = max(s.processed, next)
		s.broadcastLocked()
		s.mu.Unlock()
	}
}

// applyEvents разбирает и записывает порцию. Receipt, сохраненный после чтения
// отметок прочтения порции, но до ее записи, пересчитал бы сводку раньше, чем
// появились строки: такие receipts попадают в журнал (ApplyReceipt) и
// применяются повторно (с безусловным пересчетом) после записи порции. Receipts,
// начатые до журнала, порция дожидается (rgate): их отметки она уже увидит.
func (s *PushActionsService) applyEvents(ctx context.Context, events []Event, next int64) error {
	s.rgate.Lock()
	s.rmu.Lock()
	s.rActive, s.rJournal = true, nil
	s.rmu.Unlock()
	s.rgate.Unlock()

	batch, err := s.pushBatch(ctx, events)
	if err == nil {
		batch.Processed = next
		err = s.store.ApplyPushBatch(ctx, batch)
	}

	s.rmu.Lock()
	late := s.rJournal
	s.rActive, s.rJournal = false, nil
	s.rmu.Unlock()

	if err != nil {
		return err
	}

	s.mu.Lock()
	for _, r := range batch.ReadUpTo {
		s.pruneKeys[PushReadUpTo{UserID: r.UserID, RoomID: r.RoomID}] = struct{}{}
	}
	s.mu.Unlock()

	for _, r := range late {
		if err := s.markRead(ctx, r, true); err != nil {
			s.warn(ctx, "push actions receipt reapply failed", err)
		}
	}

	return nil
}

// CatchUpNow догоняет до текущей безопасной позиции синхронно.
func (s *PushActionsService) CatchUpNow(ctx context.Context) error {
	upper, err := s.store.SyncPosition(ctx)
	if err != nil {
		return err
	}

	return s.CatchUp(ctx, upper)
}

// Signal - события записаны: разбор идет в фоне, вызывающий не ждет. Сигналы во
// время прохода сливаются в один следующий проход.
func (s *PushActionsService) Signal() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.dirty = true
	if s.running {
		return
	}

	s.running = s.spawnLocked("push actions failed", s.run)
}

// EventsWritten - события записаны в запросе ctx: позиция потока читается здесь
// же (один запрос, без замков; заодно снимаются незакоммиченные номера только что
// записанного события, и /sync не тратит на них запрос), разбор - в фоне.
func (s *PushActionsService) EventsWritten(ctx context.Context) {
	if _, err := s.store.SyncPosition(ctx); err != nil {
		s.warn(ctx, "push actions position failed", err)
	}

	s.Signal()
}

// run - фоновые проходы разбора, пока есть сигналы. Сбой завершает цикл (без
// повторов вхолостую при недоступной БД): следующий сигнал запустит заново.
func (s *PushActionsService) run(ctx context.Context) error {
	for {
		s.mu.Lock()
		if !s.dirty || s.closed {
			s.running = false
			s.mu.Unlock()

			return nil
		}

		s.dirty = false
		s.mu.Unlock()

		err := s.CatchUpNow(ctx)
		// Сбой пишется в журнал до пробуждения ожидающих: они видят его записанным.
		if err != nil && ctx.Err() == nil {
			s.warn(ctx, "push actions failed", err)
		}

		s.mu.Lock()
		s.passes++
		s.passErr = err
		s.broadcastLocked()

		if err != nil {
			s.running, s.dirty = false, false
		} else {
			s.schedulePruneLocked()
		}
		s.mu.Unlock()

		if err != nil {
			return nil //nolint:nilerr // сбой уже в журнале, ожидающие получают его через passErr
		}
	}
}

// schedulePruneLocked запускает фоновую чистку прочитанных строк (под mu): не
// чаще PruneEvery и отдельно от разбора, чтобы большая чистка не задерживала
// счетчики /sync.
func (s *PushActionsService) schedulePruneLocked() {
	if s.pruning || len(s.pruneKeys) == 0 || time.Since(s.prunedAt) < s.pruneEvery {
		return
	}

	s.pruning = s.spawnLocked("push actions prune failed", func(ctx context.Context) error {
		defer func() {
			s.mu.Lock()
			s.pruning = false
			s.mu.Unlock()
		}()

		return s.PruneNow(ctx)
	})
}

// PruneNow удаляет прочитанные строки накопленных к вызову комнат
// пользователей порциями по pushActionsPruneBatch. Сбой возвращает
// необработанные комнаты в набор.
func (s *PushActionsService) PruneNow(ctx context.Context) error {
	s.mu.Lock()
	s.prunedAt = time.Now()

	keys := make([]PushReadUpTo, 0, len(s.pruneKeys))
	for k := range s.pruneKeys {
		keys = append(keys, k)
	}

	s.pruneKeys = map[PushReadUpTo]struct{}{}
	s.mu.Unlock()

	for len(keys) > 0 {
		chunk := keys[:min(len(keys), pushActionsPruneBatch)]

		if err := s.store.PrunePushActions(ctx, chunk); err != nil {
			s.mu.Lock()
			for _, k := range keys {
				s.pruneKeys[k] = struct{}{}
			}
			s.mu.Unlock()

			return err
		}

		keys = keys[len(chunk):]
	}

	return nil
}

// awaitProcessed ждет, пока разбор дойдет до upper, не дольше wait (по
// истечении - без ошибки, счетчики по уже разобранному). Ошибка - сбой
// фонового прохода или отмена ctx.
func (s *PushActionsService) awaitProcessed(ctx context.Context, upper int64, wait time.Duration) error {
	s.mu.Lock()
	if s.loaded && s.processed >= upper {
		s.mu.Unlock()

		return nil
	}

	passes, ch := s.passes, s.progress
	s.mu.Unlock()

	s.Signal()

	timer := time.NewTimer(wait)
	defer timer.Stop()

	for {
		select {
		case <-ch:
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}

		s.mu.Lock()
		done := s.loaded && s.processed >= upper
		failed := s.passes != passes && s.passErr != nil
		err := s.passErr
		ch = s.progress
		s.mu.Unlock()

		if done {
			return nil
		}

		if failed {
			return err
		}
	}
}

// ApplyReceipt сбрасывает уведомления по отметке прочтения: позиция прочтения
// сводки поднимается, счетчики пересчитываются по строкам выше нее (один
// запрос по ключу, без удаления строк и без замка разбора). Receipt уже
// сохранен (receipts.event_stream): порция, читающая отметки после этого,
// прочитанное не создаст, а для порции, которая их уже прочитала, receipt
// пишется в журнал и применяется повторно после ее записи. Под замком
// пользователя: досчет старых событий не разойдется с этим receipt.
func (s *PushActionsService) ApplyReceipt(ctx context.Context, r ReceiptEvent) error {
	if r.Type != ReceiptRead && r.Type != ReceiptReadPrivate {
		return nil
	}

	unlockUser := s.lock(r.UserID)
	defer unlockUser()

	s.rgate.RLock()
	defer s.rgate.RUnlock()

	s.rmu.Lock()
	if s.rActive {
		s.rJournal = append(s.rJournal, r)
	}
	s.rmu.Unlock()

	s.mu.Lock()
	s.pruneKeys[PushReadUpTo{UserID: r.UserID, RoomID: r.RoomID}] = struct{}{}
	s.mu.Unlock()

	return s.markRead(ctx, r, false)
}

// markRead применяет receipt к сводке; force - безусловный пересчет.
func (s *PushActionsService) markRead(ctx context.Context, r ReceiptEvent, force bool) error {
	thread := r.ThreadID
	if thread == "main" {
		thread = ""
	}

	return s.store.MarkPushRead(ctx, r.UserID, r.RoomID, thread, r.EventID, r.ThreadID == "", force)
}

// EnsureBackfilled досчитывает действия для событий до миграции 0039 так, как
// счетчики считались раньше (последние UnreadScanLimit событий комнаты после
// отметки прочтения), - один раз на пользователя. Без этого после обновления
// все старые непрочитанные пропали бы из счетчиков. Синхронно; /sync запускает
// досчет в фоне (awaitBackfill).
func (s *PushActionsService) EnsureBackfilled(ctx context.Context, userID string) error {
	if err := s.load(ctx); err != nil {
		return err
	}

	s.mu.Lock()
	upto, processed := s.backfillUpto, s.processed
	s.mu.Unlock()

	if upto == 0 {
		return nil
	}

	if _, ok := s.backfilled.Load(userID); ok {
		return nil
	}

	unlockUser := s.lock(userID)
	defer unlockUser()

	if _, ok := s.backfilled.Load(userID); ok {
		return nil
	}

	done, err := s.store.PushBackfilled(ctx, userID)
	if err != nil {
		return err
	}

	if !done {
		actions, err := s.backfillActions(ctx, userID, upto, max(processed, upto))
		if err != nil {
			return err
		}

		if err := s.store.ApplyPushBatch(ctx, PushBatch{Actions: actions, BackfillFor: userID}); err != nil {
			return err
		}
	}

	s.backfilled.Store(userID, struct{}{})

	return nil
}

// backfillPending - досчет при старте: все пользователи без отметки досчета.
func (s *PushActionsService) backfillPending(ctx context.Context) error {
	users, err := s.store.PushBackfillPending(ctx)
	if err != nil {
		return err
	}

	for _, userID := range users {
		if err := ctx.Err(); err != nil {
			return err
		}

		if err := s.EnsureBackfilled(ctx, userID); err != nil {
			s.warn(ctx, "push actions backfill failed", err, "user_id", userID)
		}
	}

	return nil
}

// awaitBackfill запускает досчет пользователя в фоне (если еще не сделан) и
// ждет его не дольше wait. Сбой досчета пишется в журнал фоновой работой.
func (s *PushActionsService) awaitBackfill(ctx context.Context, userID string, wait time.Duration) {
	if _, ok := s.backfilled.Load(userID); ok {
		return
	}

	s.mu.Lock()
	if s.loaded && s.backfillUpto == 0 {
		s.mu.Unlock()

		return
	}

	job := s.backfillJobs[userID]
	if job == nil {
		job = &pushBackfillJob{done: make(chan struct{})}

		started := s.spawnLocked("push actions backfill failed", func(ctx context.Context) error {
			defer func() {
				s.mu.Lock()
				delete(s.backfillJobs, userID)
				s.mu.Unlock()
				close(job.done)
			}()

			// Сбой пишется в журнал до done: ожидающий видит его уже записанным.
			if err := s.EnsureBackfilled(ctx, userID); err != nil && ctx.Err() == nil {
				s.warn(ctx, "push actions backfill failed", err)
			}

			return nil
		})
		if !started {
			s.mu.Unlock()

			return
		}

		s.backfillJobs[userID] = job
	}
	s.mu.Unlock()

	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-job.done:
	case <-timer.C:
	case <-ctx.Done():
	}
}

// UnreadCounts - счетчики комнат roomIDs по тредам для ответа /sync, одним
// запросом к сводке. Разбор до upper (счетчик соответствует отданному timeline) и досчет
// пользователя идут в фоне, /sync ждет их не дольше SyncWait: под нагрузкой
// счетчик может отстать на события, которые разбираются прямо сейчас. ok=false -
// счетчики не отдаются (сбой, /sync не падает).
func (s *PushActionsService) UnreadCounts(
	ctx context.Context, userID string, upper int64, roomIDs []string,
) (counts map[string]map[string]PushCounts, ok bool) {
	wait := time.Duration(s.syncWait.Load())

	s.awaitBackfill(ctx, userID, wait)

	// Сбой прохода уже в журнале фонового разбора.
	if err := s.awaitProcessed(ctx, upper, wait); err != nil {
		return nil, false
	}

	stored, err := s.store.PushCountsFor(ctx, userID, roomIDs)
	if err != nil {
		s.warn(ctx, "unread counts failed", err)

		return nil, false
	}

	return stored, true
}

// UnreadRooms - бейдж push: число комнат с непрочитанными уведомлениями (как
// Synapse по умолчанию), из хранимых действий; разбор до текущей позиции
// ждется не дольше pushActionsBadgeWait.
func (s *PushActionsService) UnreadRooms(ctx context.Context, userID string) (int, error) {
	s.awaitBackfill(ctx, userID, pushActionsBadgeWait)

	upper, err := s.store.SyncPosition(ctx)
	if err != nil {
		return 0, err
	}

	if err := s.awaitProcessed(ctx, upper, pushActionsBadgeWait); err != nil {
		return 0, err
	}

	return s.store.PushUnreadRooms(ctx, userID)
}

// PushTarget - получатель push о событии: его pushers и действия правил.
type PushTarget struct {
	UserID  string
	Pushers []pushers.Pusher
	Result  pushrules.Result
}

// PushTargets - получатели push о событии ev, чьи правила дали notify, в
// порядке выборки pushers. Правила вычисляются сразу, на момент
// события, как в Synapse: иначе фоновая рассылка видела бы правила, измененные
// уже после события (снятый mute давал push за сообщение, пришедшее в
// заглушенную комнату). Приглашенный получает push о приглашении, хотя еще не
// участник. Сбой выборки pushers - без получателей и без ошибки,
// как прежде; ошибка - сбой вычисления правил.
func (s *PushActionsService) PushTargets(ctx context.Context, ev Event) ([]PushTarget, error) {
	list, err := s.store.RoomPushers(ctx, ev.RoomID, ev.Sender, pushInvitee(ev)...)
	if err != nil || len(list) == 0 {
		return nil, nil //nolint:nilerr // сбой выборки pushers не мешает записи события
	}

	byUser := map[string][]pushers.Pusher{}
	users := []string{}

	for i := range list {
		if _, seen := byUser[list[i].UserID]; !seen {
			users = append(users, list[i].UserID)
		}

		byUser[list[i].UserID] = append(byUser[list[i].UserID], list[i])
	}

	evaluator, err := NewPushEvaluator(ctx, s.store, users, []string{ev.RoomID})
	if err != nil {
		return nil, err
	}

	var targets []PushTarget

	for _, userID := range users {
		if res := evaluator.Evaluate(userID, ev); res.Notify {
			targets = append(targets, PushTarget{UserID: userID, Pushers: byUser[userID], Result: res})
		}
	}

	return targets, nil
}

// pushInvitee - приглашенный локальный пользователь (push о приглашении).
func pushInvitee(ev Event) []string {
	if ev.Type != "m.room.member" || ev.StateKey == nil || *ev.StateKey == "" {
		return nil
	}

	var content struct {
		Membership string `json:"membership"`
	}

	if json.Unmarshal(ev.Content, &content) != nil || content.Membership != "invite" {
		return nil
	}

	return []string{*ev.StateKey}
}

// pushBatch разбирает порцию событий: правила получателей, неявное прочтение
// своим событием, redaction и выходы из комнат.
func (s *PushActionsService) pushBatch(ctx context.Context, events []Event) (PushBatch, error) {
	var batch PushBatch

	roomSet := map[string]struct{}{}
	for i := range events {
		roomSet[events[i].RoomID] = struct{}{}
	}

	rooms := make([]string, 0, len(roomSet))
	for roomID := range roomSet {
		rooms = append(rooms, roomID)
	}

	recipients, err := s.store.PushRecipients(ctx, rooms)
	if err != nil {
		return batch, err
	}

	userSet := map[string]struct{}{}

	for _, list := range recipients {
		for _, r := range list {
			userSet[r.UserID] = struct{}{}
		}
	}

	users := make([]string, 0, len(userSet))
	for userID := range userSet {
		users = append(users, userID)
	}

	if len(users) == 0 {
		return buildPushBatch(events, recipients, nil, nil), nil
	}

	// Отметки прочтения: receipt мог прийти раньше, чем событие разобрано
	// (клиент увидел его через /messages), - прочитанное не считается.
	marks, err := s.store.PushReadMarks(ctx, rooms, users)
	if err != nil {
		return batch, err
	}

	evaluator, err := NewPushEvaluator(ctx, s.store, users, rooms)
	if err != nil {
		return batch, err
	}

	return buildPushBatch(events, recipients, marks, evaluator.Evaluate), nil
}

// readStatesFromMarks - отметки прочтения по ключу пользователь + комната.
func readStatesFromMarks(marks []PushReadMark) map[string]*ReadState {
	reads := map[string]*ReadState{}

	for _, m := range marks {
		key := m.UserID + "\x00" + m.RoomID

		st := reads[key]
		if st == nil {
			st = &ReadState{}
			reads[key] = st
		}

		switch m.ThreadID {
		case "":
			st.unthreaded = max(st.unthreaded, m.Stream)
		case "main":
			st.raise("", m.Stream)
		default:
			st.raise(m.ThreadID, m.Stream)
		}
	}

	return reads
}

// buildPushBatch - разбор порции без обращений к БД: evaluate - правила
// получателя (nil - получателей нет, только прочтение, redaction и выходы).
func buildPushBatch(
	events []Event, recipients map[string][]PushRecipient, marks []PushReadMark,
	evaluate func(userID string, ev Event) pushrules.Result,
) PushBatch {
	var batch PushBatch

	reads := readStatesFromMarks(marks)

	// pending - действия порции. Строки одного запроса не видят друг друга,
	// поэтому прочтение и выход внутри порции применяются здесь же, одним
	// проходом в конце: readUpTo - своя лента (пользователь, комната, тред),
	// leftAt - позиция выхода из комнаты.
	var pending []PushAction

	readUpTo := map[string]int64{}
	leftAt := map[string]int64{}

	for i := range events {
		ev := events[i]
		thread := ThreadRoot(ev.Content)

		// Свое событие - неявная отметка прочтения своей ленты (как и раньше).
		if isRecipient(recipients[ev.RoomID], ev.Sender) {
			batch.ReadUpTo = append(batch.ReadUpTo,
				PushReadUpTo{UserID: ev.Sender, RoomID: ev.RoomID, ThreadID: thread, Stream: ev.StreamOrdering})
			readUpTo[ev.Sender+"\x00"+ev.RoomID+"\x00"+thread] = ev.StreamOrdering
		}

		switch ev.Type {
		case "m.room.redaction":
			// Цель redaction уже помечена redacted_by: в БД строки удаляются по
			// ней, а в порции удаленное событие не дает действий (RedactedBy).
			batch.RedactedBy = append(batch.RedactedBy, ev.EventID)
		case "m.room.member":
			if left := leftMember(ev); left != "" {
				batch.Left = append(batch.Left, PushReadUpTo{UserID: left, RoomID: ev.RoomID})
				leftAt[left+"\x00"+ev.RoomID] = ev.StreamOrdering
			}
		}

		if evaluate == nil || ev.RedactedBy != "" {
			continue
		}

		for _, r := range recipients[ev.RoomID] {
			if r.UserID == ev.Sender || ev.StreamOrdering <= r.JoinStream {
				continue
			}

			if st := reads[r.UserID+"\x00"+ev.RoomID]; st != nil && st.IsRead(ev.StreamOrdering, thread) {
				continue
			}

			res := evaluate(r.UserID, ev)
			if !res.Notify {
				continue
			}

			pending = append(pending, PushAction{
				UserID: r.UserID, RoomID: ev.RoomID, EventID: ev.EventID, Stream: ev.StreamOrdering,
				ThreadID: thread, Highlight: res.Highlight,
			})
		}
	}

	if len(readUpTo) > 0 || len(leftAt) > 0 {
		kept := pending[:0]

		for _, a := range pending {
			if a.Stream <= readUpTo[a.UserID+"\x00"+a.RoomID+"\x00"+a.ThreadID] ||
				a.Stream <= leftAt[a.UserID+"\x00"+a.RoomID] {
				continue
			}

			kept = append(kept, a)
		}

		pending = kept
	}

	batch.Actions = pending

	return batch
}

func isRecipient(list []PushRecipient, userID string) bool {
	for _, r := range list {
		if r.UserID == userID {
			return true
		}
	}

	return false
}

// leftMember - пользователь, вышедший (leave, ban) событием членства, иначе "".
func leftMember(ev Event) string {
	if ev.StateKey == nil || *ev.StateKey == "" {
		return ""
	}

	var content struct {
		Membership string `json:"membership"`
	}

	if json.Unmarshal(ev.Content, &content) != nil {
		return ""
	}

	if content.Membership == "leave" || content.Membership == "ban" {
		return *ev.StateKey
	}

	return ""
}

// backfillActions - действия для событий до upto по прежнему алгоритму: в
// каждой join-комнате последние UnreadScanLimit событий после входа и отметки
// прочтения; свои события (до scanUpper) поднимают отметку своей ленты.
func (s *PushActionsService) backfillActions(ctx context.Context, userID string, upto, scanUpper int64) ([]PushAction, error) {
	memberships, err := s.members.MembershipRooms(ctx, userID)
	if err != nil {
		return nil, err
	}

	joined := map[string]int64{}

	for _, m := range memberships {
		if m.Membership == "join" && m.LeaveStream < upto {
			joined[m.RoomID] = m.LeaveStream
		}
	}

	if len(joined) == 0 {
		return nil, nil
	}

	roomIDs := make([]string, 0, len(joined))
	for roomID := range joined {
		roomIDs = append(roomIDs, roomID)
	}

	states := s.reads.For(ctx, userID, roomIDs)

	low := make(map[string]int64, len(joined))
	for roomID, joinedAt := range joined {
		low[roomID] = max(joinedAt, states[roomID].unthreaded)
	}

	candidates, err := s.store.UnreadCandidates(ctx, low, scanUpper, UnreadScanLimit)
	if err != nil {
		return nil, err
	}

	unread := map[string][]Event{}

	for roomID, events := range candidates {
		st := states[roomID]
		threads := make([]string, len(events))

		for i := range events {
			threads[i] = ThreadRoot(events[i].Content)
			if events[i].Sender == userID {
				st.raise(threads[i], events[i].StreamOrdering)
			}
		}

		for i := range events {
			ev := events[i]
			if ev.StreamOrdering > upto || ev.Sender == userID || ev.RedactedBy != "" ||
				st.IsRead(ev.StreamOrdering, threads[i]) {
				continue
			}

			unread[roomID] = append(unread[roomID], ev)
		}
	}

	if len(unread) == 0 {
		return nil, nil
	}

	rooms := make([]string, 0, len(unread))
	for roomID := range unread {
		rooms = append(rooms, roomID)
	}

	evaluator, err := NewPushEvaluator(ctx, s.store, []string{userID}, rooms)
	if err != nil {
		return nil, fmt.Errorf("push backfill: %w", err)
	}

	var actions []PushAction

	for roomID, list := range unread {
		for i := range list {
			res := evaluator.Evaluate(userID, list[i])
			if !res.Notify {
				continue
			}

			actions = append(actions, PushAction{
				UserID: userID, RoomID: roomID, EventID: list[i].EventID, Stream: list[i].StreamOrdering,
				ThreadID: ThreadRoot(list[i].Content), Highlight: res.Highlight,
			})
		}
	}

	return actions, nil
}

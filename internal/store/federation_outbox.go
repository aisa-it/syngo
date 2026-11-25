package store

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// OutboxItem — элемент исходящей очереди федерации.
type OutboxItem struct {
	ID          int64
	Destination string
	// EventID/RoomID - ссылка на событие: PDU собирается при отправке.
	// Пусто у элементов, поставленных готовым JSON (PDU/EDU).
	EventID        string
	RoomID         string
	StreamOrdering int64
	PDU            []byte
	EDU            []byte
	Attempts       int
	QueuedAt       time.Time
}

// OutboxClaim — параметры захвата пачки.
type OutboxClaim struct {
	Destination string
	BatchSize   int
	Now         time.Time
	// Lease - аренда пачки на время отправки: пока она не истекла, другой
	// инстанс пачку destination не берет.
	Lease time.Duration
	// NewTxnID - txn_id для новой пачки; повтор берет сохраненный.
	NewTxnID string
}

// OutboxBatch — захваченная пачка: элементы и txn_id транзакции.
type OutboxBatch struct {
	TxnID string
	Items []OutboxItem
	// Retry - повтор ранее отправленной пачки с тем же txn_id.
	Retry bool
}

// OutboxFailure — итог неудачной отправки пачки.
type OutboxFailure struct {
	IDs     []int64
	RetryAt time.Time
	// KeepTxn - получатель мог принять транзакцию (сеть, 5xx): повтор идет
	// той же пачкой с тем же txn_id. После 4xx txn_id снимается.
	KeepTxn bool
	// Isolate - пачка отвергнута целиком: элементы уходят по одному, чтобы
	// ядовитый не блокировал остальные.
	Isolate bool
	Error   string
}

// OutboxDead — элемент, уходящий в dead-letter, и причина.
type OutboxDead struct {
	ID     int64
	Reason string
}

// EnqueueOutbox кладёт готовый PDU в очередь для destination. PDU хранится текстом
// байт-в-байт и уходит в транзакцию без перекодирования; room_id
// из него - для purge комнаты.
func (db *DB) EnqueueOutbox(ctx context.Context, destination string, pdu []byte) error {
	const query = `
		INSERT INTO federation_outbox (destination, pdu_json, room_id) VALUES ($1, $2, $3)`

	var header struct {
		RoomID string `json:"room_id"`
	}

	_ = json.Unmarshal(pdu, &header)

	if _, err := db.pool.Exec(ctx, query, destination, string(pdu), nullText(header.RoomID)); err != nil {
		return fmt.Errorf("store: enqueue outbox %s: %w", destination, err)
	}

	return nil
}

// EnqueueEDU ставит EDU в outbox для каждого destination. EDU не
// привязаны к событию: элемент хранит готовый JSON ({"edu_type", "content"}) и
// уходит в "edus" ближайшей транзакции получателя. Получатели вне политики
// федерации (allow_hosts/deny_hosts) пропускаются.
func (db *DB) EnqueueEDU(ctx context.Context, destinations []string, edu []byte) error {
	allowed := make([]string, 0, len(destinations))

	for _, destination := range destinations {
		if destination != "" && db.outboxDestinationAllowed(destination) {
			allowed = append(allowed, destination)
		}
	}

	if len(allowed) == 0 {
		return nil
	}

	slices.Sort(allowed)
	allowed = slices.Compact(allowed)

	if _, err := db.pool.Exec(ctx, `
		INSERT INTO federation_outbox (destination, edu_json)
		SELECT d, $2::jsonb FROM unnest($1::text[]) AS d`, allowed, string(edu)); err != nil {
		return fmt.Errorf("store: enqueue edu: %w", err)
	}

	return nil
}

// OutboxDestinations возвращает destination'ы с готовыми к отправке элементами:
// без пачки в полете и вне backoff получателя (destinations), старейшие первыми.
//
// Раньше выбор шел по имени (ORDER BY destination LIMIT 20) без учета backoff:
// получатели в конце алфавита голодали, а недоступный пробовался на каждом тике.
func (db *DB) OutboxDestinations(ctx context.Context, now time.Time, limit int) ([]string, error) {
	const query = `
		SELECT o.destination
		FROM federation_outbox o
		LEFT JOIN destinations d ON d.destination = o.destination
		WHERE o.next_attempt_at <= $1
		  AND (d.retry_last_ts IS NULL OR d.retry_interval <= 0
		       OR d.retry_last_ts + d.retry_interval * interval '1 millisecond' <= $1)
		  AND NOT EXISTS (
		      SELECT 1 FROM federation_outbox l
		      WHERE l.destination = o.destination AND l.claimed_until > $1)
		GROUP BY o.destination
		ORDER BY min(o.id)
		LIMIT $2`

	rows, err := db.pool.Query(ctx, query, now, limit)
	if err != nil {
		return nil, fmt.Errorf("store: outbox destinations: %w", err)
	}

	defer rows.Close()

	var out []string

	for rows.Next() {
		var destination string
		if err := rows.Scan(&destination); err != nil {
			return nil, fmt.Errorf("store: scan destination: %w", err)
		}

		out = append(out, destination)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate destinations: %w", err)
	}

	return out, nil
}

// outboxItemColumns — колонки элемента в порядке scanOutboxItems.
const outboxItemColumns = `id, destination, coalesce(event_id, ''), coalesce(room_id, ''), stream_ordering,
	coalesce(pdu_json::text, ''), coalesce(edu_json::text, ''), attempts, queued_at`

// ClaimOutboxBatch захватывает пачку destination.
//
// Захват сериализуется advisory-локом destination и арендой claimed_until:
// пока пачка в полете, другой инстанс ее не берет (раньше обычный SELECT, и N
// инстансов слали одну пачку N раз). Пачка, получившая сетевую ошибку или
// 5xx, повторяется целиком с тем же txn_id. Новая пачка собирается из готовых
// элементов без txn_id; элемент из отвергнутой пачки (isolate) уходит один.
func (db *DB) ClaimOutboxBatch(ctx context.Context, claim OutboxClaim) (OutboxBatch, error) {
	if claim.BatchSize <= 0 {
		claim.BatchSize = 50
	}

	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return OutboxBatch{}, fmt.Errorf("store: claim outbox begin: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	var locked bool

	if err := tx.QueryRow(ctx,
		`SELECT pg_try_advisory_xact_lock(hashtextextended('syngo.outbox:' || $1, 0))`,
		claim.Destination).Scan(&locked); err != nil {
		return OutboxBatch{}, fmt.Errorf("store: claim outbox lock %s: %w", claim.Destination, err)
	}

	if !locked {
		return OutboxBatch{}, nil
	}

	var inFlight bool

	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM federation_outbox WHERE destination = $1 AND claimed_until > $2)`,
		claim.Destination, claim.Now).Scan(&inFlight); err != nil {
		return OutboxBatch{}, fmt.Errorf("store: claim outbox in flight %s: %w", claim.Destination, err)
	}

	if inFlight {
		return OutboxBatch{}, nil
	}

	batch, err := claimRetryBatch(ctx, tx, claim)
	if err != nil {
		return OutboxBatch{}, err
	}

	if batch.TxnID == "" {
		batch, err = claimNewBatch(ctx, tx, claim)
		if err != nil {
			return OutboxBatch{}, err
		}
	}

	if len(batch.Items) == 0 {
		return OutboxBatch{}, nil
	}

	if err := tx.Commit(ctx); err != nil {
		return OutboxBatch{}, fmt.Errorf("store: claim outbox commit: %w", err)
	}

	return batch, nil
}

// claimRetryBatch берет ранее отправленную пачку (txn_id сохранен), если ее
// время пришло. Пока она ждет backoff, новые элементы destination тоже ждут:
// транзакции destination идут по одной и по порядку.
func claimRetryBatch(ctx context.Context, tx pgx.Tx, claim OutboxClaim) (OutboxBatch, error) {
	var (
		txnID string
		ready bool
	)

	err := tx.QueryRow(ctx, `
		SELECT txn_id, bool_and(next_attempt_at <= $2)
		FROM federation_outbox
		WHERE destination = $1 AND txn_id IS NOT NULL
		GROUP BY txn_id
		ORDER BY min(id)
		LIMIT 1`, claim.Destination, claim.Now).Scan(&txnID, &ready)
	if err != nil {
		if isNoRows(err) {
			return OutboxBatch{}, nil
		}

		return OutboxBatch{}, fmt.Errorf("store: claim outbox retry %s: %w", claim.Destination, err)
	}

	if !ready {
		// Пачка есть, но в backoff: новую не собираем (TxnID заполнен).
		return OutboxBatch{TxnID: txnID}, nil
	}

	rows, err := tx.Query(ctx, `
		UPDATE federation_outbox SET claimed_until = $3
		WHERE destination = $1 AND txn_id = $2
		RETURNING `+outboxItemColumns,
		claim.Destination, txnID, claim.Now.Add(claim.Lease))
	if err != nil {
		return OutboxBatch{}, fmt.Errorf("store: claim outbox retry %s: %w", claim.Destination, err)
	}

	items, err := scanOutboxItems(rows)
	if err != nil {
		return OutboxBatch{}, err
	}

	return OutboxBatch{TxnID: txnID, Items: items, Retry: true}, nil
}

// claimNewBatch собирает новую пачку из готовых элементов без txn_id.
func claimNewBatch(ctx context.Context, tx pgx.Tx, claim OutboxClaim) (OutboxBatch, error) {
	var (
		firstID int64
		isolate bool
	)

	err := tx.QueryRow(ctx, `
		SELECT id, isolate FROM federation_outbox
		WHERE destination = $1 AND txn_id IS NULL AND next_attempt_at <= $2
		ORDER BY id
		LIMIT 1
		FOR UPDATE SKIP LOCKED`, claim.Destination, claim.Now).Scan(&firstID, &isolate)
	if err != nil {
		if isNoRows(err) {
			return OutboxBatch{}, nil
		}

		return OutboxBatch{}, fmt.Errorf("store: claim outbox %s: %w", claim.Destination, err)
	}

	limit := claim.BatchSize
	if isolate {
		limit = 1
	}

	rows, err := tx.Query(ctx, `
		WITH picked AS (
			SELECT id FROM federation_outbox
			WHERE destination = $1 AND txn_id IS NULL AND next_attempt_at <= $2
			  AND (id = $3 OR NOT isolate)
			ORDER BY id
			LIMIT $4
			FOR UPDATE SKIP LOCKED
		)
		UPDATE federation_outbox o SET txn_id = $5, claimed_until = $6
		FROM picked WHERE o.id = picked.id
		RETURNING `+qualifiedOutboxColumns("o"),
		claim.Destination, claim.Now, firstID, limit, claim.NewTxnID, claim.Now.Add(claim.Lease))
	if err != nil {
		return OutboxBatch{}, fmt.Errorf("store: claim outbox %s: %w", claim.Destination, err)
	}

	items, err := scanOutboxItems(rows)
	if err != nil {
		return OutboxBatch{}, err
	}

	return OutboxBatch{TxnID: claim.NewTxnID, Items: items}, nil
}

// qualifiedOutboxColumns — outboxItemColumns с псевдонимом таблицы.
func qualifiedOutboxColumns(alias string) string {
	return alias + `.id, ` + alias + `.destination, coalesce(` + alias + `.event_id, ''), coalesce(` +
		alias + `.room_id, ''), ` + alias + `.stream_ordering, coalesce(` + alias + `.pdu_json::text, ''), coalesce(` +
		alias + `.edu_json::text, ''), ` + alias + `.attempts, ` + alias + `.queued_at`
}

// scanOutboxItems читает элементы и упорядочивает по id (RETURNING порядок не держит).
func scanOutboxItems(rows pgx.Rows) ([]OutboxItem, error) {
	defer rows.Close()

	var items []OutboxItem

	for rows.Next() {
		var item OutboxItem

		if err := rows.Scan(&item.ID, &item.Destination, &item.EventID, &item.RoomID, &item.StreamOrdering,
			&item.PDU, &item.EDU, &item.Attempts, &item.QueuedAt); err != nil {
			return nil, fmt.Errorf("store: scan outbox item: %w", err)
		}

		items = append(items, item)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate outbox: %w", err)
	}

	slices.SortFunc(items, func(a, b OutboxItem) int {
		switch {
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		}

		return 0
	})

	return items, nil
}

// AckOutbox удаляет доставленные элементы и сдвигает
// destinations.last_successful_stream_ordering.
func (db *DB) AckOutbox(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}

	const query = `
		WITH done AS (
			DELETE FROM federation_outbox WHERE id = ANY($1)
			RETURNING destination, stream_ordering, event_id
		)
		INSERT INTO destinations (destination, last_successful_stream_ordering)
		SELECT destination, max(stream_ordering) FROM done
		WHERE event_id IS NOT NULL
		GROUP BY destination
		ON CONFLICT (destination) DO UPDATE SET last_successful_stream_ordering =
			greatest(coalesce(destinations.last_successful_stream_ordering, 0),
			         EXCLUDED.last_successful_stream_ordering)`

	if _, err := db.pool.Exec(ctx, query, ids); err != nil {
		return fmt.Errorf("store: ack outbox: %w", err)
	}

	return nil
}

// FailOutbox фиксирует неудачную попытку: attempts++, следующая попытка не
// раньше RetryAt, аренда снята.
func (db *DB) FailOutbox(ctx context.Context, failure OutboxFailure) error {
	if len(failure.IDs) == 0 {
		return nil
	}

	const query = `
		UPDATE federation_outbox
		SET attempts = attempts + 1, next_attempt_at = $2, claimed_until = NULL,
		    txn_id = CASE WHEN $3 THEN txn_id END,
		    isolate = isolate OR $4,
		    last_error = nullif($5, '')
		WHERE id = ANY($1)`

	if _, err := db.pool.Exec(ctx, query, failure.IDs, failure.RetryAt, failure.KeepTxn,
		failure.Isolate, failure.Error); err != nil {
		return fmt.Errorf("store: fail outbox: %w", err)
	}

	return nil
}

// ReleaseOutbox снимает аренду без попытки (получатель в backoff, отправки не было).
func (db *DB) ReleaseOutbox(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}

	if _, err := db.pool.Exec(ctx,
		`UPDATE federation_outbox SET claimed_until = NULL WHERE id = ANY($1)`, ids); err != nil {
		return fmt.Errorf("store: release outbox: %w", err)
	}

	return nil
}

// DeadLetterOutbox переносит элементы в federation_outbox_dead с причиной.
func (db *DB) DeadLetterOutbox(ctx context.Context, dead []OutboxDead) error {
	if len(dead) == 0 {
		return nil
	}

	ids := make([]int64, 0, len(dead))
	reasons := make([]string, 0, len(dead))

	for _, item := range dead {
		ids = append(ids, item.ID)
		reasons = append(reasons, item.Reason)
	}

	const query = `
		WITH reasons AS (
			SELECT * FROM unnest($1::bigint[], $2::text[]) AS r(id, reason)
		), moved AS (
			DELETE FROM federation_outbox o USING reasons r WHERE o.id = r.id
			RETURNING o.id, o.destination, o.event_id, o.room_id, o.pdu_json, o.edu_json,
			          o.attempts, o.queued_at, r.reason
		)
		INSERT INTO federation_outbox_dead (id, destination, event_id, room_id, pdu_json, edu_json,
		                                    attempts, queued_at, reason)
		SELECT id, destination, event_id, room_id, pdu_json, edu_json, attempts, queued_at, reason FROM moved
		ON CONFLICT (id) DO NOTHING`

	if _, err := db.pool.Exec(ctx, query, ids, reasons); err != nil {
		return fmt.Errorf("store: dead-letter outbox: %w", err)
	}

	return nil
}

// OutboxStats — метрики очереди: глубина, возраст самого старого элемента, повторы.
type OutboxStats struct {
	Depth    int64
	Oldest   time.Duration
	Attempts int64
	// DeadLetters - элементы в federation_outbox_dead (недоставляемые).
	DeadLetters int64
}

// OutboxMetrics возвращает статистику исходящей очереди.
func (db *DB) OutboxMetrics(ctx context.Context) (OutboxStats, error) {
	const query = `
		SELECT count(*), coalesce(sum(attempts), 0),
		       coalesce(extract(epoch FROM (now() - min(queued_at))), 0)::bigint,
		       (SELECT count(*) FROM federation_outbox_dead)
		FROM federation_outbox`

	var (
		stats      OutboxStats
		oldestSecs int64
	)

	if err := db.pool.QueryRow(ctx, query).Scan(&stats.Depth, &stats.Attempts, &oldestSecs,
		&stats.DeadLetters); err != nil {
		return OutboxStats{}, fmt.Errorf("store: outbox metrics: %w", err)
	}

	stats.Oldest = time.Duration(oldestSecs) * time.Second

	return stats, nil
}

// SetOutboxDestinationFilter задает политику федерации для постановки в outbox:
// allowed(destination)=false - событие этому серверу не ставится (allow_hosts,
// deny_hosts). nil - без ограничений.
func (db *DB) SetOutboxDestinationFilter(allowed func(destination string) bool) {
	if allowed == nil {
		db.outboxAllowed.Store(nil)

		return
	}

	db.outboxAllowed.Store(&allowed)
}

// DisableFederationOutbox - федерация выключена (federation.enabled: false):
// запись локального события не читает серверы комнаты и не ставит outbox и
// destination_rooms. Доставлять их некому - воркер outbox без
// федерации не запускается, - а работа шла под блокировкой комнаты.
func (db *DB) DisableFederationOutbox() {
	if db != nil {
		db.noFederationOutbox.Store(true)
	}
}

// outboxDestinationAllowed проверяет политику федерации.
func (db *DB) outboxDestinationAllowed(destination string) bool {
	allowed := db.outboxAllowed.Load()

	return allowed == nil || (*allowed)(destination)
}

// joinedHostsQuery — серверы с joined-участниками комнаты по текущему состоянию.
// room_state, а не room_memberships: членство удаленных пользователей из /send
// материализуется только в room_state.
const joinedHostsQuery = `
	SELECT DISTINCT substr(rs.state_key, strpos(rs.state_key, ':') + 1)
	FROM room_state rs
	JOIN events e ON e.event_id = rs.event_id
	WHERE rs.room_id = $1 AND rs.type = 'm.room.member'
	  AND strpos(rs.state_key, ':') > 0
	  AND e.content->>'membership' = 'join'`

// joinedHosts — серверы с joined-участниками комнаты (в транзакции).
func joinedHosts(ctx context.Context, tx pgx.Tx, roomID string) ([]string, error) {
	rows, err := tx.Query(ctx, joinedHostsQuery, roomID)
	if err != nil {
		return nil, fmt.Errorf("store: joined hosts %s: %w", roomID, err)
	}

	defer rows.Close()

	var hosts []string

	for rows.Next() {
		var host string
		if err := rows.Scan(&host); err != nil {
			return nil, fmt.Errorf("store: scan joined host: %w", err)
		}

		hosts = append(hosts, host)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate joined hosts: %w", err)
	}

	slices.Sort(hosts)

	return hosts, nil
}

// DestinationsForRoom возвращает серверы, которым уходят события комнаты: с
// joined-участниками по текущему состоянию, без своего. Раньше -
// destination_rooms, куда серверы только добавлялись: вышедшие, кикнутые и
// приглашенные получали все события комнаты.
func (db *DB) DestinationsForRoom(ctx context.Context, roomID, ownServer string) ([]string, error) {
	rows, err := db.pool.Query(ctx, joinedHostsQuery, roomID)
	if err != nil {
		return nil, fmt.Errorf("store: destinations for room: %w", err)
	}

	defer rows.Close()

	var out []string

	for rows.Next() {
		var destination string
		if err := rows.Scan(&destination); err != nil {
			return nil, fmt.Errorf("store: scan destination: %w", err)
		}

		if destination != ownServer {
			out = append(out, destination)
		}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate destinations: %w", err)
	}

	slices.Sort(out)

	return out, nil
}

// outboxEvent — событие для постановки в outbox.
type outboxEvent struct {
	EventID        string
	RoomID         string
	StreamOrdering int64
	// Own - свой сервер; OnBehalfOf - сервер, у которого событие уже есть и
	// которому оно не рассылается: источник send_join/send_leave (ретрансляция от
	// его имени) или сервер приглашенного (получил invite через /invite).
	Own        string
	OnBehalfOf string
}

// enqueueEventTx ставит событие в outbox в транзакции его записи:
// событие и постановка коммитятся вместе, обрыв клиента после записи не теряет
// рассылку. Получатели - серверы с joined-участниками ДО события (before, как
// get_hosts_in_room_at_events Synapse): кикнутый сервер получает свой kick,
// приглашенный - только invite (через /invite), а не события до входа.
// destination_rooms приводится к серверам с участниками ПОСЛЕ события: ушедший
// последний участник сервера удаляет строку.
func (db *DB) enqueueEventTx(ctx context.Context, tx pgx.Tx, ev outboxEvent, before, after []string) error {
	destinations := make([]string, 0, len(before))

	for _, host := range before {
		if host != ev.Own && host != ev.OnBehalfOf && db.outboxDestinationAllowed(host) {
			destinations = append(destinations, host)
		}
	}

	if len(destinations) > 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO federation_outbox (destination, event_id, room_id, stream_ordering)
			SELECT d, $2, $3, $4 FROM unnest($1::text[]) AS d`,
			destinations, ev.EventID, ev.RoomID, ev.StreamOrdering); err != nil {
			return fmt.Errorf("store: enqueue event %s: %w", ev.EventID, err)
		}
	}

	keep := make([]string, 0, len(after))

	for _, host := range after {
		if host != ev.Own && db.outboxDestinationAllowed(host) {
			keep = append(keep, host)
		}
	}

	if _, err := tx.Exec(ctx, `
		WITH gone AS (
			DELETE FROM destination_rooms WHERE room_id = $1 AND NOT (destination = ANY($2::text[]))
		)
		INSERT INTO destination_rooms (destination, room_id, stream_ordering)
		SELECT d, $1, $3 FROM unnest($2::text[]) AS d
		ON CONFLICT (destination, room_id) DO UPDATE SET stream_ordering = EXCLUDED.stream_ordering`,
		ev.RoomID, keep, ev.StreamOrdering); err != nil {
		return fmt.Errorf("store: destination rooms %s: %w", ev.RoomID, err)
	}

	return nil
}

// RelayMembershipTx ретранслирует принятое через send_join/send_leave событие
// остальным серверам комнаты (как send_on_behalf_of в Synapse):
// в транзакции записи события, после применения состояния; сервер-источник
// событие уже имеет и не получает его.
func (db *DB) RelayMembershipTx(ctx context.Context, tx pgx.Tx, roomID, eventID, ownServer, origin string) error {
	var stream int64

	if err := tx.QueryRow(ctx, `SELECT stream_ordering FROM events WHERE event_id = $1`, eventID).Scan(&stream); err != nil {
		return fmt.Errorf("store: relay membership %s: %w", eventID, err)
	}

	hosts, err := joinedHosts(ctx, tx, roomID)
	if err != nil {
		return err
	}

	// До и после события серверы отличаются только источником (его участник
	// вошел или вышел), а источнику событие не шлется.
	return db.enqueueEventTx(ctx, tx, outboxEvent{
		EventID: eventID, RoomID: roomID, StreamOrdering: stream, Own: ownServer, OnBehalfOf: origin,
	}, hosts, hosts)
}

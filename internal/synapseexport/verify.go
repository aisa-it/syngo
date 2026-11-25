package synapseexport

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// VerifyOptions - параметры сверки.
type VerifyOptions struct {
	// MaxDiffs - сколько примеров расхождений сохранять по каждой проверке.
	MaxDiffs int
	// ServerName - сверять членство, пользователей и медиа только этого сервера
	// (syngo-import -server-name пропускает чужих); пусто - всех.
	ServerName string
	// SkipMedia - не сверять медиа (медиа еще не импортировано).
	SkipMedia bool
}

// Totals - число и границы stream_ordering событий одной стороны.
type Totals struct {
	Events    int64
	MinStream int64
	MaxStream int64
}

// VerifyReport - итоги сверки импорта с Synapse.
type VerifyReport struct {
	Source Totals
	Target Totals
	// SequenceLast - last_value events_stream_ordering_seq цели: должен быть не
	// меньше max(stream_ordering), иначе новые события пойдут "в прошлое".
	SequenceLast int64

	RoomsChecked   int64
	RoomCountDiffs int64
	MissingEvents  int64 // есть в Synapse, нет у нас
	ExtraEvents    int64 // есть у нас, нет в Synapse
	// ClassDiffs - событие у нас не в том разделе: отклоненное, soft-failed или
	// outlier в timeline либо обычное вне его.
	ClassDiffs int64
	// StateRows, StateDiffs - строк current_state_events и расхождений с room_state.
	StateRows  int64
	StateDiffs int64
	// Redacted, RedactionDiffs - удаленных целей в Synapse и расхождений.
	Redacted       int64
	RedactionDiffs int64
	// Memberships, MembershipDiffs - текущее членство (room_memberships).
	Memberships     int64
	MembershipDiffs int64
	// Users, UserDiffs, Aliases, AliasDiffs - пользователи и алиасы.
	Users      int64
	UserDiffs  int64
	Aliases    int64
	AliasDiffs int64
	// MediaChecked - в источнике есть local_media_repository; Media, MediaDiffs -
	// медиа без превью ссылок и расхождения (размер, карантин, sha256);
	// MediaURLCache - пропущенных превью.
	MediaChecked  bool
	Media         int64
	MediaDiffs    int64
	MediaURLCache int64

	// Problems - расхождения с примерами; пусто - сверка прошла.
	Problems []string
}

// OK - расхождений нет.
func (r VerifyReport) OK() bool { return len(r.Problems) == 0 }

// Text - отчет для оператора миграции.
func (r VerifyReport) Text() string {
	var b strings.Builder

	fmt.Fprintf(&b, "migration verify report\n")
	fmt.Fprintf(&b, "events: synapse=%d syngo=%d\n", r.Source.Events, r.Target.Events)
	fmt.Fprintf(&b, "stream_ordering: synapse=[%d..%d] syngo=[%d..%d] sequence=%d\n",
		r.Source.MinStream, r.Source.MaxStream, r.Target.MinStream, r.Target.MaxStream, r.SequenceLast)
	fmt.Fprintf(&b, "rooms: checked=%d count_diffs=%d\n", r.RoomsChecked, r.RoomCountDiffs)
	fmt.Fprintf(&b, "event_id: missing=%d extra=%d class_diffs=%d\n", r.MissingEvents, r.ExtraEvents, r.ClassDiffs)
	fmt.Fprintf(&b, "state: rows=%d diffs=%d\n", r.StateRows, r.StateDiffs)
	fmt.Fprintf(&b, "redactions: redacted=%d diffs=%d\n", r.Redacted, r.RedactionDiffs)
	fmt.Fprintf(&b, "memberships: rows=%d diffs=%d\n", r.Memberships, r.MembershipDiffs)
	fmt.Fprintf(&b, "users: rows=%d diffs=%d; aliases: rows=%d diffs=%d\n", r.Users, r.UserDiffs, r.Aliases, r.AliasDiffs)

	if r.MediaChecked {
		fmt.Fprintf(&b, "media: rows=%d diffs=%d url_cache_skipped=%d\n", r.Media, r.MediaDiffs, r.MediaURLCache)
	} else {
		fmt.Fprintf(&b, "media: not checked\n")
	}

	if r.OK() {
		fmt.Fprintf(&b, "result: OK\n")

		return b.String()
	}

	fmt.Fprintf(&b, "result: MISMATCH (%d)\n", len(r.Problems))

	for _, p := range r.Problems {
		fmt.Fprintf(&b, "problem: %s\n", p)
	}

	return b.String()
}

// Целевые запросы сверки: события synapse-go в том же виде, что у Synapse -
// timeline (events) вместе с отклоненными, soft-failed и outlier (excluded_events).
const (
	targetAll = `(SELECT event_id, room_id, stream_ordering, 'timeline' AS class FROM events
		UNION ALL SELECT event_id, room_id, stream_ordering, reason FROM excluded_events)`
	targetTotals = `
		SELECT count(*), coalesce(min(stream_ordering), 0), coalesce(max(stream_ordering), 0) FROM ` + targetAll + ` t`
	targetRoomCounts = `
		SELECT room_id, count(*)::text FROM ` + targetAll + ` t GROUP BY room_id ORDER BY room_id COLLATE "C"`
	targetEventClasses = `
		SELECT event_id, class FROM ` + targetAll + ` t ORDER BY event_id COLLATE "C"`
	targetState = `
		SELECT room_id || chr(1) || type || chr(1) || state_key, event_id FROM room_state
		ORDER BY (room_id || chr(1) || type || chr(1) || state_key) COLLATE "C"`
	targetRedacted = `
		SELECT event_id, '' FROM events WHERE redacted_by IS NOT NULL ORDER BY event_id COLLATE "C"`
	targetMemberships = `
		SELECT room_id || chr(1) || user_id, membership FROM room_memberships
		WHERE $1 = '' OR user_id LIKE '%:' || $1
		ORDER BY (room_id || chr(1) || user_id) COLLATE "C"`
)

// softFailedExpr - признак soft-failed из internal_metadata (текст JSON, без
// приведения к jsonb).
const softFailedExpr = `coalesce(j.internal_metadata ~ '"soft_failed"\s*:\s*true', false)`

// Verify сверяет импорт (dst) с БД Synapse (src): число и границы
// stream_ordering, число событий по комнатам, множества event_id и раздел
// каждого события (timeline или отклоненное/soft-failed/outlier), текущее
// состояние комнат с current_state_events, удаленные redaction события,
// текущее членство, sequence цели. Наборы сравниваются слиянием
// отсортированных потоков - память не растет с числом событий.
func Verify(ctx context.Context, src, dst Querier, opts VerifyOptions) (VerifyReport, error) {
	if opts.MaxDiffs <= 0 {
		opts.MaxDiffs = 20
	}

	var report VerifyReport

	if err := src.QueryRow(ctx, `
		SELECT count(*), coalesce(min(stream_ordering), 0), coalesce(max(stream_ordering), 0) FROM events`).
		Scan(&report.Source.Events, &report.Source.MinStream, &report.Source.MaxStream); err != nil {
		return report, fmt.Errorf("synapseexport: события Synapse: %w", err)
	}

	if err := dst.QueryRow(ctx, targetTotals).
		Scan(&report.Target.Events, &report.Target.MinStream, &report.Target.MaxStream); err != nil {
		return report, fmt.Errorf("synapseexport: события synapse-go: %w", err)
	}

	if err := dst.QueryRow(ctx, `SELECT last_value FROM events_stream_ordering_seq`).Scan(&report.SequenceLast); err != nil {
		return report, fmt.Errorf("synapseexport: sequence: %w", err)
	}

	if report.Source.Events != report.Target.Events {
		report.problem("число событий: synapse=%d syngo=%d", report.Source.Events, report.Target.Events)
	}

	if report.Source.MaxStream != report.Target.MaxStream || report.Source.MinStream != report.Target.MinStream {
		report.problem("границы stream_ordering: synapse=[%d..%d] syngo=[%d..%d]",
			report.Source.MinStream, report.Source.MaxStream, report.Target.MinStream, report.Target.MaxStream)
	}

	if report.SequenceLast < report.Target.MaxStream {
		report.problem("events_stream_ordering_seq=%d меньше max(stream_ordering)=%d: новые события не попадут в инкрементальный /sync",
			report.SequenceLast, report.Target.MaxStream)
	}

	checks := []func(context.Context, Querier, Querier, VerifyOptions) error{
		report.verifyRooms, report.verifyEvents, report.verifyState, report.verifyRedactions, report.verifyMemberships,
		report.verifyUsers,
	}

	if !opts.SkipMedia {
		checks = append(checks, report.verifyMedia)
	}

	for _, check := range checks {
		if err := check(ctx, src, dst, opts); err != nil {
			return report, err
		}
	}

	return report, nil
}

// samples - ограниченный список примеров расхождений.
type samples struct {
	limit int
	items []string
}

func (s *samples) add(item string) {
	if len(s.items) < s.limit {
		s.items = append(s.items, item)
	}
}

func (s *samples) String() string { return strings.Join(s.items, "; ") }

func (r *VerifyReport) verifyRooms(ctx context.Context, src, dst Querier, opts VerifyOptions) error {
	diffs := samples{limit: opts.MaxDiffs}

	err := mergeKeyed(ctx, src, dst,
		query{sql: `SELECT room_id, count(*)::text FROM events GROUP BY room_id ORDER BY room_id COLLATE "C"`},
		query{sql: targetRoomCounts},
		func(key, srcVal, dstVal string) {
			r.RoomsChecked++

			if srcVal != dstVal {
				r.RoomCountDiffs++
				diffs.add(fmt.Sprintf("%s: synapse=%s syngo=%s", key, orDash(srcVal), orDash(dstVal)))
			}
		})
	if err != nil {
		return err
	}

	if r.RoomCountDiffs > 0 {
		r.problem("событий по комнатам расходится в %d комнатах: %s", r.RoomCountDiffs, diffs.String())
	}

	return nil
}

func (r *VerifyReport) verifyEvents(ctx context.Context, src, dst Querier, opts VerifyOptions) error {
	reason, err := rejectionExpr(ctx, src)
	if err != nil {
		return err
	}

	sourceClasses := `
		SELECT e.event_id,
		       CASE WHEN ` + reason + ` IS NOT NULL THEN 'rejected'
		            WHEN ` + softFailedExpr + ` THEN 'soft_failed'
		            WHEN e.outlier THEN 'outlier'
		            ELSE 'timeline' END
		FROM events e
		LEFT JOIN event_json j ON j.event_id = e.event_id
		LEFT JOIN rejections rj ON rj.event_id = e.event_id
		ORDER BY e.event_id COLLATE "C"`

	missing, extra, classes := samples{limit: opts.MaxDiffs}, samples{limit: opts.MaxDiffs}, samples{limit: opts.MaxDiffs}

	err = mergeKeyed(ctx, src, dst, query{sql: sourceClasses}, query{sql: targetEventClasses},
		func(key, srcVal, dstVal string) {
			switch {
			case srcVal == absent:
				r.ExtraEvents++
				extra.add(key)
			case dstVal == absent:
				r.MissingEvents++
				missing.add(key)
			case srcVal != dstVal:
				r.ClassDiffs++
				classes.add(fmt.Sprintf("%s: synapse=%s syngo=%s", key, srcVal, dstVal))
			}
		})
	if err != nil {
		return err
	}

	if r.MissingEvents > 0 {
		r.problem("нет у нас %d событий Synapse, например: %s", r.MissingEvents, missing.String())
	}

	if r.ExtraEvents > 0 {
		r.problem("у нас %d событий, которых нет в Synapse, например: %s", r.ExtraEvents, extra.String())
	}

	if r.ClassDiffs > 0 {
		r.problem("%d событий не в том разделе (timeline/отклоненные): %s", r.ClassDiffs, classes.String())
	}

	return nil
}

func (r *VerifyReport) verifyState(ctx context.Context, src, dst Querier, opts VerifyOptions) error {
	diffs := samples{limit: opts.MaxDiffs}

	err := mergeKeyed(ctx, src, dst,
		query{sql: `
			SELECT room_id || chr(1) || type || chr(1) || state_key, event_id FROM current_state_events
			ORDER BY (room_id || chr(1) || type || chr(1) || state_key) COLLATE "C"`},
		query{sql: targetState},
		func(key, srcVal, dstVal string) {
			if srcVal != absent {
				r.StateRows++
			}

			if srcVal != dstVal {
				r.StateDiffs++
				diffs.add(fmt.Sprintf("%s: synapse=%s syngo=%s", strings.ReplaceAll(key, "\x01", " "), orDash(srcVal), orDash(dstVal)))
			}
		})
	if err != nil {
		return err
	}

	if r.StateDiffs > 0 {
		r.problem("текущее состояние расходится в %d строках (room type state_key): %s", r.StateDiffs, diffs.String())
	}

	return nil
}

// verifyRedactions сверяет удаленные события: в Synapse redaction действует,
// если она и цель в timeline одной комнаты и (не требует перепроверки или
// домены отправителей совпадают) - как _maybe_redact_event_row.
func (r *VerifyReport) verifyRedactions(ctx context.Context, src, dst Querier, opts VerifyOptions) error {
	hasRejectionColumn, err := hasColumn(ctx, src, "events", "rejection_reason")
	if err != nil {
		return err
	}

	hasRecheck, err := hasColumn(ctx, src, "redactions", "recheck")
	if err != nil {
		return err
	}

	recheck := `coalesce(rj_meta.internal_metadata ~ '"recheck_redaction"\s*:\s*true', false)`
	if hasRecheck {
		recheck = "rd.recheck"
	}

	// В timeline: не outlier, не отклонено, не soft-failed.
	timeline := func(alias string) string {
		cond := `NOT ` + alias + `.outlier
			AND NOT EXISTS (SELECT 1 FROM rejections x WHERE x.event_id = ` + alias + `.event_id)
			AND NOT EXISTS (SELECT 1 FROM event_json j WHERE j.event_id = ` + alias + `.event_id AND ` + softFailedExpr + `)`
		if hasRejectionColumn {
			cond += ` AND ` + alias + `.rejection_reason IS NULL`
		}

		return cond
	}

	sourceRedacted := `
		SELECT t.event_id, ''
		FROM redactions rd
		JOIN events e ON e.event_id = rd.event_id
		JOIN events t ON t.event_id = rd.redacts AND t.room_id = e.room_id
		LEFT JOIN event_json rj_meta ON rj_meta.event_id = e.event_id
		WHERE ` + timeline("e") + ` AND ` + timeline("t") + `
		  AND (NOT ` + recheck + ` OR substr(e.sender, strpos(e.sender, ':') + 1) = substr(t.sender, strpos(t.sender, ':') + 1))
		GROUP BY t.event_id
		ORDER BY t.event_id COLLATE "C"`

	diffs := samples{limit: opts.MaxDiffs}

	err = mergeKeyed(ctx, src, dst, query{sql: sourceRedacted}, query{sql: targetRedacted},
		func(key, srcVal, dstVal string) {
			if srcVal != absent {
				r.Redacted++
			}

			if srcVal != dstVal {
				r.RedactionDiffs++

				state := "удалено в Synapse, у нас нет"
				if srcVal == absent {
					state = "удалено у нас, в Synapse нет"
				}

				diffs.add(key + ": " + state)
			}
		})
	if err != nil {
		return err
	}

	if r.RedactionDiffs > 0 {
		r.problem("redaction расходятся у %d событий: %s", r.RedactionDiffs, diffs.String())
	}

	return nil
}

// verifyMemberships сверяет текущее членство (шаг 3.5 runbook) с
// current_state_events: раньше проверка шла до импорта членства и всегда
// давала 0 расхождений.
func (r *VerifyReport) verifyMemberships(ctx context.Context, src, dst Querier, opts VerifyOptions) error {
	diffs := samples{limit: opts.MaxDiffs}

	err := mergeKeyed(ctx, src, dst,
		query{sql: `
			SELECT room_id || chr(1) || state_key, coalesce(membership, '') FROM current_state_events
			WHERE type = 'm.room.member' AND ($1 = '' OR state_key LIKE '%:' || $1)
			ORDER BY (room_id || chr(1) || state_key) COLLATE "C"`, args: []any{opts.ServerName}},
		query{sql: targetMemberships, args: []any{opts.ServerName}},
		func(key, srcVal, dstVal string) {
			if srcVal != absent {
				r.Memberships++
			}

			if srcVal != dstVal {
				r.MembershipDiffs++
				diffs.add(fmt.Sprintf("%s: synapse=%s syngo=%s", strings.ReplaceAll(key, "\x01", " "), orDash(srcVal), orDash(dstVal)))
			}
		})
	if err != nil {
		return err
	}

	if r.MembershipDiffs > 0 {
		r.problem("членство расходится у %d пар (комната пользователь): %s", r.MembershipDiffs, diffs.String())
	}

	return nil
}

func (r *VerifyReport) problem(format string, args ...any) {
	r.Problems = append(r.Problems, fmt.Sprintf(format, args...))
}

// absent - значение стороны, где ключа нет (в данных пустых значений "\x00" не бывает).
const absent = "\x00"

func orDash(value string) string {
	if value == absent {
		return "нет"
	}

	return value
}

// mergeKeyed сливает два потока (key, value), отсортированных по key побайтно
// (COLLATE "C"), и вызывает fn для каждого ключа; у отсутствующей стороны
// значение absent.
// query - запрос сверки с аргументами.
type query struct {
	sql  string
	args []any
}

func mergeKeyed(ctx context.Context, src, dst Querier, srcQuery, dstQuery query, fn func(key, srcVal, dstVal string)) error {
	left, err := newCursor(ctx, src, srcQuery)
	if err != nil {
		return err
	}

	defer left.close()

	right, err := newCursor(ctx, dst, dstQuery)
	if err != nil {
		return err
	}

	defer right.close()

	for left.ok || right.ok {
		switch {
		case !right.ok || (left.ok && left.key < right.key):
			fn(left.key, left.val, absent)

			err = left.next()
		case !left.ok || right.key < left.key:
			fn(right.key, absent, right.val)

			err = right.next()
		default:
			fn(left.key, left.val, right.val)

			if err = left.next(); err == nil {
				err = right.next()
			}
		}

		if err != nil {
			return err
		}
	}

	return nil
}

type cursor struct {
	rows     pgx.Rows
	key, val string
	ok       bool
}

func newCursor(ctx context.Context, q Querier, qr query) (*cursor, error) {
	rows, err := q.Query(ctx, qr.sql, qr.args...)
	if err != nil {
		return nil, fmt.Errorf("synapseexport: сверка: %w", err)
	}

	c := &cursor{rows: rows}
	if err := c.next(); err != nil {
		rows.Close()

		return nil, err
	}

	return c, nil
}

func (c *cursor) next() error {
	if !c.rows.Next() {
		c.ok = false

		if err := c.rows.Err(); err != nil {
			return fmt.Errorf("synapseexport: сверка: %w", err)
		}

		return nil
	}

	if err := c.rows.Scan(&c.key, &c.val); err != nil {
		return fmt.Errorf("synapseexport: сверка: %w", err)
	}

	c.ok = true

	return nil
}

func (c *cursor) close() { c.rows.Close() }

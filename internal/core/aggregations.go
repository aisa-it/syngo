package core

import "context"

// Серверные агрегации связей (bundled aggregations): чтение сводок
// тредов, правок и ссылок пачкой на выдачу. Логика выборки перенесена из
// HTTP-обработчиков; unsigned.m.relations собирает транспорт.

// ThreadSummary — сводка треда для m.thread: число ответов, последний ответ и
// участвовал ли пользователь ответом.
type ThreadSummary struct {
	Count         int64
	LatestEventID string
	Participated  bool
}

// Aggregations — агрегации пачки событий по id родителя.
type Aggregations struct {
	// Threads — сводки тредов по корню.
	Threads map[string]ThreadSummary
	// Edits — id последней действующей правки (m.replace) по исходному событию.
	Edits map[string]string
	// References — id событий m.reference по родителю, в порядке ленты.
	References map[string][]string
	// Events — последние ответы тредов и правки целиком (поля клиентской выдачи):
	// читаются тем же запросом, отдельная подгрузка не нужна.
	Events map[string]Event
}

// AggregationStore - агрегации пачки событий и границы видимости комнат.
type AggregationStore interface {
	// BundledAggregations - агрегации parents одним запросом с учетом vis.
	BundledAggregations(ctx context.Context, userID string, parents []string, vis map[string]Visibility) (Aggregations, error)
	VisibilityCutoffs(ctx context.Context, userID string, roomIDs []string) (map[string]Visibility, error)
}

// AggregationTarget - какие агрегации собирать событию.
type AggregationTarget struct {
	RoomID string
	Sender string
	// Threads - событие может быть корнем треда (у него нет своей связи).
	Threads bool
	// Edits - правки применимы (событие не удалено).
	Edits bool
}

// BundledAggregations - агрегации выдачи: Aggs - событий выдачи, Latest -
// последних ответов тредов (их правки), LatestTargets - цели последних ответов.
type BundledAggregations struct {
	Aggs          Aggregations
	Latest        Aggregations
	LatestTargets map[string]AggregationTarget
}

// AggregationsService читает агрегации выдачи фиксированным числом запросов.
type AggregationsService struct {
	store AggregationStore
	warn  func(ctx context.Context, msg string, err error)
}

// NewAggregationsService создает сервис; warn - сбои, после которых выдача
// продолжается без части агрегаций (nil - без журнала).
func NewAggregationsService(store AggregationStore, warn func(ctx context.Context, msg string, err error)) *AggregationsService {
	if warn == nil {
		warn = func(context.Context, string, error) {}
	}

	return &AggregationsService{store: store, warn: warn}
}

// Bundle читает агрегации событий ids (цели - targets). vis - уже посчитанные
// границы видимости по комнатам (может быть nil): недостающие комнаты
// дочитываются одним запросом. Число запросов не зависит от числа событий и
// комнат: один запрос агрегаций (последние ответы и правки - в нем же) и, если
// в выдаче есть треды, второй - правки последних ответов. ok=false - агрегаций
// нет (сбой основного запроса), выдача без них.
func (s *AggregationsService) Bundle(
	ctx context.Context, userID string, targets map[string]AggregationTarget, ids []string, vis map[string]Visibility,
) (BundledAggregations, bool) {
	vis = s.completeVisibility(ctx, userID, vis, targets)

	aggs, err := s.store.BundledAggregations(ctx, userID, ids, vis)
	if err != nil {
		s.warn(ctx, "bundled aggregations failed", err)

		return BundledAggregations{}, false
	}

	// latest_event треда - со своей правкой, как в Synapse: иначе в панели тредов
	// виден текст ответа до правки.
	out := BundledAggregations{Aggs: aggs, LatestTargets: map[string]AggregationTarget{}}
	latestIDs := make([]string, 0)

	for _, id := range ids {
		summary, ok := aggs.Threads[id]
		if !ok || !targets[id].Threads {
			continue
		}

		if latest, ok := aggs.Events[summary.LatestEventID]; ok {
			out.LatestTargets[latest.EventID] = AggregationTarget{RoomID: latest.RoomID, Edits: true}
			latestIDs = append(latestIDs, latest.EventID)
		}
	}

	if len(latestIDs) > 0 {
		out.Latest, err = s.store.BundledAggregations(ctx, userID, latestIDs, vis)
		if err != nil {
			s.warn(ctx, "bundled aggregations of thread replies failed", err)
		}
	}

	return out, true
}

// completeVisibility дочитывает границы видимости комнат, которых нет в vis.
// Ошибка - консервативно: связи этих комнат не учитываются.
func (s *AggregationsService) completeVisibility(
	ctx context.Context, userID string, vis map[string]Visibility, targets map[string]AggregationTarget,
) map[string]Visibility {
	out := make(map[string]Visibility, len(vis))
	for roomID, v := range vis {
		out[roomID] = v
	}

	missing := make([]string, 0)
	seen := map[string]bool{}

	for _, t := range targets {
		if _, ok := out[t.RoomID]; ok || seen[t.RoomID] {
			continue
		}

		seen[t.RoomID] = true
		missing = append(missing, t.RoomID)
	}

	if len(missing) == 0 {
		return out
	}

	loaded, err := s.store.VisibilityCutoffs(ctx, userID, missing)
	if err != nil {
		s.warn(ctx, "history visibility for aggregations failed", err)

		for _, roomID := range missing {
			out[roomID] = Visibility{From: 1 << 62}
		}

		return out
	}

	for roomID, v := range loaded {
		out[roomID] = v
	}

	return out
}

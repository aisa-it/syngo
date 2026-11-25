package api

import (
	"context"
	"encoding/json"

	"github.com/aisa-it/syngo/internal/core"
)

// Серверные агрегации связей (bundled aggregations): unsigned.m.relations
// с m.thread (последний ответ, число ответов, участие пользователя), m.replace
// (последняя действующая правка целиком) и m.reference (id ссылающихся событий).
// По ним Element показывает число ответов в треде и последнюю правку без
// отдельных запросов /relations на каждое сообщение.
//
// Агрегации добавляются во всех путях выдачи событий: /sync (timeline), /messages,
// /context, /event, /relations, /threads. Правила - как в спеке и Synapse:
//   - у событий состояния, правок и реакций агрегаций нет;
//   - m.thread - только у событий без собственной связи (корень треда не может
//     сам быть в треде или правкой);
//   - правка удаленного события не применяется (m.replace не отдается), удаленная
//     правка или ответ не учитываются;
//   - связи из скрытого по history_visibility промежутка не учитываются;
//   - latest_event треда отдается со своей последней правкой.

// relatesTo - rel_type из content.m.relates_to (пусто, если связи нет).
func relatesTo(content json.RawMessage) string {
	var body struct {
		RelatesTo *struct {
			RelType string `json:"rel_type"`
		} `json:"m.relates_to"`
	}

	if len(content) == 0 || json.Unmarshal(content, &body) != nil || body.RelatesTo == nil {
		return ""
	}

	return body.RelatesTo.RelType
}

// aggregationTargets выбирает события, которым положены агрегации, без повторов
// (одно событие может прийти в нескольких срезах, например /context).
func aggregationTargets(groups [][]eventView) (targets map[string]core.AggregationTarget, ids []string) {
	targets = map[string]core.AggregationTarget{}
	ids = make([]string, 0)

	for _, views := range groups {
		for i := range views {
			view := &views[i]
			if view.StateKey != nil {
				continue
			}

			if _, seen := targets[view.EventID]; seen {
				continue
			}

			relType := relatesTo(view.Content)
			if relType == "m.replace" || relType == "m.annotation" {
				continue
			}

			targets[view.EventID] = core.AggregationTarget{
				RoomID:  view.RoomID,
				Sender:  view.Sender,
				Threads: relType == "",
				Edits:   view.redactedBy == "",
			}
			ids = append(ids, view.EventID)
		}
	}

	return targets, ids
}

// attachAggregations добавляет unsigned.m.relations событиям всех срезов. vis -
// уже посчитанные границы видимости по комнатам (может быть nil). Выборка и
// число запросов - в core.AggregationsService.
func (s *Server) attachAggregations(
	ctx context.Context, userID string, vis map[string]core.Visibility, groups ...[]eventView,
) {
	if s.aggregations == nil {
		return
	}

	bundleAggregations(ctx, s.aggregations, userID, vis, groups...)
}

func bundleAggregations(
	ctx context.Context, svc *core.AggregationsService, userID string, vis map[string]core.Visibility,
	groups ...[]eventView,
) {
	targets, ids := aggregationTargets(groups)
	if len(ids) == 0 {
		return
	}

	bundle, ok := svc.Bundle(ctx, userID, targets, ids, vis)
	if !ok {
		return
	}

	for _, views := range groups {
		for i := range views {
			t, ok := targets[views[i].EventID]
			if !ok {
				continue
			}

			rels := relationsBundle(views[i].EventID, t, bundle.Aggs, userID, func(latest core.Event) eventView {
				view := newEventView(latest)
				if nested := relationsBundle(latest.EventID, bundle.LatestTargets[latest.EventID],
					bundle.Latest, userID, nil); len(nested) > 0 {
					view.Unsigned["m.relations"] = nested
				}

				return view
			})
			if len(rels) == 0 {
				continue
			}

			if views[i].Unsigned == nil {
				views[i].Unsigned = map[string]any{}
			}

			views[i].Unsigned["m.relations"] = rels
		}
	}
}

// relationsBundle собирает unsigned.m.relations одного события; latestView
// готовит latest_event треда (nil - треды не собираются).
func relationsBundle(
	eventID string, t core.AggregationTarget, aggs core.Aggregations,
	userID string, latestView func(core.Event) eventView,
) map[string]any {
	rels := map[string]any{}

	if summary, ok := aggs.Threads[eventID]; ok && t.Threads && latestView != nil {
		if latest, ok := aggs.Events[summary.LatestEventID]; ok {
			rels["m.thread"] = map[string]any{
				"latest_event":              latestView(latest),
				"count":                     summary.Count,
				"current_user_participated": summary.Participated || t.Sender == userID,
			}
		}
	}

	if edit, ok := aggs.Edits[eventID]; ok && t.Edits {
		if ev, ok := aggs.Events[edit]; ok {
			rels["m.replace"] = newEventView(ev)
		}
	}

	if refs := aggs.References[eventID]; len(refs) > 0 {
		chunk := make([]map[string]string, 0, len(refs))
		for _, id := range refs {
			chunk = append(chunk, map[string]string{"event_id": id})
		}

		rels["m.reference"] = map[string]any{"chunk": chunk}
	}

	return rels
}

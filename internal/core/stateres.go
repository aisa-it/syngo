package core

import (
	"context"
	"slices"
	"sort"

	"github.com/aisa-it/syngo/internal/federation/auth"
	"github.com/aisa-it/syngo/internal/state"
)

// Состояние комнаты после входящих событий федерации (state resolution v2;
// логика - в домене).
//
// Для каждого события: состояние до него - разрешение наборов состояний после его
// prev_events (один prev - его состояние, prev без записанного состояния - текущее
// состояние комнаты); событие проверяется auth rules по этому состоянию и при
// отказе в состояние и forward extremities не попадает. Состояние после события
// записывается, extremities сдвигаются, текущее состояние комнаты - разрешение
// наборов состояний после всех forward extremities.

// StateRow - строка материализованного состояния комнаты.
type StateRow struct {
	Type     string
	StateKey string
	EventID  string
}

// RoomStateTx - состояние комнаты в транзакции под блокировкой комнаты.
type RoomStateTx interface {
	// RoomVersion - версия комнаты.
	RoomVersion(ctx context.Context) (string, error)
	// CurrentState - материализованное состояние комнаты (room_state).
	CurrentState(ctx context.Context) (state.StateMap, error)
	// ForwardExtremities - forward extremities комнаты (exclude - события, которые
	// нельзя взять опорой для комнаты без extremities).
	ForwardExtremities(ctx context.Context, exclude []string) ([]string, error)
	// SetForwardExtremities заменяет forward extremities.
	SetForwardExtremities(ctx context.Context, ids []string) error
	// StateAfter - записанное состояние после событий (без записи - нет в ответе).
	StateAfter(ctx context.Context, eventIDs []string) (map[string]state.StateMap, error)
	// Events - события комнаты по id.
	Events(ctx context.Context, ids []string) (map[string]state.Event, error)
	// AuthChains - события ids с полными auth chains.
	AuthChains(ctx context.Context, ids []string) (map[string]state.Event, error)
	// SaveStateAfter записывает состояние после события (parentEventID - prev, от
	// состояния которого оно отличается мало, или "").
	SaveStateAfter(ctx context.Context, eventID string, after state.StateMap, parentEventID string, rejected bool) error
	// ApplyCurrentState делает room_state и room_memberships равными st и
	// возвращает изменившиеся строки (EventID "" - ключ удален).
	ApplyCurrentState(ctx context.Context, st state.StateMap, groupEventID string) ([]StateRow, error)
}

// StateResolutionStore - транзакция состояния комнаты.
type StateResolutionStore interface {
	// WithRoomState выполняет fn в транзакции под блокировкой состояния комнаты.
	WithRoomState(ctx context.Context, roomID string, fn func(RoomStateTx) error) error
}

// StateResolutionService - доменный сервис разрешения состояния комнаты.
type StateResolutionService struct {
	store StateResolutionStore
	// onApplied получает число изменившихся и конфликтных ключей (журнал).
	onApplied func(ctx context.Context, roomID string, changed, conflicted int)
}

// NewStateResolutionService создает сервис; onApplied может быть nil.
func NewStateResolutionService(
	store StateResolutionStore, onApplied func(ctx context.Context, roomID string, changed, conflicted int),
) *StateResolutionService {
	return &StateResolutionService{store: store, onApplied: onApplied}
}

// Materialize применяет к состоянию комнаты уже записанные события events (все
// события комнаты из транзакции, включая не-state: от них зависят extremities).
// Повтор для тех же событий ничего не меняет.
func (s *StateResolutionService) Materialize(ctx context.Context, roomID string, events []state.Event) error {
	if len(events) == 0 {
		return nil
	}

	var result RoomStateResult

	err := s.store.WithRoomState(ctx, roomID, func(tx RoomStateTx) error {
		var err error

		result, err = ApplyRoomEvents(ctx, tx, roomID, events, ApplyOptions{})

		return err
	})
	if err != nil {
		return err
	}

	if len(result.Changed) > 0 && s.onApplied != nil {
		s.onApplied(ctx, roomID, len(result.Changed), result.Conflicted)
	}

	return nil
}

// Apply применяет события к состоянию комнаты в транзакции tx, открытой
// вызывающим (запись события и его состояние - одной транзакцией).
func (s *StateResolutionService) Apply(
	ctx context.Context, tx RoomStateTx, roomID string, events []state.Event, opts ApplyOptions,
) (RoomStateResult, error) {
	result, err := ApplyRoomEvents(ctx, tx, roomID, events, opts)
	if err != nil {
		return result, err
	}

	if len(result.Changed) > 0 && s.onApplied != nil {
		s.onApplied(ctx, roomID, len(result.Changed), result.Conflicted)
	}

	return result, nil
}

// RoomStateResult - итог применения событий к состоянию комнаты.
type RoomStateResult struct {
	// Changed - изменившиеся ключи room_state.
	Changed []StateRow
	// Conflicted - суммарный размер full conflicted set всех разрешений.
	Conflicted int
	// Rejected - события, не прошедшие auth по состоянию до них (и отклоненные
	// заранее, ApplyOptions.Rejected).
	Rejected []string
	// SoftFailed - события, прошедшие auth по состоянию до них, но не по текущему
	// состоянию комнаты (soft-fail, ApplyOptions.SoftFail): состояние после них
	// записано, в forward extremities и текущее состояние они не попадают.
	SoftFailed []string
}

// ApplyOptions - известное заранее о событиях.
type ApplyOptions struct {
	// StateBefore - состояние до события (state из ответа send_join); иначе оно
	// вычисляется по prev_events.
	StateBefore map[string]state.StateMap
	// Trusted - события, которые не проверяются по состоянию до них (собственный
	// join из send_join: его уже проверил резидентный сервер).
	Trusted map[string]struct{}
	// PrevStates - состояние после prev_events, которых у нас нет (дыра в DAG,
	// состояние получено от отправителя по /state_ids): участвует в разрешении
	// состояния до события вместе с записанными состояниями известных prev.
	PrevStates map[string]state.StateMap
	// Rejected - события, уже отклоненные по своим auth_events: состояние до них
	// вычисляется и записывается, событие помечается отклоненным без проверки.
	Rejected map[string]struct{}
	// SoftFail - проверять принятые события и по текущему состоянию комнаты
	// (Synapse _check_for_soft_fail): новые события федерации.
	SoftFail bool
}

// ApplyRoomEvents применяет события к состоянию комнаты в транзакции tx.
func ApplyRoomEvents(
	ctx context.Context, tx RoomStateTx, roomID string, events []state.Event, opts ApplyOptions,
) (RoomStateResult, error) {
	var result RoomStateResult

	version, err := tx.RoomVersion(ctx)
	if err != nil {
		return result, err
	}

	app := &roomApply{ctx: ctx, tx: tx, roomID: roomID, algorithm: state.AlgorithmFor(version), result: &result}

	sorted := append([]state.Event(nil), events...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Depth != sorted[j].Depth {
			return sorted[i].Depth < sorted[j].Depth
		}

		return sorted[i].ID < sorted[j].ID
	})

	ids := make([]string, 0, len(sorted))
	for i := range sorted {
		ids = append(ids, sorted[i].ID)
	}

	extremities, err := tx.ForwardExtremities(ctx, ids)
	if err != nil {
		return result, err
	}

	done, err := tx.StateAfter(ctx, ids)
	if err != nil {
		return result, err
	}

	app.cache = make(map[string]state.StateMap, len(done)+len(sorted))
	for id, st := range done {
		app.cache[id] = st
	}

	extSet := make(map[string]struct{}, len(extremities)+len(sorted))
	for _, id := range extremities {
		extSet[id] = struct{}{}
	}

	applied := false

	for i := range sorted {
		ev := sorted[i]
		if _, ok := done[ev.ID]; ok {
			continue
		}

		if ev.RoomID == "" {
			ev.RoomID = roomID
		}

		accepted, before, err := app.applyEvent(ev, opts)
		if err != nil {
			return result, err
		}

		applied = true

		if !accepted {
			result.Rejected = append(result.Rejected, ev.ID)

			continue
		}

		if _, trusted := opts.Trusted[ev.ID]; opts.SoftFail && !trusted {
			soft, err := app.softFailed(ev, before, extSet, opts)
			if err != nil {
				return result, err
			}

			if soft {
				result.SoftFailed = append(result.SoftFailed, ev.ID)

				continue
			}
		}

		for _, prev := range ev.Prev {
			delete(extSet, prev)
		}

		extSet[ev.ID] = struct{}{}
	}

	if !applied {
		return result, nil
	}

	exts := make([]string, 0, len(extSet))
	for id := range extSet {
		exts = append(exts, id)
	}

	sort.Strings(exts)

	if err := tx.SetForwardExtremities(ctx, exts); err != nil {
		return result, err
	}

	if len(exts) == 0 {
		return result, nil
	}

	sets, err := app.statesOf(exts)
	if err != nil {
		return result, err
	}

	if len(sets) == 0 {
		return result, nil
	}

	current, groupEvent := sets[0], ""
	if len(sets) == 1 {
		for _, id := range exts {
			if _, ok := app.cache[id]; ok {
				groupEvent = id
			}
		}
	} else {
		current, err = app.resolve(sets)
		if err != nil {
			return result, err
		}
	}

	result.Changed, err = tx.ApplyCurrentState(ctx, current, groupEvent)

	return result, err
}

// roomApply - состояние одного применения событий.
type roomApply struct {
	ctx       context.Context
	tx        RoomStateTx
	roomID    string
	algorithm state.Algorithm
	// cache - состояние после событий (записанное и вычисленное).
	cache  map[string]state.StateMap
	result *RoomStateResult
}

// statesOf - состояние после событий ids, для которых оно известно.
func (a *roomApply) statesOf(ids []string) ([]state.StateMap, error) {
	var missing []string

	for _, id := range ids {
		if _, ok := a.cache[id]; !ok {
			missing = append(missing, id)
		}
	}

	if len(missing) > 0 {
		loaded, err := a.tx.StateAfter(a.ctx, missing)
		if err != nil {
			return nil, err
		}

		for id, st := range loaded {
			a.cache[id] = st
		}
	}

	sets := make([]state.StateMap, 0, len(ids))

	for _, id := range ids {
		if st, ok := a.cache[id]; ok {
			sets = append(sets, st)
		}
	}

	return sets, nil
}

// ResolveRoomStateSets разрешает наборы состояний комнаты с полными auth chains
// их событий (алгоритм - по версии комнаты).
func ResolveRoomStateSets(ctx context.Context, tx RoomStateTx, roomID string, sets []state.StateMap) (state.StateMap, error) {
	version, err := tx.RoomVersion(ctx)
	if err != nil {
		return nil, err
	}

	app := &roomApply{ctx: ctx, tx: tx, roomID: roomID, algorithm: state.AlgorithmFor(version), result: &RoomStateResult{}}

	return app.resolve(sets)
}

// resolve разрешает наборы состояний с полными auth chains их событий.
func (a *roomApply) resolve(sets []state.StateMap) (state.StateMap, error) {
	seen := map[string]struct{}{}
	ids := make([]string, 0, len(sets[0]))

	for _, set := range sets {
		for _, id := range set {
			if _, ok := seen[id]; !ok {
				seen[id] = struct{}{}
				ids = append(ids, id)
			}
		}
	}

	events, err := a.tx.AuthChains(a.ctx, ids)
	if err != nil {
		return nil, err
	}

	resolver := state.New().WithRoom(a.roomID).WithAlgorithm(a.algorithm)

	resolved, err := resolver.Resolve(sets, events)
	if err != nil {
		return nil, err
	}

	a.result.Conflicted += resolver.Stats.Conflicted

	return state.StateMap(resolved), nil
}

// applyEvent вычисляет состояние до события, проверяет его и записывает состояние
// после. false - событие отклонено auth rules по состоянию до него.
func (a *roomApply) applyEvent(ev state.Event, opts ApplyOptions) (accepted bool, before state.StateMap, err error) {
	before, parent, err := a.stateBefore(ev, opts)
	if err != nil {
		return false, nil, err
	}

	_, trusted := opts.Trusted[ev.ID]
	_, rejected := opts.Rejected[ev.ID]

	switch {
	case trusted:
	case rejected:
		ev.Rejected = true
	default:
		authorized, err := a.authorizedBy(ev, before)
		if err != nil {
			return false, nil, err
		}

		ev.Rejected = !authorized
	}

	accepted, err = a.saveAfter(ev, before, parent)

	return accepted, before, err
}

// authorizedBy проверяет событие auth rules по состоянию st (auth-события - из st).
func (a *roomApply) authorizedBy(ev state.Event, st state.StateMap) (bool, error) {
	authIDs := make([]string, 0, 6)

	for key := range state.AuthTypesForEvent(ev) {
		if id, ok := st[key]; ok {
			authIDs = append(authIDs, id)
		}
	}

	if auth.IsCreateHashRoomID(ev.RoomID) {
		authIDs = append(authIDs, "$"+ev.RoomID[1:])
	}

	authEvents, err := a.tx.Events(a.ctx, authIDs)
	if err != nil {
		return false, err
	}

	return state.Authorized(ev, st, authEvents), nil
}

// softFailed - событие, принятое по состоянию до него, не проходит auth по
// текущему состоянию комнаты (Synapse _check_for_soft_fail): состояние после
// forward extremities (без этого события), а при дыре в prev_events - разрешение
// его с состоянием до события. prev_events равны extremities - состояние то же,
// проверка не нужна.
func (a *roomApply) softFailed(ev state.Event, before state.StateMap, extSet map[string]struct{}, opts ApplyOptions) (bool, error) {
	exts := make([]string, 0, len(extSet))
	for id := range extSet {
		exts = append(exts, id)
	}

	sort.Strings(exts)

	if sameIDs(exts, ev.Prev) {
		return false, nil
	}

	sets, err := a.statesOf(exts)
	if err != nil {
		return false, err
	}

	_, knownBefore := opts.StateBefore[ev.ID]
	if knownBefore || hasAnyKey(opts.PrevStates, ev.Prev) {
		sets = append(sets, before)
	}

	var current state.StateMap

	switch len(sets) {
	case 0:
		return false, nil
	case 1:
		current = sets[0]
	default:
		current, err = a.resolve(sets)
		if err != nil {
			return false, err
		}
	}

	authorized, err := a.authorizedBy(ev, current)

	return !authorized, err
}

// sameIDs - одинаковые множества id.
func sameIDs(sorted, ids []string) bool {
	other := append([]string(nil), ids...)
	sort.Strings(other)
	other = slices.Compact(other)

	if len(sorted) != len(other) {
		return false
	}

	for i := range sorted {
		if sorted[i] != other[i] {
			return false
		}
	}

	return true
}

// hasAnyKey - хотя бы один из ids есть в m.
func hasAnyKey(m map[string]state.StateMap, ids []string) bool {
	for _, id := range ids {
		if _, ok := m[id]; ok {
			return true
		}
	}

	return false
}

// saveAfter записывает состояние после события.
func (a *roomApply) saveAfter(ev state.Event, before state.StateMap, parent string) (bool, error) {
	after := state.StateAfter(before, ev)

	if err := a.tx.SaveStateAfter(a.ctx, ev.ID, after, parent, ev.Rejected); err != nil {
		return false, err
	}

	a.cache[ev.ID] = after

	return !ev.Rejected, nil
}

// stateBefore - состояние до события и prev, от состояния которого оно взято.
func (a *roomApply) stateBefore(
	ev state.Event, opts ApplyOptions,
) (before state.StateMap, parent string, err error) {
	if before, ok := opts.StateBefore[ev.ID]; ok {
		return before, "", nil
	}

	prevs := make([]string, 0, len(ev.Prev))
	for _, id := range ev.Prev {
		if id != ev.ID {
			prevs = append(prevs, id)
		}
	}

	sets, err := a.statesOf(prevs)
	if err != nil {
		return nil, "", err
	}

	// prev, которых у нас нет: состояние после них - от отправителя.
	for _, id := range prevs {
		if _, recorded := a.cache[id]; recorded {
			continue
		}

		if st, ok := opts.PrevStates[id]; ok {
			sets = append(sets, st)
		}
	}

	switch len(sets) {
	case 0:
		// prev без записанного состояния (событие до миграции 0044, outlier или
		// prev_events пусты): опора - текущее состояние комнаты.
		before, err = a.tx.CurrentState(a.ctx)

		return before, "", err
	case 1:
		for _, id := range prevs {
			if _, ok := a.cache[id]; ok {
				return sets[0], id, nil
			}
		}

		return sets[0], "", nil
	default:
		before, err = a.resolve(sets)

		return before, "", err
	}
}

package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// upgradeCopiedState - state-события (state_key ""), которые upgrade переносит в
// новую комнату, как Synapse (clone_existing_room). power_levels переносятся
// отдельно, canonical_alias - после переноса алиасов.
var upgradeCopiedState = []string{
	"m.room.join_rules",
	"m.room.name",
	"m.room.topic",
	"m.room.history_visibility",
	"m.room.guest_access",
	"m.room.avatar",
	"m.room.encryption",
	"m.room.server_acl",
	"m.room.related_groups",
}

// UpgradeResult - итог upgrade: комната-замена, признак повтора (комната уже
// была заменена раньше, новая не создавалась) и некритичные сбои шагов после
// tombstone (перенос бана, алиаса, уровней) - их пишет в лог транспорт.
type UpgradeResult struct {
	ReplacementRoom string
	AlreadyUpgraded bool
	Warnings        []error
}

// warn запоминает некритичный сбой шага upgrade.
func (r *UpgradeResult) warn(step string, err error) {
	r.Warnings = append(r.Warnings, fmt.Errorf("upgrade %s: %w", step, err))
}

// UpgradeRoom заменяет комнату новой версии (POST /rooms/{roomId}/upgrade).
//
// Порядок как в Synapse (RoomCreationHandler.upgrade_room): права на tombstone в
// старой комнате проверяются до создания новой; новая комната получает predecessor,
// перенесенное состояние, power_levels и баны; затем tombstone в старой комнате,
// перенос локальных алиасов, canonical_alias и публикации в каталоге, ограничение
// уровней в старой комнате. Повторный upgrade уже замененной комнаты возвращает
// существующую замену: иначе каждый повтор плодил бы новую комнату и tombstone.
func (s *RoomService) UpgradeRoom(
	ctx context.Context,
	sender, deviceID, roomID string,
	version int,
) (UpgradeResult, error) {
	if version < DefaultRoomVersion || version > MaxRoomVersion {
		return UpgradeResult{}, ErrInvalidRoomVersion
	}

	// Upgrade редкий: один мьютекс на сервис исключает гонку двух запросов на одну
	// комнату (обе создали бы замену). Между процессами не защищает - как и
	// _upgrade_response_cache в Synapse.
	s.upgradeMu.Lock()
	defer s.upgradeMu.Unlock()

	if err := s.requireJoined(ctx, roomID, sender); err != nil {
		return UpgradeResult{}, err
	}

	oldRoom, err := s.store.RoomByID(ctx, roomID)
	if err != nil {
		return UpgradeResult{}, err
	}

	oldState, err := s.store.StateEvents(ctx, roomID)
	if err != nil {
		return UpgradeResult{}, err
	}

	// Права до создания новой комнаты: без них 403 и никаких побочных эффектов.
	emptyKey := ""
	probe := []byte(`{"body":"This room has been replaced","replacement_room":"!probe:` + s.serverName + `"}`)

	if err := s.authorizeStateEvent(sender, roomID, "m.room.tombstone", &emptyKey, probe, oldState); err != nil {
		return UpgradeResult{}, err
	}

	// Заблокированную комнату не заменяют: tombstone в нее не запишется, а новая
	// комната осталась бы сиротой.
	if err := s.checkRoomBlock(ctx, Event{RoomID: roomID, Type: "m.room.tombstone", StateKey: &emptyKey}); err != nil {
		return UpgradeResult{}, err
	}

	byKey := make(map[string]Event, len(oldState))
	for i := range oldState {
		if oldState[i].StateKey != nil {
			byKey[oldState[i].Type+"|"+*oldState[i].StateKey] = oldState[i]
		}
	}

	if tombstone, ok := byKey["m.room.tombstone|"]; ok {
		var body struct {
			ReplacementRoom string `json:"replacement_room"`
		}

		if json.Unmarshal(tombstone.Content, &body) == nil && body.ReplacementRoom != "" {
			return UpgradeResult{ReplacementRoom: body.ReplacementRoom, AlreadyUpgraded: true}, nil
		}
	}

	latest, err := s.store.LatestEvents(ctx, roomID, 1)
	if err != nil {
		return UpgradeResult{}, err
	}

	if len(latest) == 0 {
		return UpgradeResult{}, fmt.Errorf("core: room %s has no events: %w", roomID, ErrNotFound)
	}

	extras := map[string]any{
		"predecessor": map[string]any{"room_id": roomID, "event_id": latest[len(latest)-1].EventID},
	}

	var spaceRoom bool

	if create, ok := byKey["m.room.create|"]; ok {
		var body map[string]any
		if json.Unmarshal(create.Content, &body) == nil {
			if roomType, has := body["type"]; has {
				extras["type"] = roomType
				spaceRoom = roomType == "m.space"
			}

			if federate, has := body["m.federate"]; has {
				extras["m.federate"] = federate
			}
		}
	}

	initialState := make([]StateEventRequest, 0, len(upgradeCopiedState))

	for _, eventType := range upgradeCopiedState {
		if ev, ok := byKey[eventType+"|"]; ok {
			initialState = append(initialState, StateEventRequest{Type: eventType, Content: ev.Content})
		}
	}

	// Пространство переносит дочерние комнаты (MSC3818, как Synapse).
	if spaceRoom {
		for i := range oldState {
			ev := oldState[i]
			if ev.Type == "m.space.child" && ev.StateKey != nil {
				initialState = append(initialState, StateEventRequest{Type: ev.Type, StateKey: *ev.StateKey, Content: ev.Content})
			}
		}
	}

	oldLevels := map[string]any{}
	if pl, ok := byKey["m.room.power_levels|"]; ok {
		if err := json.Unmarshal(pl.Content, &oldLevels); err != nil {
			return UpgradeResult{}, fmt.Errorf("core: old power_levels: %w", err)
		}
	}

	genesisLevels, raised := upgradeGenesisLevels(oldLevels, sender)

	// v12: отправитель upgrade - создатель новой комнаты с бесконечным уровнем, в
	// power_levels.users его быть не должно; временное повышение не нужно.
	if version >= HashRoomIDVersion {
		genesisLevels, raised = levelsWithoutUser(oldLevels, sender), false
	}

	newRoomID, err := s.CreateRoom(ctx, CreateRoomRequest{
		Creator:        sender,
		RoomVersion:    version,
		Preset:         "private_chat",
		IsPublic:       oldRoom.IsPublic,
		InitialState:   initialState,
		CreationExtras: extras,
		PowerLevels:    genesisLevels,
	})
	if err != nil {
		return UpgradeResult{}, err
	}

	res := UpgradeResult{ReplacementRoom: newRoomID}

	// Баны переносятся, иначе забаненный войдет в публичную замену.
	for i := range oldState {
		ev := oldState[i]
		if ev.Type != "m.room.member" || ev.StateKey == nil || membershipOf(ev.Content) != "ban" {
			continue
		}

		ban := map[string]any{"membership": "ban"}

		var body struct {
			Reason string `json:"reason"`
		}

		if json.Unmarshal(ev.Content, &body) == nil && body.Reason != "" {
			ban["reason"] = body.Reason
		}

		content, _ := json.Marshal(ban)
		if _, _, banErr := s.SendState(ctx, sender, deviceID, newRoomID, "m.room.member", *ev.StateKey, "", content); banErr != nil {
			res.warn("ban "+*ev.StateKey, banErr)
		}
	}

	tombstone, err := json.Marshal(map[string]any{
		"body":             "This room has been replaced",
		"replacement_room": newRoomID,
	})
	if err != nil {
		return UpgradeResult{}, err
	}

	// Ключ транзакции пустой: tombstone пишется всегда (раньше фиксированный txn
	// "upgrade" глотал tombstone второго upgrade с того же устройства).
	if _, _, err := s.SendState(ctx, sender, deviceID, roomID, "m.room.tombstone", "", "", tombstone); err != nil {
		return UpgradeResult{}, err
	}

	s.moveAliasesOnUpgrade(ctx, &res, sender, deviceID, roomID, newRoomID, byKey)

	if oldRoom.IsPublic {
		if err := s.store.SetRoomPublic(ctx, roomID, false); err != nil {
			res.warn("unpublish old room", err)
		}
	}

	s.restrictOldRoomLevels(ctx, &res, sender, deviceID, roomID, oldLevels)

	// Итоговые уровни новой комнаты - как в старой (временное повышение снято).
	if raised && len(oldLevels) > 0 {
		content, marshalErr := json.Marshal(oldLevels)
		if marshalErr == nil {
			_, _, marshalErr = s.SendState(ctx, sender, deviceID, newRoomID, "m.room.power_levels", "", "", content)
		}

		if marshalErr != nil {
			res.warn("final power_levels", marshalErr)
		}
	}

	return res, nil
}

// upgradeGenesisLevels - power_levels новой комнаты при создании: копия старых с
// уровнем отправителя, поднятым до нужного для переноса состояния и банов (как
// Synapse). raised - уровень поднимался, итоговые уровни нужно отправить отдельно.
func upgradeGenesisLevels(old map[string]any, sender string) (map[string]any, bool) {
	if len(old) == 0 {
		return nil, false
	}

	levels := make(map[string]any, len(old))
	for k, v := range old {
		levels[k] = v
	}

	needed := max(levelValue(old["state_default"], 50), levelValue(old["ban"], 50))

	if events, ok := old["events"].(map[string]any); ok {
		for _, v := range events {
			needed = max(needed, levelValue(v, 0))
		}
	}

	users := map[string]any{}
	if oldUsers, ok := old["users"].(map[string]any); ok {
		for k, v := range oldUsers {
			users[k] = v
		}
	}

	current := levelValue(old["users_default"], 0)
	if v, ok := users[sender]; ok {
		current = levelValue(v, 0)
	}

	raised := current < needed
	if raised {
		users[sender] = needed
	}

	levels["users"] = users

	return levels, raised
}

// levelsWithoutUser - копия power_levels без пользователя в users (nil для пустых).
func levelsWithoutUser(old map[string]any, userID string) map[string]any {
	if len(old) == 0 {
		return nil
	}

	levels := make(map[string]any, len(old))
	for k, v := range old {
		levels[k] = v
	}

	if oldUsers, ok := old["users"].(map[string]any); ok {
		users := make(map[string]any, len(oldUsers))

		for k, v := range oldUsers {
			if k != userID {
				users[k] = v
			}
		}

		levels["users"] = users
	}

	return levels
}

// restrictOldRoomLevels поднимает events_default и invite в старой комнате до
// max(users_default+1, 50): обычные участники больше не пишут и не приглашают
// (Synapse _update_upgraded_room_pls). Отказ не прерывает upgrade.
func (s *RoomService) restrictOldRoomLevels(
	ctx context.Context, res *UpgradeResult, sender, deviceID, roomID string, oldLevels map[string]any,
) {
	if len(oldLevels) == 0 {
		return
	}

	restricted := max(levelValue(oldLevels["users_default"], 0)+1, 50)

	levels := make(map[string]any, len(oldLevels))
	for k, v := range oldLevels {
		levels[k] = v
	}

	updated := false

	for _, key := range []string{"invite", "events_default"} {
		if levelValue(levels[key], 0) < restricted {
			levels[key] = restricted
			updated = true
		}
	}

	if !updated {
		return
	}

	content, err := json.Marshal(levels)
	if err == nil {
		_, _, err = s.SendState(ctx, sender, deviceID, roomID, "m.room.power_levels", "", "", content)
	}

	if err != nil {
		res.warn("restrict old power_levels", err)
	}
}

// moveAliasesOnUpgrade переносит локальные алиасы в новую комнату и делит
// canonical_alias: локальные алиасы уходят в новую комнату, остальные остаются в
// старой (Synapse _move_aliases_to_new_room). Ошибки только логируются.
func (s *RoomService) moveAliasesOnUpgrade(
	ctx context.Context, res *UpgradeResult, sender, deviceID, oldRoomID, newRoomID string, byKey map[string]Event,
) {
	aliases, err := s.store.AliasesByRoom(ctx, oldRoomID)
	if err != nil {
		res.warn("list aliases", err)
	}

	for _, alias := range aliases {
		creator, _, ownerErr := s.store.AliasOwner(ctx, alias)
		if ownerErr != nil {
			res.warn("alias owner "+alias, ownerErr)

			continue
		}

		if delErr := s.store.DeleteAlias(ctx, alias); delErr != nil {
			res.warn("alias delete "+alias, delErr)

			continue
		}

		if createErr := s.store.CreateAlias(ctx, alias, newRoomID, creator); createErr != nil {
			res.warn("alias move "+alias, createErr)
		}
	}

	canonical, ok := byKey["m.room.canonical_alias|"]
	if !ok {
		return
	}

	var body struct {
		Alias      string   `json:"alias"`
		AltAliases []string `json:"alt_aliases"`
	}

	if err := json.Unmarshal(canonical.Content, &body); err != nil {
		return
	}

	oldContent := map[string]any{}
	if err := json.Unmarshal(canonical.Content, &oldContent); err != nil {
		return
	}

	newContent := map[string]any{}

	if body.Alias != "" && s.isLocalAlias(body.Alias) {
		newContent["alias"] = body.Alias
		delete(oldContent, "alias")
	}

	if len(body.AltAliases) > 0 {
		var keepOld, moveNew []string

		for _, alias := range body.AltAliases {
			if s.isLocalAlias(alias) {
				moveNew = append(moveNew, alias)
			} else {
				keepOld = append(keepOld, alias)
			}
		}

		if len(moveNew) > 0 {
			newContent["alt_aliases"] = moveNew
		}

		if len(keepOld) > 0 {
			oldContent["alt_aliases"] = keepOld
		} else {
			delete(oldContent, "alt_aliases")
		}
	}

	for _, target := range []struct {
		roomID  string
		content map[string]any
	}{{oldRoomID, oldContent}, {newRoomID, newContent}} {
		raw, marshalErr := json.Marshal(target.content)
		if marshalErr == nil {
			_, _, marshalErr = s.SendState(ctx, sender, deviceID, target.roomID, "m.room.canonical_alias", "", "", raw)
		}

		if marshalErr != nil {
			res.warn("canonical_alias "+target.roomID, marshalErr)
		}
	}
}

// isLocalAlias - алиас на этом сервере.
func (s *RoomService) isLocalAlias(alias string) bool {
	local, found := strings.CutSuffix(alias, ":"+s.serverName)

	return found && len(local) > 1 && local[0] == '#'
}

// membershipOf - значение membership из content m.room.member.
func membershipOf(content []byte) string {
	var body struct {
		Membership string `json:"membership"`
	}

	_ = json.Unmarshal(content, &body)

	return body.Membership
}

// levelValue - целый уровень из JSON-значения power_levels (число или строка с
// числом, как допускают старые версии комнат); иначе def.
func levelValue(value any, def int) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}

	return def
}

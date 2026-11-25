package pushrules

import (
	"encoding/json"
	"strconv"
	"strings"
)

// RoomInfo - срез состояния комнаты, нужный для вычисления правил одного
// получателя.
type RoomInfo struct {
	// MemberCount - участники с membership=join.
	MemberCount int
	// DisplayName - displayname получателя в комнате.
	DisplayName string
	// PowerLevels - content m.room.power_levels (nil - события нет).
	PowerLevels json.RawMessage
	// Creator - отправитель m.room.create: без power_levels у него 100.
	Creator string
}

type powerLevels struct {
	Users         map[string]json.RawMessage `json:"users"`
	UsersDefault  json.RawMessage            `json:"users_default"`
	Notifications map[string]json.RawMessage `json:"notifications"`
}

// Context собирает контекст вычисления для события от sender.
func (ri RoomInfo) Context(sender string) Context {
	ctx := Context{
		DisplayName:        ri.DisplayName,
		MemberCount:        ri.MemberCount,
		NotificationLevels: map[string]int{"room": 50},
	}

	if len(ri.PowerLevels) == 0 {
		if sender == ri.Creator && sender != "" {
			ctx.SenderPowerLevel = 100
		}

		return ctx
	}

	var levels powerLevels
	if json.Unmarshal(ri.PowerLevels, &levels) != nil {
		return ctx
	}

	ctx.SenderPowerLevel = levelValue(levels.UsersDefault, 0)
	if raw, ok := levels.Users[sender]; ok {
		ctx.SenderPowerLevel = levelValue(raw, ctx.SenderPowerLevel)
	}

	for key, raw := range levels.Notifications {
		ctx.NotificationLevels[key] = levelValue(raw, ctx.NotificationLevels[key])
	}

	return ctx
}

// levelValue читает уровень: число или (в старых версиях комнат) строка с числом.
func levelValue(raw json.RawMessage, fallback int) int {
	if len(raw) == 0 {
		return fallback
	}

	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		if v, err := n.Int64(); err == nil {
			return int(v)
		}
	}

	var s string
	if json.Unmarshal(raw, &s) == nil {
		if v, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			return v
		}
	}

	return fallback
}

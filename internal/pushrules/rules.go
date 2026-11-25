// Package pushrules - push-правила Matrix (CS API, раздел «Push Notifications»):
// набор серверных правил по умолчанию, слияние с правилами пользователя и
// вычисление действий для события.
//
// Пакет не ходит в БД и не знает про HTTP: на вход - сохраненные правила
// пользователя и контекст комнаты, на выход - действия. Так одно и то же
// вычисление используется в push-рассылке, счетчиках unread в /sync и
// /notifications.
package pushrules

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Виды правил в порядке вычисления (спецификация: override, content, room,
// sender, underride).
const (
	KindOverride  = "override"
	KindContent   = "content"
	KindRoom      = "room"
	KindSender    = "sender"
	KindUnderride = "underride"
)

// ScopeGlobal - единственная область правил в спецификации.
const ScopeGlobal = "global"

// Kinds - виды правил в порядке приоритета.
var Kinds = []string{KindOverride, KindContent, KindRoom, KindSender, KindUnderride}

// ValidKind сообщает, известен ли вид правила.
func ValidKind(kind string) bool {
	for _, k := range Kinds {
		if k == kind {
			return true
		}
	}

	return false
}

// Condition - условие правила. Неизвестные поля не теряются: правило отдается
// клиенту так, как он его сохранил.
type Condition struct {
	Kind    string          `json:"kind"`
	Key     string          `json:"key,omitempty"`
	Pattern *string         `json:"pattern,omitempty"`
	Value   json.RawMessage `json:"value,omitempty"`
	Is      string          `json:"is,omitempty"`
}

// Rule - правило в том виде, в каком оно отдается клиенту и вычисляется.
type Rule struct {
	Kind       string
	RuleID     string
	Default    bool
	Enabled    bool
	Conditions []Condition
	Actions    json.RawMessage
	Pattern    string
}

// MarshalJSON - формат PushRule из спецификации: conditions только у
// override/underride, pattern только у content.
func (r Rule) MarshalJSON() ([]byte, error) {
	out := map[string]any{
		"rule_id": r.RuleID,
		"default": r.Default,
		"enabled": r.Enabled,
		"actions": r.actionsOrEmpty(),
	}

	switch r.Kind {
	case KindOverride, KindUnderride:
		conditions := r.Conditions
		if conditions == nil {
			conditions = []Condition{}
		}

		out["conditions"] = conditions
	case KindContent:
		out["pattern"] = r.Pattern
	}

	return json.Marshal(out)
}

func (r Rule) actionsOrEmpty() json.RawMessage {
	if isNullJSON(r.Actions) {
		return json.RawMessage(`[]`)
	}

	return r.Actions
}

// Ruleset - набор правил области global.
type Ruleset struct {
	Override  []Rule `json:"override"`
	Content   []Rule `json:"content"`
	Room      []Rule `json:"room"`
	Sender    []Rule `json:"sender"`
	Underride []Rule `json:"underride"`
}

// List возвращает список правил вида kind (nil для неизвестного вида).
func (rs *Ruleset) List(kind string) *[]Rule {
	switch kind {
	case KindOverride:
		return &rs.Override
	case KindContent:
		return &rs.Content
	case KindRoom:
		return &rs.Room
	case KindSender:
		return &rs.Sender
	case KindUnderride:
		return &rs.Underride
	}

	return nil
}

// Find ищет правило по виду и id.
func (rs Ruleset) Find(kind, ruleID string) (Rule, bool) {
	list := rs.List(kind)
	if list == nil {
		return Rule{}, false
	}

	for _, rule := range *list {
		if rule.RuleID == ruleID {
			return rule, true
		}
	}

	return Rule{}, false
}

// IsServerDefaultID - id, начинающиеся с точки, зарезервированы за серверными
// правилами (спецификация): пользователь не может создавать такие правила.
func IsServerDefaultID(ruleID string) bool {
	return strings.HasPrefix(ruleID, ".")
}

// StoredRule - строка хранилища. Для пользовательского правила это само
// правило; для серверного (id с точкой) - только переопределение enabled и
// actions, а условия берутся из кода.
type StoredRule struct {
	Kind       string
	RuleID     string
	Enabled    bool
	Conditions json.RawMessage
	Actions    json.RawMessage
	Pattern    string
}

// IsDefaultOverride отличает переопределение серверного правила от мусорной
// строки старого кода: старый PUT .../actions сохранял серверное правило как
// пользовательское с enabled=false и пустыми условиями, и оно выключало
// уведомления. Новый код пишет переопределения с conditions = null.
func (s StoredRule) IsDefaultOverride() bool {
	return IsServerDefaultID(s.RuleID) && isNullJSON(s.Conditions)
}

// Build собирает итоговый набор правил: серверные правила по умолчанию с
// переопределениями пользователя и его собственные правила. Порядок как в
// Synapse: .m.rule.master первым, затем override пользователя, затем остальные
// серверные override; в content и underride пользовательские правила идут
// перед серверными. stored должен быть отсортирован по приоритету.
func Build(userID string, stored []StoredRule) Ruleset {
	defaults := Defaults(userID)

	overrides := map[string]StoredRule{}
	user := Ruleset{}

	for _, row := range stored {
		if IsServerDefaultID(row.RuleID) {
			if row.IsDefaultOverride() {
				overrides[row.Kind+"\x00"+row.RuleID] = row
			}

			continue
		}

		list := user.List(row.Kind)
		if list == nil {
			continue
		}

		*list = append(*list, ruleFromStored(row))
	}

	applyOverrides := func(rules []Rule) []Rule {
		out := make([]Rule, len(rules))
		copy(out, rules)

		for i := range out {
			row, ok := overrides[out[i].Kind+"\x00"+out[i].RuleID]
			if !ok {
				continue
			}

			out[i].Enabled = row.Enabled
			if !isNullJSON(row.Actions) {
				out[i].Actions = row.Actions
			}
		}

		return out
	}

	def := Ruleset{
		Override:  applyOverrides(defaults.Override),
		Content:   applyOverrides(defaults.Content),
		Underride: applyOverrides(defaults.Underride),
	}

	result := Ruleset{
		Content:   append(append([]Rule{}, user.Content...), def.Content...),
		Room:      append([]Rule{}, user.Room...),
		Sender:    append([]Rule{}, user.Sender...),
		Underride: append(append([]Rule{}, user.Underride...), def.Underride...),
	}

	// .m.rule.master - первым, остальные серверные override - после пользовательских.
	result.Override = append(result.Override, def.Override[0])
	result.Override = append(result.Override, user.Override...)
	result.Override = append(result.Override, def.Override[1:]...)

	return result
}

func ruleFromStored(row StoredRule) Rule {
	rule := Rule{
		Kind:    row.Kind,
		RuleID:  row.RuleID,
		Enabled: row.Enabled,
		Actions: row.Actions,
		Pattern: row.Pattern,
	}

	if !isNullJSON(row.Conditions) {
		_ = json.Unmarshal(row.Conditions, &rule.Conditions)
	}

	return rule
}

// EffectiveConditions - условия, по которым правило вычисляется: у content,
// room и sender они выводятся из pattern и rule_id (спецификация).
func (r Rule) EffectiveConditions() []Condition {
	switch r.Kind {
	case KindContent:
		pattern := r.Pattern

		return []Condition{{Kind: CondEventMatch, Key: "content.body", Pattern: &pattern}}
	case KindRoom:
		id := r.RuleID

		return []Condition{{Kind: CondEventMatch, Key: "room_id", Pattern: &id}}
	case KindSender:
		id := r.RuleID

		return []Condition{{Kind: CondEventMatch, Key: "sender", Pattern: &id}}
	}

	return r.Conditions
}

func isNullJSON(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)

	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

// ValidateActions проверяет список действий: строки notify, dont_notify,
// coalesce (последние две устарели, но клиенты их шлют) и объекты set_tweak.
// Как в Synapse, неизвестное действие - ошибка.
func ValidateActions(raw json.RawMessage) bool {
	var actions []json.RawMessage
	if err := json.Unmarshal(raw, &actions); err != nil || actions == nil {
		return false
	}

	for _, action := range actions {
		var name string
		if json.Unmarshal(action, &name) == nil {
			switch name {
			case ActionNotify, ActionDontNotify, ActionCoalesce:
				continue
			}

			return false
		}

		var tweak map[string]json.RawMessage
		if json.Unmarshal(action, &tweak) != nil {
			return false
		}

		var tweakName string
		if json.Unmarshal(tweak["set_tweak"], &tweakName) != nil || tweakName == "" {
			return false
		}
	}

	return true
}

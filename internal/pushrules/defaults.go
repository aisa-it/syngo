package pushrules

import (
	"encoding/json"
	"strings"
)

// Действия правил.
const (
	ActionNotify     = "notify"
	ActionDontNotify = "dont_notify"
	ActionCoalesce   = "coalesce"
)

// Id серверных правил, которые вычислитель обрабатывает особо.
const (
	RuleMaster              = ".m.rule.master"
	RuleContainsDisplayName = ".m.rule.contains_display_name"
	RuleContainsUserName    = ".m.rule.contains_user_name"
	RuleRoomNotif           = ".m.rule.roomnotif"
)

// Готовые наборы действий серверных правил.
const (
	actionsNone            = `[]`
	actionsNotify          = `["notify"]`
	actionsNotifySound     = `["notify",{"set_tweak":"sound","value":"default"}]`
	actionsNotifyHighlight = `["notify",{"set_tweak":"highlight"}]`
	actionsMention         = `["notify",{"set_tweak":"sound","value":"default"},{"set_tweak":"highlight"}]`
	actionsRing            = `["notify",{"set_tweak":"sound","value":"ring"}]`
)

func match(key, pattern string) Condition {
	return Condition{Kind: CondEventMatch, Key: key, Pattern: &pattern}
}

func propertyIs(key, value string) Condition {
	return Condition{Kind: CondEventPropertyIs, Key: key, Value: json.RawMessage(value)}
}

func defaultRule(kind, id, actions string, conditions ...Condition) Rule {
	if conditions == nil {
		conditions = []Condition{}
	}

	return Rule{
		Kind: kind, RuleID: id, Default: true, Enabled: true,
		Conditions: conditions, Actions: json.RawMessage(actions),
	}
}

// Defaults - серверные правила по умолчанию для пользователя (CS API v1.11,
// «Predefined Rules»; порядок внутри вида - как в спецификации). Раньше их было
// четыре с одинаковым условием «любой m.room.message»: упоминания не
// подсвечивались, а выключить уведомления (.m.rule.master) было нельзя.
func Defaults(userID string) Ruleset {
	userJSON, _ := json.Marshal(userID)

	master := defaultRule(KindOverride, RuleMaster, actionsNone)
	master.Enabled = false

	contains := Rule{
		Kind: KindContent, RuleID: RuleContainsUserName, Default: true, Enabled: true,
		Pattern: Localpart(userID), Actions: json.RawMessage(actionsMention),
	}

	return Ruleset{
		Override: []Rule{
			master,
			defaultRule(KindOverride, ".m.rule.suppress_notices", actionsNone,
				match("content.msgtype", "m.notice")),
			defaultRule(KindOverride, ".m.rule.invite_for_me", actionsNotifySound,
				match("type", "m.room.member"),
				match("content.membership", "invite"),
				match("state_key", userID)),
			defaultRule(KindOverride, ".m.rule.member_event", actionsNone,
				match("type", "m.room.member")),
			defaultRule(KindOverride, ".m.rule.is_user_mention", actionsMention,
				Condition{Kind: CondEventPropertyContains, Key: `content.m\.mentions.user_ids`, Value: userJSON}),
			defaultRule(KindOverride, RuleContainsDisplayName, actionsMention,
				Condition{Kind: CondContainsDisplayName}),
			defaultRule(KindOverride, ".m.rule.is_room_mention", actionsNotifyHighlight,
				propertyIs(`content.m\.mentions.room`, `true`),
				Condition{Kind: CondSenderNotificationPermission, Key: "room"}),
			defaultRule(KindOverride, RuleRoomNotif, actionsNotifyHighlight,
				Condition{Kind: CondSenderNotificationPermission, Key: "room"},
				match("content.body", "@room")),
			defaultRule(KindOverride, ".m.rule.tombstone", actionsNotifyHighlight,
				match("type", "m.room.tombstone"),
				match("state_key", "")),
			defaultRule(KindOverride, ".m.rule.reaction", actionsNone,
				match("type", "m.reaction")),
			defaultRule(KindOverride, ".m.rule.room.server_acl", actionsNone,
				match("type", "m.room.server_acl"),
				match("state_key", "")),
			defaultRule(KindOverride, ".m.rule.suppress_edits", actionsNone,
				propertyIs(`content.m\.relates_to.rel_type`, `"m.replace"`)),
		},
		Content: []Rule{contains},
		Room:    []Rule{},
		Sender:  []Rule{},
		Underride: []Rule{
			defaultRule(KindUnderride, ".m.rule.call", actionsRing,
				match("type", "m.call.invite")),
			defaultRule(KindUnderride, ".m.rule.encrypted_room_one_to_one", actionsNotifySound,
				Condition{Kind: CondRoomMemberCount, Is: "2"},
				match("type", "m.room.encrypted")),
			defaultRule(KindUnderride, ".m.rule.room_one_to_one", actionsNotifySound,
				Condition{Kind: CondRoomMemberCount, Is: "2"},
				match("type", "m.room.message")),
			defaultRule(KindUnderride, ".m.rule.message", actionsNotify,
				match("type", "m.room.message")),
			defaultRule(KindUnderride, ".m.rule.encrypted", actionsNotify,
				match("type", "m.room.encrypted")),
		},
	}
}

// Localpart - локальная часть MXID (@alice:server -> alice).
func Localpart(userID string) string {
	trimmed := strings.TrimPrefix(userID, "@")
	if i := strings.IndexByte(trimmed, ':'); i >= 0 {
		return trimmed[:i]
	}

	return trimmed
}

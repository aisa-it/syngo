package pushrules

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// Виды условий (спецификация, «Conditions»).
const (
	CondEventMatch                   = "event_match"
	CondEventPropertyIs              = "event_property_is"
	CondEventPropertyContains        = "event_property_contains"
	CondContainsDisplayName          = "contains_display_name"
	CondRoomMemberCount              = "room_member_count"
	CondSenderNotificationPermission = "sender_notification_permission"
)

// Event - событие для вычисления: верхнеуровневые поля и content.
type Event struct {
	Type     string
	Sender   string
	RoomID   string
	StateKey *string
	Content  json.RawMessage
}

// Context - то, что вычислитель знает о комнате и получателе.
type Context struct {
	// DisplayName - имя получателя в этой комнате (m.room.member).
	DisplayName string
	// MemberCount - число участников комнаты с membership=join.
	MemberCount int
	// SenderPowerLevel - уровень отправителя события.
	SenderPowerLevel int
	// NotificationLevels - power_levels.notifications (room по умолчанию 50).
	NotificationLevels map[string]int
}

// Result - итог вычисления.
type Result struct {
	// RuleID - сработавшее правило ("" - ни одно).
	RuleID  string
	Actions json.RawMessage
	Notify  bool
	// Highlight - set_tweak highlight (без value или value=true).
	Highlight bool
	// Sound - значение set_tweak sound.
	Sound string
	// Tweaks - все set_tweak действия (для push-шлюза).
	Tweaks map[string]json.RawMessage
}

// Evaluate вычисляет первое подходящее включенное правило в порядке override,
// content, room, sender, underride.
func (rs *Ruleset) Evaluate(ev Event, ctx Context) Result {
	doc := newEventDoc(ev)

	// m.mentions в content означает «упоминания указаны явно»: устаревшие
	// правила по тексту (имя, логин, @room) не применяются - иначе ответ на
	// сообщение с именем в цитате подсвечивался бы (спецификация v1.7, как в Synapse).
	_, hasMentions := doc.content["m.mentions"]

	for _, kind := range Kinds {
		for _, rule := range *rs.List(kind) {
			if !rule.Enabled {
				continue
			}

			if hasMentions && rule.Default && isLegacyMentionRule(rule.RuleID) {
				continue
			}

			if !allMatch(rule.EffectiveConditions(), doc, ctx) {
				continue
			}

			return resultFromActions(rule.RuleID, rule.actionsOrEmpty())
		}
	}

	return Result{Actions: json.RawMessage(`[]`)}
}

func isLegacyMentionRule(ruleID string) bool {
	return ruleID == RuleContainsDisplayName || ruleID == RuleContainsUserName || ruleID == RuleRoomNotif
}

func resultFromActions(ruleID string, actions json.RawMessage) Result {
	res := Result{RuleID: ruleID, Actions: actions}

	var list []json.RawMessage
	if json.Unmarshal(actions, &list) != nil {
		return res
	}

	for _, action := range list {
		var name string
		if json.Unmarshal(action, &name) == nil {
			if name == ActionNotify {
				res.Notify = true
			}

			continue
		}

		var tweak struct {
			SetTweak string          `json:"set_tweak"`
			Value    json.RawMessage `json:"value"`
		}

		if json.Unmarshal(action, &tweak) != nil || tweak.SetTweak == "" {
			continue
		}

		if res.Tweaks == nil {
			res.Tweaks = map[string]json.RawMessage{}
		}

		value := tweak.Value
		if tweak.SetTweak == "highlight" && isNullJSON(value) {
			value = json.RawMessage(`true`)
		}

		res.Tweaks[tweak.SetTweak] = value

		switch tweak.SetTweak {
		case "highlight":
			res.Highlight = bytes.Equal(bytes.TrimSpace(value), []byte("true"))
		case "sound":
			_ = json.Unmarshal(value, &res.Sound)
		}
	}

	return res
}

func allMatch(conditions []Condition, doc eventDoc, ctx Context) bool {
	for _, cond := range conditions {
		if !matchCondition(cond, doc, ctx) {
			return false
		}
	}

	return true
}

func matchCondition(cond Condition, doc eventDoc, ctx Context) bool {
	switch cond.Kind {
	case CondEventMatch:
		if cond.Pattern == nil {
			return false
		}

		value, ok := doc.lookup(cond.Key).(string)
		if !ok {
			return false
		}

		return globMatch(*cond.Pattern, value, cond.Key == "content.body")
	case CondEventPropertyIs:
		value, ok := doc.lookupExists(cond.Key)
		if !ok {
			return false
		}

		return scalarEqual(value, cond.Value)
	case CondEventPropertyContains:
		value, ok := doc.lookup(cond.Key).([]any)
		if !ok {
			return false
		}

		for _, item := range value {
			if scalarEqual(item, cond.Value) {
				return true
			}
		}

		return false
	case CondContainsDisplayName:
		body, ok := doc.lookup("content.body").(string)
		if !ok || ctx.DisplayName == "" {
			return false
		}

		return wordMatch(regexp.QuoteMeta(ctx.DisplayName), body)
	case CondRoomMemberCount:
		return memberCountMatch(cond.Is, ctx.MemberCount)
	case CondSenderNotificationPermission:
		required, ok := ctx.NotificationLevels[cond.Key]
		if !ok {
			required = 50
		}

		return ctx.SenderPowerLevel >= required
	}

	// Неизвестный вид условия - правило не срабатывает (спецификация).
	return false
}

// memberCountMatch разбирает "2", "==2", "<2", ">2", "<=2", ">=2".
func memberCountMatch(is string, count int) bool {
	op := "=="

	for _, prefix := range []string{"==", "<=", ">=", "<", ">"} {
		if strings.HasPrefix(is, prefix) {
			op, is = prefix, strings.TrimPrefix(is, prefix)

			break
		}
	}

	want, err := strconv.Atoi(is)
	if err != nil {
		return false
	}

	switch op {
	case "<":
		return count < want
	case ">":
		return count > want
	case "<=":
		return count <= want
	case ">=":
		return count >= want
	}

	return count == want
}

// scalarEqual сравнивает значение из события со значением условия: строки,
// целые, bool и null (спецификация: event_property_is не работает с объектами).
func scalarEqual(value any, want json.RawMessage) bool {
	if isNullJSON(want) {
		return value == nil && bytes.Equal(bytes.TrimSpace(want), []byte("null"))
	}

	var expected any

	dec := json.NewDecoder(bytes.NewReader(want))
	dec.UseNumber()

	if dec.Decode(&expected) != nil {
		return false
	}

	switch exp := expected.(type) {
	case string:
		got, ok := value.(string)

		return ok && got == exp
	case bool:
		got, ok := value.(bool)

		return ok && got == exp
	case json.Number:
		got, ok := value.(json.Number)
		if !ok {
			return false
		}

		a, errA := got.Int64()
		b, errB := exp.Int64()

		return errA == nil && errB == nil && a == b
	}

	return false
}

// eventDoc - событие в виде дерева для поиска по пути ключа.
type eventDoc struct {
	root    map[string]any
	content map[string]any
}

func newEventDoc(ev Event) eventDoc {
	content := map[string]any{}

	if len(ev.Content) > 0 {
		dec := json.NewDecoder(bytes.NewReader(ev.Content))
		dec.UseNumber()

		var decoded map[string]any
		if dec.Decode(&decoded) == nil && decoded != nil {
			content = decoded
		}
	}

	root := map[string]any{
		"type":    ev.Type,
		"sender":  ev.Sender,
		"room_id": ev.RoomID,
		"content": content,
	}

	if ev.StateKey != nil {
		root["state_key"] = *ev.StateKey
	}

	return eventDoc{root: root, content: content}
}

func (d eventDoc) lookup(key string) any {
	value, _ := d.lookupExists(key)

	return value
}

// lookupExists идет по пути key: точки разделяют уровни, "\." - точка внутри
// имени поля, "\\" - обратная косая (спецификация v1.7, «dot-separated path»).
func (d eventDoc) lookupExists(key string) (any, bool) {
	var current any = d.root

	for _, part := range splitKey(key) {
		obj, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}

		current, ok = obj[part]
		if !ok {
			return nil, false
		}
	}

	return current, true
}

func splitKey(key string) []string {
	var (
		parts []string
		cur   strings.Builder
	)

	for i := 0; i < len(key); i++ {
		ch := key[i]

		switch {
		case ch == '\\' && i+1 < len(key) && (key[i+1] == '.' || key[i+1] == '\\'):
			cur.WriteByte(key[i+1])
			i++
		case ch == '.':
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(ch)
		}
	}

	return append(parts, cur.String())
}

// regexCache - скомпилированные шаблоны: правила вычисляются на каждом /sync,
// а шаблонов у пользователя немного.
var regexCache sync.Map

const regexCacheLimit = 4096

var regexCacheSize int64

var regexCacheMu sync.Mutex

func cachedRegexp(expr string) *regexp.Regexp {
	if cached, ok := regexCache.Load(expr); ok {
		re, _ := cached.(*regexp.Regexp)

		return re
	}

	re, err := regexp.Compile(expr)
	if err != nil {
		return nil
	}

	regexCacheMu.Lock()
	if regexCacheSize >= regexCacheLimit {
		regexCache.Range(func(key, _ any) bool {
			regexCache.Delete(key)

			return true
		})

		regexCacheSize = 0
	}

	regexCacheSize++
	regexCacheMu.Unlock()

	regexCache.Store(expr, re)

	return re
}

// globToRegexp переводит glob (* и ?) в регулярное выражение.
func globToRegexp(glob string) string {
	var b strings.Builder

	for _, r := range glob {
		switch r {
		case '*':
			b.WriteString(".*?")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}

	return b.String()
}

// globMatch - event_match: без учета регистра; для content.body шаблон ищется
// как отдельное слово, для остальных ключей - совпадение целиком.
func globMatch(pattern, value string, words bool) bool {
	expr := globToRegexp(pattern)
	if words {
		return wordMatchGlob(pattern, expr, value)
	}

	re := cachedRegexp(`(?is)\A` + expr + `\z`)

	return re != nil && re.MatchString(value)
}

func wordMatch(expr, value string) bool {
	return wordMatchGlob(expr, expr, value)
}

// wordMatchGlob ищет expr как слово. Граница слова - по буквам и цифрам Unicode:
// \b в Go только ASCII, и для кириллицы «вет» нашелся бы внутри «привет».
// Если шаблон начинается (заканчивается) не буквой, граница с этой стороны не
// нужна - как \b в Synapse для шаблонов вида "@room".
func wordMatchGlob(source, expr, value string) bool {
	prefix, suffix := "", ""

	if first, _ := utf8.DecodeRuneInString(source); isWordRune(first) {
		prefix = `(?:\A|[^\p{L}\p{N}_])`
	}

	if last, _ := utf8.DecodeLastRuneInString(source); isWordRune(last) {
		suffix = `(?:[^\p{L}\p{N}_]|\z)`
	}

	re := cachedRegexp(`(?is)` + prefix + `(?:` + expr + `)` + suffix)

	return re != nil && re.MatchString(value)
}

func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

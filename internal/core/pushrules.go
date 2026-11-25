package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/aisa-it/syngo/internal/pushrules"
)

// Push-правила (/pushrules). Логика перенесена из
// HTTP-обработчиков: транспорт разбирает путь и тело, сервис
// проверяет, сохраняет и обновляет снимок m.push_rules.

// AccountDataPushRules - тип account data, в котором клиент получает правила
// через /sync (спецификация). Element берет из него состояние уведомлений
// комнаты: без него значок «заглушено» не менялся до перезагрузки.
const AccountDataPushRules = "m.push_rules"

var (
	// ErrPushRuleAnchorNotFound - before/after ссылается на правило, которого нет
	// среди пользовательских правил этого вида.
	ErrPushRuleAnchorNotFound = errors.New("core: push rule before/after not found")
	// ErrNoStorage - сервис запущен без хранилища (503).
	ErrNoStorage = errors.New("core: storage is not configured")
)

// PushRule - сохраненное правило. Для серверных правил (id с точкой)
// строка хранит только переопределение enabled/actions, conditions = null;
// actions = null - действия серверного правила не менялись.
type PushRule struct {
	Scope      string
	Kind       string
	RuleID     string
	Priority   int
	Enabled    bool
	Conditions json.RawMessage
	Actions    json.RawMessage
	Pattern    string
}

// PushRulesStore - хранение push-правил и снимка m.push_rules в account data.
type PushRulesStore interface {
	// PushRules - правила пользователя области scope ("" - все) по приоритету.
	PushRules(ctx context.Context, userID, scope string) ([]PushRule, error)
	// PutUserPushRule создает или заменяет пользовательское правило и ставит его
	// по before/after; неизвестный якорь - ErrPushRuleAnchorNotFound.
	PutUserPushRule(ctx context.Context, userID string, rule PushRule, before, after string) error
	// SetUserPushRuleAttr меняет enabled/actions (nil - не менять); false - правила нет.
	SetUserPushRuleAttr(
		ctx context.Context, userID, scope, kind, ruleID string, enabled *bool, actions json.RawMessage,
	) (bool, error)
	// SetDefaultPushRuleAttr сохраняет переопределение серверного правила.
	SetDefaultPushRuleAttr(
		ctx context.Context, userID, scope, kind, ruleID string, defaultEnabled bool, enabled *bool, actions json.RawMessage,
	) error
	// DeletePushRule удаляет правило; false - его не было.
	DeletePushRule(ctx context.Context, userID, scope, kind, ruleID string) (bool, error)
	// SetAccountData пишет account data (снимок m.push_rules).
	SetAccountData(ctx context.Context, userID, roomID, eventType string, content []byte) error
}

// PushRulesHooks - реакции транспорта на изменение правил.
type PushRulesHooks struct {
	// Invalidate вызывается сразу после изменения правил, до записи снимка:
	// счетчики непрочитанного зависят от правил.
	Invalidate func(userID string)
	// Warn получает некритичный сбой: правило уже сохранено, ответ не меняется.
	Warn func(ctx context.Context, msg string, err error)
}

// PushRulesService - доменный сервис push-правил.
type PushRulesService struct {
	store PushRulesStore
	hooks PushRulesHooks
}

// NewPushRulesService создает сервис. store == nil - без хранилища: чтение
// отдает серверные правила, изменение - ErrNoStorage.
func NewPushRulesService(store PushRulesStore, hooks PushRulesHooks) *PushRulesService {
	return &PushRulesService{store: store, hooks: hooks}
}

// PushRuleRef - адрес правила /pushrules/{scope}/{kind}/{ruleId}.
type PushRuleRef struct {
	UserID, Scope, Kind, RuleID string
}

// CheckPushRuleRef проверяет scope, kind и rule_id. Раньше они не проверялись:
// правило с произвольным kind сохранялось и ломало ответ GET.
// Коды - как в Synapse.
func CheckPushRuleRef(ref PushRuleRef) error {
	switch {
	case ref.Scope != pushrules.ScopeGlobal:
		return requestError(ErrUnrecognized, "Unknown push rule scope")
	case !pushrules.ValidKind(ref.Kind):
		return requestError(ErrUnrecognized, "Unknown push rule kind")
	case ref.RuleID == "" || strings.ContainsAny(ref.RuleID, `/\`):
		return requestError(ErrInvalidParam, "rule_id may not be empty or contain slashes")
	default:
		return nil
	}
}

// BuildPushRules собирает итоговые правила из сохраненных строк (серверные с
// переопределениями и пользовательские).
func BuildPushRules(userID string, rows []PushRule) pushrules.Ruleset {
	out := make([]pushrules.StoredRule, 0, len(rows))

	for i := range rows {
		row := &rows[i]
		out = append(out, pushrules.StoredRule{
			Kind: row.Kind, RuleID: row.RuleID, Enabled: row.Enabled,
			Conditions: row.Conditions, Actions: row.Actions, Pattern: row.Pattern,
		})
	}

	return pushrules.Build(userID, out)
}

// Rules - итоговые правила пользователя.
func (s *PushRulesService) Rules(ctx context.Context, userID string) (pushrules.Ruleset, error) {
	if s.store == nil {
		return pushrules.Build(userID, nil), nil
	}

	rows, err := s.store.PushRules(ctx, userID, pushrules.ScopeGlobal)
	if err != nil {
		return pushrules.Ruleset{}, err
	}

	return BuildPushRules(userID, rows), nil
}

// Snapshot - content account data m.push_rules: {"global": правила}.
func (s *PushRulesService) Snapshot(ctx context.Context, userID string) (json.RawMessage, error) {
	rs, err := s.Rules(ctx, userID)
	if err != nil {
		return nil, err
	}

	return json.Marshal(map[string]any{"global": rs})
}

// Rule ищет правило в итоговом наборе (серверные тоже); нет - 404.
func (s *PushRulesService) Rule(ctx context.Context, ref PushRuleRef) (pushrules.Rule, error) {
	rs, err := s.Rules(ctx, ref.UserID)
	if err != nil {
		return pushrules.Rule{}, err
	}

	rule, ok := rs.Find(ref.Kind, ref.RuleID)
	if !ok {
		return pushrules.Rule{}, requestError(ErrNotFound, "Push rule not found")
	}

	return rule, nil
}

// PushRuleBody - тело PUT /pushrules/{scope}/{kind}/{ruleId}.
type PushRuleBody struct {
	Actions    json.RawMessage `json:"actions"`
	Conditions json.RawMessage `json:"conditions"`
	Pattern    *string         `json:"pattern"`
}

// CheckPushRulePut - проверки PUT правила до разбора тела: серверные правила
// целиком не заменяются (только /enabled и /actions), якорем не может быть
// серверное правило. before важнее after: при before значение after не учитывается.
func CheckPushRulePut(ref PushRuleRef, before, after string) error {
	if before != "" {
		after = ""
	}

	if pushrules.IsServerDefaultID(ref.RuleID) {
		return requestError(ErrInvalidParam, "Cannot add new rule_ids that start with '.'")
	}

	if pushrules.IsServerDefaultID(before) || pushrules.IsServerDefaultID(after) {
		return requestError(ErrInvalidParam, "before/after cannot be a server-default rule")
	}

	return nil
}

// Put создает или заменяет пользовательское правило и ставит его по before/after
// (before важнее after). Ref уже проверен CheckPushRuleRef.
func (s *PushRulesService) Put(ctx context.Context, ref PushRuleRef, before, after string, body PushRuleBody) error {
	if before != "" {
		after = ""
	}

	if err := CheckPushRulePut(ref, before, after); err != nil {
		return err
	}

	rule, err := buildUserPushRule(ref, body)
	if err != nil {
		return err
	}

	if s.store == nil {
		return ErrNoStorage
	}

	if err := s.store.PutUserPushRule(ctx, ref.UserID, rule, before, after); err != nil {
		if errors.Is(err, ErrPushRuleAnchorNotFound) {
			return requestError(ErrPushRuleAnchorNotFound, "before/after rule not found")
		}

		return err
	}

	s.changed(ctx, ref.UserID)

	return nil
}

// buildUserPushRule проверяет тело по виду правила. Новое правило включено
// (раньше сохранялось с enabled=false и не работало).
func buildUserPushRule(ref PushRuleRef, body PushRuleBody) (PushRule, error) {
	if body.Actions == nil {
		return PushRule{}, requestError(ErrInvalidParam, "Missing 'actions'")
	}

	if !pushrules.ValidateActions(body.Actions) {
		return PushRule{}, requestError(ErrInvalidParam, "Invalid 'actions'")
	}

	rule := PushRule{
		Scope: ref.Scope, Kind: ref.Kind, RuleID: ref.RuleID, Enabled: true,
		Conditions: json.RawMessage(`[]`), Actions: body.Actions,
	}

	switch ref.Kind {
	case pushrules.KindContent:
		if body.Pattern == nil || *body.Pattern == "" {
			return PushRule{}, requestError(ErrInvalidParam, "Missing 'pattern'")
		}

		rule.Pattern = *body.Pattern
	case pushrules.KindOverride, pushrules.KindUnderride:
		if body.Conditions != nil && string(body.Conditions) != "null" {
			var conditions []pushrules.Condition
			if err := json.Unmarshal(body.Conditions, &conditions); err != nil {
				return PushRule{}, requestError(ErrInvalidParam, "Invalid 'conditions'")
			}

			for _, cond := range conditions {
				if cond.Kind == "" {
					return PushRule{}, requestError(ErrInvalidParam, "Condition without 'kind'")
				}
			}

			rule.Conditions = body.Conditions
		}
	}

	return rule, nil
}

// SetEnabled - PUT .../enabled. Для серверных правил тоже: раньше отвечал 404,
// и Element не мог выключить уведомления.
func (s *PushRulesService) SetEnabled(ctx context.Context, ref PushRuleRef, enabled *bool) error {
	if enabled == nil {
		return requestError(ErrMissingParam, "Missing boolean 'enabled'")
	}

	return s.setAttr(ctx, ref, enabled, nil)
}

// SetActions - PUT .../actions: меняются только действия (раньше затирались
// условия и правило выключалось).
func (s *PushRulesService) SetActions(ctx context.Context, ref PushRuleRef, actions json.RawMessage) error {
	if !pushrules.ValidateActions(actions) {
		return requestError(ErrInvalidParam, "Invalid 'actions'")
	}

	return s.setAttr(ctx, ref, nil, actions)
}

// setAttr пишет атрибут серверного или пользовательского правила.
func (s *PushRulesService) setAttr(ctx context.Context, ref PushRuleRef, enabled *bool, actions json.RawMessage) error {
	if s.store == nil {
		return ErrNoStorage
	}

	if pushrules.IsServerDefaultID(ref.RuleID) {
		def, ok := pushrules.Defaults(ref.UserID).Find(ref.Kind, ref.RuleID)
		if !ok {
			return requestError(ErrNotFound, "Push rule not found")
		}

		if err := s.store.SetDefaultPushRuleAttr(ctx, ref.UserID, ref.Scope, ref.Kind, ref.RuleID,
			def.Enabled, enabled, actions); err != nil {
			return err
		}
	} else {
		found, err := s.store.SetUserPushRuleAttr(ctx, ref.UserID, ref.Scope, ref.Kind, ref.RuleID, enabled, actions)
		if err != nil {
			return err
		}

		if !found {
			return requestError(ErrNotFound, "Push rule not found")
		}
	}

	s.changed(ctx, ref.UserID)

	return nil
}

// Delete удаляет пользовательское правило; серверное удалить нельзя.
func (s *PushRulesService) Delete(ctx context.Context, ref PushRuleRef) error {
	if pushrules.IsServerDefaultID(ref.RuleID) {
		if _, isDefault := pushrules.Defaults(ref.UserID).Find(ref.Kind, ref.RuleID); isDefault {
			return requestError(ErrInvalidParam, "Cannot delete a server-default rule")
		}

		return requestError(ErrNotFound, "Push rule not found")
	}

	if s.store == nil {
		return ErrNoStorage
	}

	deleted, err := s.store.DeletePushRule(ctx, ref.UserID, ref.Scope, ref.Kind, ref.RuleID)
	if err != nil {
		return err
	}

	if !deleted {
		return requestError(ErrNotFound, "Push rule not found")
	}

	s.changed(ctx, ref.UserID)

	return nil
}

// changed кладет актуальные правила в account data m.push_rules: клиент узнает
// об изменении из account_data, как в Synapse. Ошибка не влияет на результат -
// правила уже сохранены.
func (s *PushRulesService) changed(ctx context.Context, userID string) {
	if s.hooks.Invalidate != nil {
		s.hooks.Invalidate(userID)
	}

	content, err := s.Snapshot(ctx, userID)
	if err != nil {
		s.warn(ctx, "reload push rules failed", err)

		return
	}

	if err := s.store.SetAccountData(ctx, userID, "", AccountDataPushRules, content); err != nil {
		s.warn(ctx, "push rules account data failed", err)
	}
}

// RefreshSnapshot подменяет сохраненный снимок m.push_rules в global актуальными
// правилами. Снимок пишется после каждого изменения, но Element меняет связанные
// правила параллельными PUT, и последним мог записаться снимок без одного из
// изменений; кроме того, набор серверных правил меняется с версией сервера.
// Synapse тоже собирает m.push_rules на лету. always - добавить
// m.push_rules, даже если снимка нет (initial sync).
func (s *PushRulesService) RefreshSnapshot(ctx context.Context, userID string, global map[string][]byte, always bool) {
	if _, ok := global[AccountDataPushRules]; !ok && !always {
		return
	}

	content, err := s.Snapshot(ctx, userID)
	if err != nil {
		s.warn(ctx, "push rules for account data failed", err)

		return
	}

	global[AccountDataPushRules] = content
}

func (s *PushRulesService) warn(ctx context.Context, msg string, err error) {
	if s.hooks.Warn != nil {
		s.hooks.Warn(ctx, msg, err)
	}
}

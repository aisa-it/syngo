package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aisa-it/syngo/internal/core"
)

// ErrPushRuleAnchorNotFound - before/after ссылается на правило, которого нет
// среди пользовательских правил этого вида. Ошибка и тип PushRule
// объявлены в core: store реализует core.PushRulesStore.
var ErrPushRuleAnchorNotFound = core.ErrPushRuleAnchorNotFound

// PushRule - строка push_rules.
type PushRule = core.PushRule

var _ core.PushRulesStore = (*DB)(nil)

// PushRules возвращает правила пользователя области scope ("" - все) в
// порядке приоритета внутри вида.
func (db *DB) PushRules(ctx context.Context, userID, scope string) ([]PushRule, error) {
	query := `SELECT scope, kind, rule_id, priority, enabled, conditions, actions, pattern
	          FROM push_rules WHERE user_id = $1`
	args := []any{userID}

	if scope != "" {
		query += ` AND scope = $2`
		args = append(args, scope)
	}

	query += ` ORDER BY scope, kind, priority, rule_id`

	rows, err := db.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: push rules: %w", err)
	}

	defer rows.Close()

	var out []PushRule

	for rows.Next() {
		var rule PushRule
		if err := rows.Scan(&rule.Scope, &rule.Kind, &rule.RuleID, &rule.Priority,
			&rule.Enabled, &rule.Conditions, &rule.Actions, &rule.Pattern); err != nil {
			return nil, fmt.Errorf("store: scan push rule: %w", err)
		}

		out = append(out, rule)
	}

	return out, rows.Err()
}

// PushRulesForUsers - правила нескольких пользователей одним запросом (для
// push-рассылки по участникам комнаты).
func (db *DB) PushRulesForUsers(ctx context.Context, userIDs []string) (map[string][]PushRule, error) {
	out := make(map[string][]PushRule, len(userIDs))
	if len(userIDs) == 0 {
		return out, nil
	}

	rows, err := db.pool.Query(ctx, `
		SELECT user_id, scope, kind, rule_id, priority, enabled, conditions, actions, pattern
		FROM push_rules WHERE user_id = ANY($1) AND scope = 'global'
		ORDER BY user_id, kind, priority, rule_id`, userIDs)
	if err != nil {
		return nil, fmt.Errorf("store: push rules for users: %w", err)
	}

	defer rows.Close()

	for rows.Next() {
		var (
			userID string
			rule   PushRule
		)

		if err := rows.Scan(&userID, &rule.Scope, &rule.Kind, &rule.RuleID, &rule.Priority,
			&rule.Enabled, &rule.Conditions, &rule.Actions, &rule.Pattern); err != nil {
			return nil, fmt.Errorf("store: scan push rule: %w", err)
		}

		out[userID] = append(out[userID], rule)
	}

	return out, rows.Err()
}

// PutUserPushRule создает или заменяет пользовательское правило и ставит его
// по before/after (спецификация). Без before/after правило становится первым
// в своем виде - как в Synapse. Раньше before/after игнорировались, а новое
// правило сохранялось выключенным.
func (db *DB) PutUserPushRule(ctx context.Context, userID string, rule PushRule, before, after string) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin push rule: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	// Перенумерация приоритетов - чтение и запись всего вида; параллельный PUT
	// того же пользователя в тот же вид иначе перемешал бы порядок.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('push_rules:' || $1 || ':' || $2))`,
		userID, rule.Kind); err != nil {
		return fmt.Errorf("store: lock push rules: %w", err)
	}

	rows, err := tx.Query(ctx, `
		SELECT rule_id FROM push_rules
		WHERE user_id = $1 AND scope = $2 AND kind = $3 AND rule_id <> $4 AND left(rule_id, 1) <> '.'
		ORDER BY priority, rule_id`, userID, rule.Scope, rule.Kind, rule.RuleID)
	if err != nil {
		return fmt.Errorf("store: list push rules: %w", err)
	}

	var order []string

	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()

			return fmt.Errorf("store: scan push rule id: %w", err)
		}

		order = append(order, id)
	}

	rows.Close()

	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: list push rules: %w", err)
	}

	position := 0

	if anchor := before + after; anchor != "" {
		position = -1

		for i, id := range order {
			if id == anchor {
				position = i

				break
			}
		}

		if position < 0 {
			return ErrPushRuleAnchorNotFound
		}

		if before == "" {
			position++
		}
	}

	order = append(order[:position], append([]string{rule.RuleID}, order[position:]...)...)

	if _, err := tx.Exec(ctx, `
		INSERT INTO push_rules (user_id, scope, kind, rule_id, priority, enabled, conditions, actions, pattern)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (user_id, scope, kind, rule_id) DO UPDATE
		SET enabled = EXCLUDED.enabled, conditions = EXCLUDED.conditions, actions = EXCLUDED.actions,
		    pattern = EXCLUDED.pattern, updated_at = now()`,
		userID, rule.Scope, rule.Kind, rule.RuleID, position, rule.Enabled,
		rule.Conditions, rule.Actions, rule.Pattern); err != nil {
		return fmt.Errorf("store: upsert push rule: %w", err)
	}

	priorities := make([]int, len(order))
	for i := range order {
		priorities[i] = i
	}

	if _, err := tx.Exec(ctx, `
		UPDATE push_rules p SET priority = o.priority
		FROM unnest($4::text[], $5::int[]) AS o(rule_id, priority)
		WHERE p.user_id = $1 AND p.scope = $2 AND p.kind = $3 AND p.rule_id = o.rule_id`,
		userID, rule.Scope, rule.Kind, order, priorities); err != nil {
		return fmt.Errorf("store: reorder push rules: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit push rule: %w", err)
	}

	return nil
}

// SetUserPushRuleAttr меняет enabled или actions пользовательского правила, не
// трогая условия (раньше PUT .../actions затирал conditions и выключал
// правило). enabled == nil - не менять, actions == nil - не менять.
// false - правила нет.
func (db *DB) SetUserPushRuleAttr(
	ctx context.Context, userID, scope, kind, ruleID string, enabled *bool, actions json.RawMessage,
) (bool, error) {
	tag, err := db.pool.Exec(ctx, `
		UPDATE push_rules
		SET enabled = coalesce($5, enabled), actions = coalesce($6::jsonb, actions), updated_at = now()
		WHERE user_id = $1 AND scope = $2 AND kind = $3 AND rule_id = $4`,
		userID, scope, kind, ruleID, enabled, nullableJSON(actions))
	if err != nil {
		return false, fmt.Errorf("store: set push rule attr: %w", err)
	}

	return tag.RowsAffected() > 0, nil
}

// SetDefaultPushRuleAttr сохраняет переопределение серверного правила: enabled
// (defaultEnabled - значение по умолчанию, если строки еще нет) и/или actions.
// Старая строка с тем же id (conditions не null) - мусор прежнего кода, ее
// actions не наследуются.
func (db *DB) SetDefaultPushRuleAttr(
	ctx context.Context, userID, scope, kind, ruleID string, defaultEnabled bool, enabled *bool, actions json.RawMessage,
) error {
	initialEnabled := defaultEnabled
	if enabled != nil {
		initialEnabled = *enabled
	}

	_, err := db.pool.Exec(ctx, `
		INSERT INTO push_rules (user_id, scope, kind, rule_id, priority, enabled, conditions, actions, pattern)
		VALUES ($1, $2, $3, $4, 0, $5, 'null'::jsonb, coalesce($7::jsonb, 'null'::jsonb), '')
		ON CONFLICT (user_id, scope, kind, rule_id) DO UPDATE
		SET enabled = CASE
		        WHEN $6::boolean IS NOT NULL THEN $6::boolean
		        WHEN push_rules.conditions = 'null'::jsonb THEN push_rules.enabled
		        ELSE $5 END,
		    actions = CASE
		        WHEN $7::jsonb IS NOT NULL THEN $7::jsonb
		        WHEN push_rules.conditions = 'null'::jsonb THEN push_rules.actions
		        ELSE 'null'::jsonb END,
		    conditions = 'null'::jsonb, pattern = '', updated_at = now()`,
		userID, scope, kind, ruleID, initialEnabled, enabled, nullableJSON(actions))
	if err != nil {
		return fmt.Errorf("store: set default push rule attr: %w", err)
	}

	return nil
}

func nullableJSON(raw json.RawMessage) any {
	if raw == nil {
		return nil
	}

	return string(raw)
}

// DeletePushRule удаляет правило; возвращает false, если его не было.
func (db *DB) DeletePushRule(ctx context.Context, userID, scope, kind, ruleID string) (bool, error) {
	tag, err := db.pool.Exec(ctx,
		`DELETE FROM push_rules WHERE user_id=$1 AND scope=$2 AND kind=$3 AND rule_id=$4`,
		userID, scope, kind, ruleID)
	if err != nil {
		return false, fmt.Errorf("store: delete push rule: %w", err)
	}

	return tag.RowsAffected() > 0, nil
}

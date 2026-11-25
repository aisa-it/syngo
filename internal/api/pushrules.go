package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/pushrules"
)

// Push-правила. Проверки и сохранение - в
// core.PushRulesService, здесь разбор запроса и ответ.

// loadPushRules собирает итоговые правила пользователя (серверные с
// переопределениями и пользовательские).
func (s *Server) loadPushRules(ctx context.Context, userID string) (pushrules.Ruleset, error) {
	return s.pushRules.Rules(ctx, userID)
}

// handleGetPushRules - GET /pushrules/ ({"global": ...}) и GET /pushrules/global/.
func (s *Server) handleGetPushRules(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	rs, err := s.loadPushRules(r.Context(), session.UserID)
	if err != nil {
		s.log.ErrorContext(r.Context(), "api: push rules failed", "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Internal server error")

		return
	}

	if scope := pathParamValue(r, "scope"); scope != "" {
		if scope != pushrules.ScopeGlobal {
			writeMatrixError(w, http.StatusBadRequest, ErrCodeUnrecognized, "Unknown push rule scope")

			return
		}

		writeJSON(w, http.StatusOK, rs)

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"global": rs})
}

// parsePushRulePath разбирает и проверяет путь /pushrules/{scope}/{kind}/{ruleId};
// false - ответ с ошибкой уже записан.
func parsePushRulePath(w http.ResponseWriter, r *http.Request) (core.PushRuleRef, bool) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return core.PushRuleRef{}, false
	}

	ref := core.PushRuleRef{
		UserID: session.UserID,
		Scope:  pathParamValue(r, "scope"),
		Kind:   pathParamValue(r, "kind"),
		RuleID: pathParamValue(r, "ruleId"),
	}

	if writeRequestError(w, core.CheckPushRuleRef(ref)) {
		return core.PushRuleRef{}, false
	}

	return ref, true
}

// findPushRule ищет правило в итоговом наборе; ответ с ошибкой уже записан, если false.
func (s *Server) findPushRule(w http.ResponseWriter, r *http.Request, ref core.PushRuleRef) (pushrules.Rule, bool) {
	rule, err := s.pushRules.Rule(r.Context(), ref)
	if err != nil {
		s.writePushRuleError(w, r, "api: push rules failed", err)

		return pushrules.Rule{}, false
	}

	return rule, true
}

// handleGetPushRule - GET /pushrules/{scope}/{kind}/{ruleId}. Серверные правила
// тоже находятся (раньше - 404).
func (s *Server) handleGetPushRule(w http.ResponseWriter, r *http.Request) {
	ref, ok := parsePushRulePath(w, r)
	if !ok {
		return
	}

	if rule, ok := s.findPushRule(w, r, ref); ok {
		writeJSON(w, http.StatusOK, rule)
	}
}

// handleGetPushRuleAttr - GET .../enabled и .../actions.
func (s *Server) handleGetPushRuleAttr(attr string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ref, ok := parsePushRulePath(w, r)
		if !ok {
			return
		}

		rule, ok := s.findPushRule(w, r, ref)
		if !ok {
			return
		}

		if attr == "enabled" {
			writeJSON(w, http.StatusOK, map[string]bool{"enabled": rule.Enabled})

			return
		}

		actions := rule.Actions
		if len(actions) == 0 {
			actions = json.RawMessage(`[]`)
		}

		writeJSON(w, http.StatusOK, map[string]json.RawMessage{"actions": actions})
	}
}

// handlePutPushRule - создание или замена пользовательского правила.
func (s *Server) handlePutPushRule(w http.ResponseWriter, r *http.Request) {
	ref, ok := parsePushRulePath(w, r)
	if !ok {
		return
	}

	query := r.URL.Query()
	before, after := query.Get("before"), query.Get("after")

	// Отказы, не зависящие от тела, - до его разбора (как раньше).
	if writeRequestError(w, core.CheckPushRulePut(ref, before, after)) {
		return
	}

	var body core.PushRuleBody
	if !decodeRoomBody(w, r, &body) {
		return
	}

	if err := s.pushRules.Put(r.Context(), ref, before, after, body); err != nil {
		s.writePushRuleError(w, r, "api: put push rule failed", err)

		return
	}

	s.notifySyncUser(ref.UserID)
	writeJSON(w, http.StatusOK, map[string]any{})
}

// handlePutPushRuleAttr - PUT .../enabled и .../actions.
func (s *Server) handlePutPushRuleAttr(attr string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ref, ok := parsePushRulePath(w, r)
		if !ok {
			return
		}

		var body struct {
			Enabled *bool           `json:"enabled"`
			Actions json.RawMessage `json:"actions"`
		}

		if !decodeRoomBody(w, r, &body) {
			return
		}

		var err error
		if attr == "enabled" {
			err = s.pushRules.SetEnabled(r.Context(), ref, body.Enabled)
		} else {
			err = s.pushRules.SetActions(r.Context(), ref, body.Actions)
		}

		if err != nil {
			s.writePushRuleError(w, r, "api: set push rule failed", err)

			return
		}

		s.notifySyncUser(ref.UserID)
		writeJSON(w, http.StatusOK, map[string]any{})
	}
}

// handleDeletePushRule - DELETE /pushrules/{scope}/{kind}/{ruleId}.
func (s *Server) handleDeletePushRule(w http.ResponseWriter, r *http.Request) {
	ref, ok := parsePushRulePath(w, r)
	if !ok {
		return
	}

	if err := s.pushRules.Delete(r.Context(), ref); err != nil {
		s.writePushRuleError(w, r, "api: delete push rule failed", err)

		return
	}

	s.notifySyncUser(ref.UserID)
	writeJSON(w, http.StatusOK, map[string]any{})
}

// writePushRuleError отвечает на ошибку сервиса push-правил.
func (s *Server) writePushRuleError(w http.ResponseWriter, r *http.Request, logMsg string, err error) {
	switch {
	case writeRequestError(w, err):
	case errors.Is(err, core.ErrNoStorage):
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")
	default:
		s.log.ErrorContext(r.Context(), logMsg, "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Internal server error")
	}
}

// pushRulesHooks - реакции HTTP-слоя на изменение правил. Сбрасывать нечего:
// действия правил вычисляются и хранятся в момент события, как в
// Synapse, - новые правила действуют на следующие события.
func (s *Server) pushRulesHooks() core.PushRulesHooks {
	return core.PushRulesHooks{
		Warn: func(ctx context.Context, msg string, err error) {
			s.log.WarnContext(ctx, "api: "+msg, "error", err)
		},
	}
}

// registerPushRulesRoutes подключает API push-правил.
func (s *Server) registerPushRulesRoutes(r chi.Router) {
	// GET /pushrules/ - пользовательские правила, поэтому тоже требует сессию:
	// без неё отдавались дефолтные правила «ни за кого».
	r.Group(func(r chi.Router) {
		r.Use(s.requireAuth)
		r.Get("/pushrules/", s.handleGetPushRules)
		r.Get("/pushrules/{scope}/", s.handleGetPushRules)

		const rule = "/pushrules/{scope}/{kind}/{ruleId}"

		r.Get(rule, s.handleGetPushRule)
		r.Put(rule, s.handlePutPushRule)
		r.Delete(rule, s.handleDeletePushRule)
		r.Get(rule+"/enabled", s.handleGetPushRuleAttr("enabled"))
		r.Put(rule+"/enabled", s.handlePutPushRuleAttr("enabled"))
		r.Get(rule+"/actions", s.handleGetPushRuleAttr("actions"))
		r.Put(rule+"/actions", s.handlePutPushRuleAttr("actions"))
	})
}

package api

import "github.com/go-chi/chi/v5"

// registerPusherRoutes подключает реестр pushers.
func (s *Server) registerPusherRoutes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(s.requireAuth)
		r.Get("/pushers", s.handleGetPushers)
		r.Post("/pushers/set", s.handleSetPusher)
	})
}

package api

import "github.com/go-chi/chi/v5"

// registerAccountDataRoutes подключает account data пользователя.
func (s *Server) registerAccountDataRoutes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(s.requireAuth)
		r.Get("/user/{userId}/account_data/{eventType}", s.handleGetAccountData)
		r.Put("/user/{userId}/account_data/{eventType}", s.handlePutAccountData)
		r.Get("/user/{userId}/rooms/{roomId}/account_data/{eventType}", s.handleGetRoomAccountData)
		r.Put("/user/{userId}/rooms/{roomId}/account_data/{eventType}", s.handlePutRoomAccountData)
		r.Delete("/user/{userId}/account_data/{eventType}", s.handleDeleteAccountData)
		r.Delete("/user/{userId}/rooms/{roomId}/account_data/{eventType}", s.handleDeleteRoomAccountData)
		r.Get("/user/{userId}/rooms/{roomId}/tags", s.handleGetTags)
		r.Put("/user/{userId}/rooms/{roomId}/tags/{tag}", s.handlePutTag)
		r.Delete("/user/{userId}/rooms/{roomId}/tags/{tag}", s.handleDeleteTag)
	})
}

package httpapi

import (
	"net/http"

	"postra/internal/application"
)

func (s *Server) registerWorkflowRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/action-cards", s.createActionCard)
}

func (s *Server) createActionCard(w http.ResponseWriter, r *http.Request) {
	input, err := decode[application.CreateActionCardInput](r)
	if err != nil {
		writeErr(w, err)
		return
	}
	card, err := s.app.CreateActionCard(r.Context(), input)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, card)
}

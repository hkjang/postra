package httpapi

import (
	"net/http"
	"strconv"
)

func (s *Server) listDrafts(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	result, err := s.app.ListDrafts(r.Context(), r.URL.Query().Get("status"), limit, r.URL.Query().Get("cursor"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

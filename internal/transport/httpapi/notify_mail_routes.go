package httpapi

import (
	"errors"
	"io"
	"net/http"
	"strconv"

	"postra/internal/domain"
)

// Relay notification log and test send (MAIL-STANDARD). Both are admin-only;
// the settings themselves live in the shared configuration endpoint.
func (s *Server) registerNotifyMailRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/admin/mail/deliveries", s.adminMailDeliveries)
	mux.HandleFunc("POST /api/admin/mail/test", s.adminSendTestMail)
}

func (s *Server) adminMailDeliveries(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	page, err := s.app.AdminListMailDeliveries(r.Context(), r.URL.Query().Get("status"), limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// adminSendTestMail sends one real message with the saved settings and
// reports the outcome in place, because a relay is rarely right first time.
func (s *Server) adminSendTestMail(w http.ResponseWriter, r *http.Request) {
	input, err := decode[struct {
		Recipient string `json:"recipient"`
	}](r)
	if err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, err)
		return
	}
	delivery, err := s.app.AdminSendTestMail(r.Context(), input.Recipient)
	if err != nil {
		writeErr(w, err)
		return
	}
	ok := delivery.Status == domain.MailDeliverySent
	message := "테스트 메일을 릴레이가 수락했습니다. 받은 편지함을 확인하세요."
	if !ok {
		message = "테스트 메일을 보내지 못했습니다: " + delivery.Error
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "message": message, "delivery": delivery})
}

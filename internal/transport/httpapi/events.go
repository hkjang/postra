package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"postra/internal/application"
	"postra/internal/domain"
)

func (s *Server) registerEventsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/events", s.events)
}

func (s *Server) eventPrincipal(r *http.Request) (domain.Principal, bool) {
	if !s.app.Cfg.Auth.Enabled && s.apiToken == "" {
		return s.localPrincipal(r)
	}
	return s.authenticate(r)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	// GET streams still need origin enforcement: a cross-site request must not
	// use ambient credentials to establish an ongoing channel to private data.
	if !sameOrigin(r, false) {
		writeErr(w, &domain.PublicError{Code: "forbidden", Message: "동일 출처 연결만 허용됩니다.", Status: 403})
		return
	}
	initial, ok := s.eventPrincipal(r)
	if !ok {
		writeErr(w, &domain.PublicError{Code: "authentication_required", Message: "로그인이 필요합니다.", Status: 401})
		return
	}
	r = r.WithContext(application.WithPrincipal(r.Context(), initial))
	snapshot, err := s.app.NotificationEvents(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	if r.URL.Query().Get("once") == "true" {
		writeJSON(w, http.StatusOK, snapshot)
		return
	}
	controller := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	writeEvent := func(name string, value any) bool {
		data, err := json.Marshal(value)
		if err != nil {
			return false
		}
		_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data); err != nil {
			return false
		}
		return controller.Flush() == nil
	}
	if !writeEvent("snapshot", snapshot) {
		return
	}
	previous, _ := json.Marshal(snapshot)
	lastSnapshot := time.Now()
	// A short authentication heartbeat is independent of notification cadence.
	// Expired/revoked sessions or keys cannot keep a long-lived authorized view.
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case now := <-ticker.C:
			checkCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			checkRequest := r.WithContext(checkCtx)
			principal, valid := s.eventPrincipal(checkRequest)
			if !valid || principal.UserID != initial.UserID || principal.MCPKeyID != initial.MCPKeyID {
				cancel()
				_, failure := application.PublicError(r.Context(), &domain.PublicError{Code: "authentication_required", Message: "세션이 만료되었습니다. 다시 로그인하세요.", Status: 401})
				writeEvent("session_expired", failure)
				return
			}
			checkCtx = application.WithPrincipal(checkCtx, principal)
			if principal.AuthMethod == "mcp_key" {
				if err := s.app.CheckMCPToolPolicy(checkCtx, "mail_events"); err != nil {
					cancel()
					_, failure := application.PublicError(r.Context(), err)
					writeEvent("stream_error", failure)
					return
				}
			}
			poll := s.app.NotificationPollSeconds()
			policyChanged := snapshot.Enabled != s.app.SettingBool("notifications.enabled") || snapshot.PollSeconds != poll
			if policyChanged || !now.Before(lastSnapshot.Add(time.Duration(poll)*time.Second)) {
				snapshot, err = s.app.NotificationEvents(checkCtx)
				if err != nil {
					cancel()
					_, failure := application.PublicError(r.Context(), err)
					writeEvent("stream_error", failure)
					return
				}
				data, _ := json.Marshal(snapshot)
				if !bytes.Equal(data, previous) {
					if !writeEvent("snapshot", snapshot) {
						cancel()
						return
					}
					previous = data
				}
				lastSnapshot = now
			}
			cancel()
			_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil || controller.Flush() != nil {
				return
			}
		}
	}
}

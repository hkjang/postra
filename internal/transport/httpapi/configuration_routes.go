package httpapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"postra/internal/application"
	"postra/internal/domain"
	"postra/internal/transport/mcpserver"
)

// Versioning changes routing only. Both paths use the same authorization,
// application services, limits and response contract (including errors).
func versionedAPI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1" || strings.HasPrefix(r.URL.Path, "/api/v1/") {
			cloned := r.Clone(r.Context())
			copyURL := *r.URL
			copyURL.Path = "/api" + strings.TrimPrefix(r.URL.Path, "/api/v1")
			copyURL.RawPath = ""
			cloned.URL = &copyURL
			r = cloned
		} else if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Deprecation", "true")
			w.Header().Set("Link", "</api/v1/>; rel=successor-version")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) registerConfigurationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/admin/configuration", s.configuration)
	mux.HandleFunc("PATCH /api/admin/configuration", s.configuration)
	mux.HandleFunc("POST /api/admin/configuration/test", s.probeConfiguration)
	mux.HandleFunc("GET /api/preferences", s.preferences)
	mux.HandleFunc("PATCH /api/preferences", s.preferences)
	mux.HandleFunc("GET /api/accounts/{id}/preferences", s.preferences)
	mux.HandleFunc("PATCH /api/accounts/{id}/preferences", s.preferences)
	mux.HandleFunc("GET /api/admin/operations", s.operations)
	mux.HandleFunc("GET /api/admin/mcp", s.mcpOperations)
	mux.HandleFunc("PATCH /api/mcp-keys/{id}", s.patchMCPKey)
	mux.HandleFunc("PATCH /api/admin/mcp-keys/{id}", s.patchMCPKey)
}

func (s *Server) probeConfiguration(w http.ResponseWriter, r *http.Request) {
	input, err := decode[application.SettingsProbe](r)
	if err != nil {
		writeErr(w, err)
		return
	}
	result, err := s.app.ProbeSettings(r.Context(), input)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) configuration(w http.ResponseWriter, r *http.Request) {
	var view application.SettingsView
	var err error
	if r.Method == http.MethodGet {
		view, err = s.app.AdminSettingsCatalog(r.Context())
	} else {
		patch, decodeErr := decode[application.SettingsPatch](r)
		if decodeErr != nil {
			writeErr(w, decodeErr)
			return
		}
		view, err = s.app.AdminPatchSettings(r.Context(), patch)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) preferences(w http.ResponseWriter, r *http.Request) {
	var view application.SettingsView
	var err error
	if r.Method == http.MethodGet {
		view, err = s.app.PersonalSettings(r.Context(), r.PathValue("id"))
	} else {
		patch, decodeErr := decode[application.SettingsPatch](r)
		if decodeErr != nil {
			writeErr(w, decodeErr)
			return
		}
		view, err = s.app.SavePersonalSettings(r.Context(), r.PathValue("id"), patch)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) patchMCPKey(w http.ResponseWriter, r *http.Request) {
	input, err := decode[struct {
		Scopes []string `json:"scopes"`
	}](r)
	if err != nil {
		writeErr(w, err)
		return
	}
	key, err := s.app.UpdateMCPKeyScopes(r.Context(), r.PathValue("id"), input.Scopes, strings.HasPrefix(r.URL.Path, "/api/admin/"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, key)
}

func (s *Server) operations(w http.ResponseWriter, r *http.Request) {
	// This endpoint is a status inventory, never an implicit call to an
	// external AI/mail/IdP service. Explicit tests are separate operations.
	if _, err := s.app.AdminSettingsCatalog(r.Context()); err != nil {
		writeErr(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	database := "healthy"
	if s.app.Ready(ctx) != nil {
		database = "unavailable"
	}
	ai := "unconfigured"
	if s.app.Setting("ai.base_url") != "" && s.app.Setting("ai.model") != "" {
		ai = "configured"
	}
	mcp := "disabled"
	if s.app.SettingBool("mcp.enabled") {
		mcp = "enabled"
	}
	oidc := "unconfigured"
	if s.app.OIDCConfigured(ctx) {
		oidc = "configured"
	}
	mail := "disabled"
	if s.app.SettingBool("mail.imap_enabled") || s.app.SettingBool("mail.pop3_enabled") {
		mail = "enabled"
	}
	writeJSON(w, http.StatusOK, map[string]any{"database": database, "ai": ai, "mcp": mcp, "oidc": oidc, "mail": mail, "checked_at": time.Now().UTC(), "message": "연결 테스트 전에는 configured/enabled를 정상 연결로 간주하지 않습니다."})
}

func (s *Server) mcpOperations(w http.ResponseWriter, r *http.Request) {
	if _, err := s.app.AdminSettingsCatalog(r.Context()); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mcpserver.Capabilities(r.Context(), s.app))
}

// A key cannot evade its scope/gateway policy by switching MCP to REST.
// Unknown REST capabilities and credential/security mutation are deny-by-default.
func (s *Server) checkMCPRESTScope(ctx context.Context, route string) error {
	tool := map[string]string{
		"GET /api/messages/{id}/body/frame":    "mail_message_get",
		"POST /api/messages/{id}/images/allow": "mail_set_work_status", "DELETE /api/messages/{id}/images/allow": "mail_set_work_status", "POST /api/action-cards": "mail_action_card_create",
		"GET /api/events": "mail_events", "POST /api/mail/rewrite-selection": "mail_text_rewrite",
		"POST /api/drafts/{id}/attachments": "mail_draft_attachment_add", "GET /api/drafts/{id}/attachments/{attachment}": "mail_draft_attachment_get", "DELETE /api/drafts/{id}/attachments/{attachment}": "mail_draft_attachment_remove",
		"GET /api/messages/{id}/auth": "mail_auth_inspect", "GET /api/messages/{id}/collab": "mail_collab_get", "POST /api/messages/{id}/sla": "mail_set_sla",
		"GET /api/action-cards/{id}": "mail_action_cards_list", "POST /api/rules/draft": "mail_rule_draft_from_text", "GET /api/mail/templates": "mail_templates",
		"POST /api/signatures": "mail_signature_save", "PUT /api/signatures/{id}": "mail_signature_save", "DELETE /api/signatures/{id}": "mail_signature_delete", "GET /api/audit": "mail_audit_search",
		"GET /api/me": "mail_identity", "GET /api/system/info": "mail_system_info",
		"GET /api/accounts": "mail_account_list", "GET /api/accounts/{id}": "mail_account_get",
		"POST /api/accounts/{id}/test": "mail_account_test", "POST /api/accounts/{id}/sync": "mail_sync_start",
		"GET /api/jobs": "job_list", "GET /api/jobs/{id}": "job_status", "POST /api/jobs/{id}/cancel": "job_cancel",
		"GET /api/messages": "mail_search", "GET /api/messages/{id}": "mail_message_get", "GET /api/messages/{id}/raw": "mail_message_get",
		"GET /api/messages/{id}/attachments": "mail_attachment_list", "GET /api/messages/{id}/attachments/{att}": "mail_attachment_list",
		"GET /api/messages/{id}/attachments/{att}/text": "mail_attachment_extract_text", "POST /api/messages/{id}/attachments/{att}/summarize": "mail_attachment_summarize",
		"POST /api/messages/{id}/analyze": "mail_summarize", "POST /api/messages/{id}/suggest-replies": "mail_suggest_replies",
		"POST /api/messages/{id}/calendar": "mail_calendar_extract", "GET /api/messages/{id}/calendar.ics": "mail_calendar_extract",
		"GET /api/threads/{id}": "mail_thread_get", "GET /api/threads/{id}/timeline": "mail_thread_timeline", "POST /api/threads/{id}/summarize": "mail_thread_summarize",
		"DELETE /api/messages/{id}": "mail_local_delete", "POST /api/messages/batch": "mail_batch_update",
		"POST /api/qa": "mail_question_answer", "POST /api/digest": "mail_daily_digest", "POST /api/eval": "mail_eval_prompt",
		"POST /api/semantic-search": "mail_semantic_search", "POST /api/hybrid-search": "mail_hybrid_search", "POST /api/embeddings/build": "mail_embeddings_build",
		"GET /api/drafts": "mail_draft_get", "POST /api/drafts": "mail_draft_create", "GET /api/drafts/{id}": "mail_draft_get", "PATCH /api/drafts/{id}": "mail_draft_update", "DELETE /api/drafts/{id}": "mail_draft_delete",
		"POST /api/drafts/{id}/rewrite": "mail_draft_rewrite", "POST /api/drafts/{id}/preview": "mail_send_preview", "POST /api/drafts/{id}/request-approval": "mail_send_request_approval", "POST /api/drafts/{id}/send": "mail_send",
		"GET /api/outbound": "mail_outbound_list", "GET /api/outbound/{id}": "mail_outbound_status",
		"POST /api/mail/render": "mail_render", "GET /api/signatures": "mail_signature_list", "GET /api/signatures/{id}": "mail_signature_get",
		"GET /api/work-inbox": "mail_work_inbox", "GET /api/team-inbox": "mail_team_inbox", "GET /api/action-cards": "mail_action_cards_list",
		"GET /api/rules": "mail_rules_list", "GET /api/rules/{id}": "mail_rules_list", "POST /api/rules": "mail_rule_create", "PUT /api/rules/{id}": "mail_rule_update", "DELETE /api/rules/{id}": "mail_rule_delete",
		"POST /api/messages/{id}/apply-rules": "mail_apply_rules", "POST /api/messages/{id}/assign": "mail_assign", "POST /api/messages/{id}/work-status": "mail_set_work_status", "POST /api/messages/{id}/notes": "mail_add_note",
		"POST /api/messages/{id}/action-cards": "mail_action_cards_extract", "POST /api/action-cards/{id}/status": "mail_action_card_set_status", "POST /api/action-cards/{id}/export": "mail_action_card_export",
	}[route]
	if tool == "" {
		return &domain.PublicError{Code: "insufficient_scope", Message: "MCP 키로 이 REST 기능을 사용할 수 없습니다.", Status: 403}
	}
	return s.app.CheckMCPToolPolicy(ctx, tool)
}

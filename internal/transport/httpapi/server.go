// Package httpapi is the REST transport. Handlers are thin: decode, call
// the application use case, encode — no business logic here (§16).
package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"postra/internal/application"
	"postra/internal/domain"
	"postra/internal/platform/metrics"
)

type Server struct {
	app      *application.App
	apiToken string
}

func New(app *application.App, apiToken string) *Server {
	return &Server{app: app, apiToken: apiToken}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/me", s.me)
	mux.HandleFunc("GET /api/admin/users", s.adminListUsers)
	mux.HandleFunc("POST /api/admin/users", s.adminCreateUser)
	mux.HandleFunc("PATCH /api/admin/users/{id}", s.adminUpdateUser)
	mux.HandleFunc("DELETE /api/admin/users/{id}", s.adminDeleteUser)
	mux.HandleFunc("POST /api/admin/users/{id}/password", s.adminResetPassword)
	mux.HandleFunc("GET /api/admin/settings", s.adminGetSettings)
	mux.HandleFunc("PATCH /api/admin/settings", s.adminSaveSettings)
	mux.HandleFunc("PUT /api/admin/ai", s.adminSaveAI)
	mux.HandleFunc("POST /api/admin/ai/test", s.adminTestAI)

	mux.HandleFunc("POST /api/secrets", s.postSecret)
	mux.HandleFunc("POST /api/secrets/{ref}/rotate", s.rotateSecret)
	mux.HandleFunc("DELETE /api/secrets/{ref}", s.revokeSecret)

	mux.HandleFunc("POST /api/accounts", s.createAccount)
	mux.HandleFunc("GET /api/accounts", s.listAccounts)
	mux.HandleFunc("GET /api/accounts/{id}", s.getAccount)
	mux.HandleFunc("PATCH /api/accounts/{id}", s.updateAccount)
	mux.HandleFunc("DELETE /api/accounts/{id}", s.deleteAccount)
	mux.HandleFunc("POST /api/accounts/{id}/test", s.testAccount)
	mux.HandleFunc("POST /api/accounts/{id}/disable", s.disableAccount)
	mux.HandleFunc("POST /api/accounts/{id}/sync", s.startSync)

	mux.HandleFunc("GET /api/jobs/{id}", s.getJob)
	mux.HandleFunc("POST /api/jobs/{id}/cancel", s.cancelJob)

	mux.HandleFunc("GET /api/messages", s.search)
	mux.HandleFunc("GET /api/messages/{id}", s.getMessage)
	mux.HandleFunc("GET /api/messages/{id}/raw", s.getRaw)
	mux.HandleFunc("GET /api/messages/{id}/attachments", s.listAttachments)
	mux.HandleFunc("GET /api/messages/{id}/attachments/{att}", s.getAttachment)
	mux.HandleFunc("GET /api/threads/{id}", s.getThread)
	mux.HandleFunc("DELETE /api/messages/{id}", s.localDelete)
	mux.HandleFunc("POST /api/accounts/{id}/server-delete/preview", s.serverDeletePreview)
	mux.HandleFunc("POST /api/accounts/{id}/server-delete/request-approval", s.serverDeleteApproval)
	mux.HandleFunc("POST /api/accounts/{id}/server-delete", s.serverDelete)

	mux.HandleFunc("POST /api/messages/{id}/analyze", s.analyzeMessage)
	mux.HandleFunc("POST /api/threads/{id}/summarize", s.summarizeThread)
	mux.HandleFunc("POST /api/qa", s.questionAnswer)
	mux.HandleFunc("POST /api/embeddings/build", s.buildEmbeddings)
	mux.HandleFunc("POST /api/semantic-search", s.semanticSearch)

	mux.HandleFunc("POST /api/drafts", s.createDraft)
	mux.HandleFunc("GET /api/drafts/{id}", s.getDraft)
	mux.HandleFunc("PATCH /api/drafts/{id}", s.updateDraft)
	mux.HandleFunc("DELETE /api/drafts/{id}", s.deleteDraft)
	mux.HandleFunc("POST /api/drafts/{id}/rewrite", s.rewriteDraft)
	mux.HandleFunc("POST /api/drafts/{id}/preview", s.previewSend)
	mux.HandleFunc("POST /api/drafts/{id}/request-approval", s.requestApproval)
	mux.HandleFunc("POST /api/drafts/{id}/send", s.send)
	mux.HandleFunc("GET /api/outbound/{id}", s.getOutbound)

	mux.HandleFunc("GET /api/audit", s.audit)
	// Probes (unauthenticated): livez = process is up; readyz/healthz = backing
	// store reachable, 503 otherwise. healthz keeps the readiness meaning it
	// had documented while livez separates pure liveness for orchestrators.
	mux.HandleFunc("GET /api/livez", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/readyz", s.readyz)
	mux.HandleFunc("GET /api/healthz", s.readyz)

	// Prometheus scrape endpoint (§18.1): unauthenticated so scrapers need no
	// token; gated by config and by whatever the operator binds HTTPAddr to.
	if s.app.Cfg.MetricsEnabled {
		mux.Handle("GET /metrics", metrics.Handler())
	}

	return s.middleware(mux)
}

// ---------- identity and administration ----------

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	p, ok := application.PrincipalFrom(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not authenticated"})
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) adminListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.app.AdminListUsers(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, users)
}

func (s *Server) adminCreateUser(w http.ResponseWriter, r *http.Request) {
	var in application.CreateUserInput
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		writeErr(w, err)
		return
	}
	u, err := s.app.AdminCreateUser(r.Context(), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

func (s *Server) adminUpdateUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DisplayName string            `json:"display_name"`
		Email       string            `json:"email"`
		Role        domain.UserRole   `json:"role"`
		Status      domain.UserStatus `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		writeErr(w, err)
		return
	}
	u, err := s.app.AdminEditUser(r.Context(), r.PathValue("id"), in.DisplayName, in.Email, in.Role, in.Status)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) adminDeleteUser(w http.ResponseWriter, r *http.Request) {
	if err := s.app.AdminDeleteUser(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) adminResetPassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.app.AdminResetPassword(r.Context(), r.PathValue("id"), in.Password); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) adminGetSettings(w http.ResponseWriter, r *http.Request) {
	if p, ok := application.PrincipalFrom(r.Context()); !ok || !p.IsAdmin() {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "administrator permission required"})
		return
	}
	settings, err := s.app.SystemSettings(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) adminSaveSettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Values           map[string]string `json:"values"`
		OIDCClientSecret string            `json:"oidc_client_secret"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.app.AdminSaveSettings(r.Context(), in.Values, in.OIDCClientSecret); err != nil {
		writeErr(w, err)
		return
	}
	settings, err := s.app.SystemSettings(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) adminSaveAI(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Values map[string]string `json:"values"`
		APIKey string            `json:"api_key"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.app.AdminSaveAISettings(r.Context(), in.Values, in.APIKey); err != nil {
		writeErr(w, err)
		return
	}
	settings, err := s.app.SystemSettings(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) adminTestAI(w http.ResponseWriter, r *http.Request) {
	result, err := s.app.AdminTestAI(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// statusRecorder captures the response status code for request metrics.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

// readyz reports 200 when the backing store is reachable, 503 otherwise.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.app.Ready(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// publicPaths are reachable without the API token: scrape and probe endpoints
// that reveal no sensitive data.
func publicPath(p string) bool {
	switch p {
	case "/api/livez", "/api/readyz", "/api/healthz":
		return true
	}
	return false
}

func (s *Server) middleware(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /metrics bypasses auth and is not self-instrumented.
		if r.URL.Path == "/metrics" {
			mux.ServeHTTP(w, r)
			return
		}
		// Label by the matched route pattern (e.g. "GET /api/messages/{id}")
		// so path parameters do not explode series cardinality.
		route := "unmatched"
		if _, pattern := mux.Handler(r); pattern != "" {
			route = pattern
		}
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		start := time.Now()
		func() {
			ctx := application.WithActor(r.Context(), "rest")
			if !publicPath(r.URL.Path) && (s.app.Cfg.Auth.Enabled || s.apiToken != "") {
				principal, ok := s.authenticate(r)
				if !ok {
					writeJSON(rec, http.StatusUnauthorized, map[string]string{"error": "invalid or missing bearer token"})
					return
				}
				ctx = application.WithPrincipal(ctx, principal)
			}
			mux.ServeHTTP(rec, r.WithContext(ctx))
		}()
		metrics.HTTPRequests.WithLabelValues(route, r.Method, strconv.Itoa(rec.code)).Inc()
		metrics.HTTPLatency.WithLabelValues(route).Observe(time.Since(start).Seconds())
	})
}

func (s *Server) authenticate(r *http.Request) (domain.Principal, bool) {
	raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if s.apiToken != "" && subtle.ConstantTimeCompare([]byte(raw), []byte(s.apiToken)) == 1 {
		u, err := s.app.Store.GetUser(r.Context(), application.DefaultUserID)
		if err == nil {
			return domain.Principal{UserID: u.ID, LoginID: u.LoginID, DisplayName: u.DisplayName,
				Role: domain.RoleAdmin, AuthMethod: "api_token"}, true
		}
	}
	if raw != "" {
		if p, err := s.app.AuthenticateOIDCAccessToken(r.Context(), raw); err == nil {
			return p, true
		}
	}
	if c, err := r.Cookie("postra_session"); err == nil {
		if _, p, err := s.app.AuthenticateSession(r.Context(), c.Value); err == nil {
			return p, true
		}
	}
	return domain.Principal{}, false
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	var ue *application.UserError
	switch {
	case errors.As(err, &ue):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": ue.Msg})
	case errors.Is(err, domain.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	default:
		slog.Error("request failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
}

func decode[T any](r *http.Request) (T, error) {
	var v T
	err := json.NewDecoder(io.LimitReader(r.Body, 10<<20)).Decode(&v)
	return v, err
}

// ---------- secrets ----------

type secretBody struct {
	Type  string `json:"type"` // mail_password | api_key
	Label string `json:"label"`
	Value string `json:"value"`
}

// postSecret is the REST leg of the secure secret-registration flow (§11.4).
// The value goes straight into the secret store; it is never logged and this
// endpoint is deliberately absent from the MCP tool surface.
func (s *Server) postSecret(w http.ResponseWriter, r *http.Request) {
	in, err := decode[secretBody](r)
	if err != nil {
		writeErr(w, err)
		return
	}
	t := domain.SecretType(in.Type)
	if t != domain.SecretMailPassword && t != domain.SecretAPIKey {
		writeErr(w, errors.New("type must be mail_password or api_key"))
		return
	}
	handle := domain.NewSecretHandle([]byte(in.Value))
	in.Value = ""
	ref, err := s.app.RegisterSecret(r.Context(), t, in.Label, handle)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"secret_ref": string(ref)})
}

func (s *Server) rotateSecret(w http.ResponseWriter, r *http.Request) {
	in, err := decode[secretBody](r)
	if err != nil {
		writeErr(w, err)
		return
	}
	handle := domain.NewSecretHandle([]byte(in.Value))
	in.Value = ""
	if err := s.app.RotateSecret(r.Context(), domain.SecretRef(r.PathValue("ref")), handle); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "rotated"})
}

func (s *Server) revokeSecret(w http.ResponseWriter, r *http.Request) {
	if err := s.app.RevokeSecret(r.Context(), domain.SecretRef(r.PathValue("ref"))); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// ---------- accounts ----------

func (s *Server) createAccount(w http.ResponseWriter, r *http.Request) {
	in, err := decode[application.CreateAccountInput](r)
	if err != nil {
		writeErr(w, err)
		return
	}
	acc, err := s.app.CreateAccount(r.Context(), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, acc)
}

func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) {
	accs, err := s.app.ListAccounts(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, accs)
}

func (s *Server) getAccount(w http.ResponseWriter, r *http.Request) {
	acc, err := s.app.GetAccount(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, acc)
}

func (s *Server) updateAccount(w http.ResponseWriter, r *http.Request) {
	in, err := decode[application.UpdateAccountInput](r)
	if err != nil {
		writeErr(w, err)
		return
	}
	in.AccountID = r.PathValue("id")
	acc, err := s.app.UpdateAccount(r.Context(), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, acc)
}

func (s *Server) testAccount(w http.ResponseWriter, r *http.Request) {
	diags, err := s.app.TestAccount(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, diags)
}

func (s *Server) disableAccount(w http.ResponseWriter, r *http.Request) {
	if err := s.app.DisableAccount(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "disabled"})
}

func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	if err := s.app.DeleteAccount(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- sync & jobs ----------

func (s *Server) startSync(w http.ResponseWriter, r *http.Request) {
	var opts application.SyncOptions
	if r.ContentLength > 0 {
		opts, _ = decode[application.SyncOptions](r)
	}
	job, err := s.app.StartSync(r.Context(), r.PathValue("id"), opts)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	job, err := s.app.GetJob(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	if err := s.app.CancelJob(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelling"})
}

// ---------- messages ----------

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sq := domain.SearchQuery{
		AccountID: q.Get("account_id"), Text: q.Get("q"),
		From: q.Get("from"), To: q.Get("to"), Subject: q.Get("subject"),
		Cursor: q.Get("cursor"),
	}
	sq.Since, _ = strconv.ParseInt(q.Get("since"), 10, 64)
	sq.Until, _ = strconv.ParseInt(q.Get("until"), 10, 64)
	sq.Limit, _ = strconv.Atoi(q.Get("limit"))
	if v := q.Get("has_attachment"); v != "" {
		b := v == "true" || v == "1"
		sq.HasAttachment = &b
	}
	res, err := s.app.Search(r.Context(), sq)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) getMessage(w http.ResponseWriter, r *http.Request) {
	includeBody := r.URL.Query().Get("body") != "false"
	var mv *application.MessageView
	var err error
	if r.URL.Query().Get("mask") == "true" {
		mv, err = s.app.GetMessageMasked(r.Context(), r.PathValue("id"), includeBody)
	} else {
		mv, err = s.app.GetMessage(r.Context(), r.PathValue("id"), includeBody)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mv)
}

func (s *Server) getRaw(w http.ResponseWriter, r *http.Request) {
	rc, err := s.app.GetRawMessage(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "message/rfc822")
	io.Copy(w, rc)
}

func (s *Server) listAttachments(w http.ResponseWriter, r *http.Request) {
	atts, err := s.app.ListAttachments(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, atts)
}

func (s *Server) getAttachment(w http.ResponseWriter, r *http.Request) {
	ack := r.URL.Query().Get("ack") == "true"
	at, rc, err := s.app.GetAttachment(r.Context(), r.PathValue("id"), r.PathValue("att"), ack)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+at.Name+`"`)
	io.Copy(w, rc)
}

func (s *Server) localDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.app.LocalDelete(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) serverDeletePreview(w http.ResponseWriter, r *http.Request) {
	pv, err := s.app.ServerDeletePreview(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pv)
}

func (s *Server) serverDeleteApproval(w http.ResponseWriter, r *http.Request) {
	in, _ := decode[struct {
		Approver   string `json:"approver"`
		TTLSeconds int    `json:"ttl_seconds"`
	}](r)
	pv, tok, err := s.app.RequestServerDeleteApproval(r.Context(), r.PathValue("id"), in.Approver, in.TTLSeconds)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"preview": pv, "approval": tok})
}

func (s *Server) serverDelete(w http.ResponseWriter, r *http.Request) {
	in, err := decode[struct {
		UIDLs         []string `json:"uidls"`
		ApprovalToken string   `json:"approval_token"`
	}](r)
	if err != nil {
		writeErr(w, err)
		return
	}
	res, err := s.app.ServerDelete(r.Context(), r.PathValue("id"), in.UIDLs, in.ApprovalToken)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) getThread(w http.ResponseWriter, r *http.Request) {
	tv, err := s.app.GetThread(r.Context(), r.PathValue("id"), r.URL.Query().Get("bodies") == "true")
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tv)
}

// ---------- AI ----------

func (s *Server) analyzeMessage(w http.ResponseWriter, r *http.Request) {
	in, err := decode[struct {
		Type string `json:"type"`
	}](r)
	if err != nil {
		writeErr(w, err)
		return
	}
	an, err := s.app.AnalyzeMessage(r.Context(), r.PathValue("id"), in.Type)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, an)
}

func (s *Server) summarizeThread(w http.ResponseWriter, r *http.Request) {
	an, err := s.app.SummarizeThread(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, an)
}

func (s *Server) questionAnswer(w http.ResponseWriter, r *http.Request) {
	in, err := decode[struct {
		Question  string `json:"question"`
		AccountID string `json:"account_id"`
	}](r)
	if err != nil {
		writeErr(w, err)
		return
	}
	an, err := s.app.AnswerQuestion(r.Context(), in.Question, in.AccountID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, an)
}

func (s *Server) buildEmbeddings(w http.ResponseWriter, r *http.Request) {
	in, _ := decode[struct {
		AccountID string `json:"account_id"`
		Max       int    `json:"max"`
	}](r)
	job, err := s.app.BuildEmbeddings(r.Context(), in.AccountID, in.Max)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) semanticSearch(w http.ResponseWriter, r *http.Request) {
	in, err := decode[struct {
		Query     string `json:"query"`
		AccountID string `json:"account_id"`
		Limit     int    `json:"limit"`
	}](r)
	if err != nil {
		writeErr(w, err)
		return
	}
	hits, err := s.app.SemanticSearch(r.Context(), in.Query, in.AccountID, in.Limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": hits})
}

// ---------- drafts & send ----------

func (s *Server) createDraft(w http.ResponseWriter, r *http.Request) {
	in, err := decode[application.CreateDraftInput](r)
	if err != nil {
		writeErr(w, err)
		return
	}
	dv, err := s.app.CreateDraft(r.Context(), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, dv)
}

func (s *Server) getDraft(w http.ResponseWriter, r *http.Request) {
	dv, err := s.app.GetDraft(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, dv)
}

func (s *Server) updateDraft(w http.ResponseWriter, r *http.Request) {
	in, err := decode[application.UpdateDraftInput](r)
	if err != nil {
		writeErr(w, err)
		return
	}
	in.DraftID = r.PathValue("id")
	dv, err := s.app.UpdateDraft(r.Context(), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, dv)
}

func (s *Server) deleteDraft(w http.ResponseWriter, r *http.Request) {
	if err := s.app.DiscardDraft(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) rewriteDraft(w http.ResponseWriter, r *http.Request) {
	in, err := decode[struct {
		Style string `json:"style"`
	}](r)
	if err != nil {
		writeErr(w, err)
		return
	}
	dv, err := s.app.RewriteDraft(r.Context(), r.PathValue("id"), in.Style)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, dv)
}

func (s *Server) previewSend(w http.ResponseWriter, r *http.Request) {
	p, err := s.app.PreviewSend(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) requestApproval(w http.ResponseWriter, r *http.Request) {
	in, _ := decode[struct {
		Approver   string `json:"approver"`
		TTLSeconds int    `json:"ttl_seconds"`
	}](r)
	preview, tok, err := s.app.RequestSendApproval(r.Context(), r.PathValue("id"), in.Approver, in.TTLSeconds)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"preview": preview, "approval": tok})
}

func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	in, err := decode[application.SendInput](r)
	if err != nil {
		writeErr(w, err)
		return
	}
	in.DraftID = r.PathValue("id")
	out, err := s.app.Send(r.Context(), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getOutbound(w http.ResponseWriter, r *http.Request) {
	out, err := s.app.GetOutbound(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) audit(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	evs, err := s.app.SearchAudit(r.Context(), limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, evs)
}

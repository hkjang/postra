// Package httpapi is the REST transport. Handlers are thin: decode, call
// the application use case, encode — no business logic here (§16).
package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"postra/internal/adapters/persistence"
	"postra/internal/application"
	"postra/internal/domain"
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

	mux.HandleFunc("POST /api/secrets", s.postSecret)
	mux.HandleFunc("POST /api/secrets/{ref}/rotate", s.rotateSecret)
	mux.HandleFunc("DELETE /api/secrets/{ref}", s.revokeSecret)

	mux.HandleFunc("POST /api/accounts", s.createAccount)
	mux.HandleFunc("GET /api/accounts", s.listAccounts)
	mux.HandleFunc("GET /api/accounts/{id}", s.getAccount)
	mux.HandleFunc("PATCH /api/accounts/{id}", s.updateAccount)
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

	mux.HandleFunc("POST /api/drafts", s.createDraft)
	mux.HandleFunc("GET /api/drafts/{id}", s.getDraft)
	mux.HandleFunc("PATCH /api/drafts/{id}", s.updateDraft)
	mux.HandleFunc("POST /api/drafts/{id}/rewrite", s.rewriteDraft)
	mux.HandleFunc("POST /api/drafts/{id}/preview", s.previewSend)
	mux.HandleFunc("POST /api/drafts/{id}/request-approval", s.requestApproval)
	mux.HandleFunc("POST /api/drafts/{id}/send", s.send)
	mux.HandleFunc("GET /api/outbound/{id}", s.getOutbound)

	mux.HandleFunc("GET /api/audit", s.audit)
	mux.HandleFunc("GET /api/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	return s.middleware(mux)
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.apiToken != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.apiToken)) != 1 {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or missing bearer token"})
				return
			}
		}
		ctx := application.WithActor(r.Context(), "rest")
		next.ServeHTTP(w, r.WithContext(ctx))
	})
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
	case errors.Is(err, persistence.ErrNotFound):
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
	mv, err := s.app.GetMessage(r.Context(), r.PathValue("id"), r.URL.Query().Get("body") != "false")
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
	at, rc, err := s.app.GetAttachment(r.Context(), r.PathValue("id"), r.PathValue("att"))
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

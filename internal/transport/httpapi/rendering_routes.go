package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"postra/internal/application"
	"postra/internal/domain"
	"postra/internal/mailrender"
)

func (s *Server) registerRenderingRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/mail/templates", s.mailTemplates)
	mux.HandleFunc("POST /api/mail/render", s.renderMail)
	mux.HandleFunc("POST /api/mail/rewrite-selection", s.rewriteMailText)
	mux.HandleFunc("POST /api/drafts/{id}/attachments", s.addDraftAttachment)
	mux.HandleFunc("GET /api/drafts/{id}/attachments/{attachment}", s.getDraftAttachment)
	mux.HandleFunc("DELETE /api/drafts/{id}/attachments/{attachment}", s.removeDraftAttachment)
	mux.HandleFunc("GET /api/signatures", s.listMailSignatures)
	mux.HandleFunc("POST /api/signatures", s.saveMailSignature)
	mux.HandleFunc("GET /api/signatures/{id}", s.getMailSignature)
	mux.HandleFunc("PUT /api/signatures/{id}", s.saveMailSignature)
	mux.HandleFunc("DELETE /api/signatures/{id}", s.deleteMailSignature)
}

func (s *Server) addDraftAttachment(w http.ResponseWriter, r *http.Request) {
	// Resolve ownership before accepting a large upload. Browser CSRF and MCP
	// scopes have already been checked by the shared transport middleware.
	if _, err := s.app.GetDraft(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	limit := s.app.SettingInt("attachments.max_bytes")
	if limit <= 0 || limit > application.MaxOutboundAttachmentBytes {
		limit = application.MaxOutboundAttachmentBytes
	}
	var in application.AddDraftAttachmentInput
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		r.Body = http.MaxBytesReader(w, r.Body, int64(limit)+(64<<10))
		reader, err := r.MultipartReader()
		if err != nil {
			writeErr(w, &domain.PublicError{Code: "invalid_request", Message: "올바른 multipart 첨부 요청이 필요합니다.", Status: http.StatusBadRequest})
			return
		}
		part, err := reader.NextPart()
		if err != nil || part.FormName() != "file" {
			writeErr(w, &domain.PublicError{Code: "invalid_request", Message: "file 첨부파일 하나를 지정하세요.", Status: http.StatusBadRequest})
			return
		}
		data, err := io.ReadAll(io.LimitReader(part, int64(limit)+1))
		part.Close()
		if err != nil || len(data) > limit {
			writeErr(w, &domain.PublicError{Code: "payload_too_large", Message: "첨부파일이 크기 제한을 초과했습니다.", Status: http.StatusRequestEntityTooLarge})
			return
		}
		if _, err := reader.NextPart(); err != io.EOF {
			writeErr(w, &domain.PublicError{Code: "invalid_request", Message: "한 요청에 첨부파일 하나만 허용됩니다.", Status: http.StatusBadRequest})
			return
		}
		in.Name, in.DataBase64, in.Inline = part.FileName(), base64.StdEncoding.EncodeToString(data), r.URL.Query().Get("inline") == "true"
	} else {
		r.Body = http.MaxBytesReader(w, r.Body, int64(base64.StdEncoding.EncodedLen(limit))+(64<<10))
		decoder := json.NewDecoder(r.Body)
		if err := decoder.Decode(&in); err != nil {
			writeErr(w, &domain.PublicError{Code: "invalid_request", Message: "올바른 첨부파일 JSON 요청이 필요합니다.", Status: http.StatusBadRequest})
			return
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			writeErr(w, &domain.PublicError{Code: "invalid_request", Message: "JSON 요청 하나만 허용됩니다.", Status: http.StatusBadRequest})
			return
		}
	}
	in.DraftID = r.PathValue("id")
	out, err := s.app.AddDraftAttachment(r.Context(), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) getDraftAttachment(w http.ResponseWriter, r *http.Request) {
	version, _ := strconv.Atoi(r.URL.Query().Get("version"))
	attachment, data, err := s.app.GetDraftAttachment(r.Context(), r.PathValue("id"), r.PathValue("attachment"), version)
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", attachment.MIMEType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": attachment.Name}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Download-Options", "noopen")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	// #nosec G705 -- Owner-checked scanned bytes are a forced attachment, never interpolated into HTML; nosniff, noopen and a scriptless sandbox CSP prevent document execution.
	_, _ = w.Write(data)
}

func (s *Server) removeDraftAttachment(w http.ResponseWriter, r *http.Request) {
	out, err := s.app.RemoveDraftAttachment(r.Context(), r.PathValue("id"), r.PathValue("attachment"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) rewriteMailText(w http.ResponseWriter, r *http.Request) {
	in, err := decode[application.RewriteMailTextInput](r)
	if err != nil {
		writeErr(w, err)
		return
	}
	out, err := s.app.RewriteMailText(r.Context(), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"text": out})
}

func (s *Server) mailTemplates(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, mailrender.Templates())
}

func (s *Server) renderMail(w http.ResponseWriter, r *http.Request) {
	in, err := decode[application.RenderMailInput](r)
	if err != nil {
		writeErr(w, err)
		return
	}
	out, err := s.app.RenderMail(r.Context(), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) listMailSignatures(w http.ResponseWriter, r *http.Request) {
	out, err := s.app.ListMailSignatures(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	if out == nil {
		out = []domain.MailSignature{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getMailSignature(w http.ResponseWriter, r *http.Request) {
	out, err := s.app.GetMailSignature(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) saveMailSignature(w http.ResponseWriter, r *http.Request) {
	in, err := decode[application.SaveMailSignatureInput](r)
	if err != nil {
		writeErr(w, err)
		return
	}
	// The route is authoritative; a body ID cannot redirect an update.
	in.ID = r.PathValue("id")
	out, err := s.app.SaveMailSignature(r.Context(), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	status := http.StatusOK
	if in.ID == "" {
		status = http.StatusCreated
	}
	writeJSON(w, status, out)
}

func (s *Server) deleteMailSignature(w http.ResponseWriter, r *http.Request) {
	if err := s.app.DeleteMailSignature(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

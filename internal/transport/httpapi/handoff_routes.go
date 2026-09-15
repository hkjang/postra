package httpapi

import (
	"errors"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"postra/internal/domain"
	"postra/internal/platform/handoff"
)

// Handing a message to another in-house service (aidev HANDOFF-STANDARD.md).
//
// The standard names its endpoints under /api/v1; versionedAPI maps that
// prefix onto the /api routes registered here, so both spellings reach the
// same handlers. Issuing a claim needs the signed-in user; collecting one
// needs no login at all — the claim is the credential — so the collect path
// is listed as public in publicPath.

// handoffCollectPrefix is the collect path as seen after versionedAPI has
// stripped /v1 — the form publicPath is asked about.
const handoffCollectPrefix = "/api/handoff/claims/"

func (s *Server) registerHandoffRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/handoff/targets", s.handoffTargets)
	mux.HandleFunc("POST /api/handoff/claims", s.issueHandoffClaim)
	mux.HandleFunc("GET /api/handoff/claims/{claim}", s.collectHandoffClaim)
}

// handoffCollectPath reports whether p is a collect request — the one path
// under the API that carries its own credential.
func handoffCollectPath(p string) bool {
	rest := strings.TrimPrefix(p, handoffCollectPrefix)
	return rest != p && rest != "" && !strings.Contains(rest, "/")
}

type handoffTargetView struct {
	Name   string `json:"name"`
	Origin string `json:"origin"`
}

// handoffTargets lists the services the message screen may offer a "send to"
// button for: allow-listed origins whose receiving side reads markdown. An
// empty list — the default — means no button at all.
func (s *Server) handoffTargets(w http.ResponseWriter, r *http.Request) {
	targets := s.app.HandoffTargets(r.Context())
	out := make([]handoffTargetView, 0, len(targets))
	for _, t := range targets {
		out = append(out, handoffTargetView{Name: t.Name, Origin: t.Origin})
	}
	writeJSON(w, http.StatusOK, map[string]any{"format": handoff.FormatMarkdown, "targets": out})
}

type handoffClaimBody struct {
	Resource string `json:"resource"`
	Format   string `json:"format"`
}

// issueHandoffClaim mints a single-use claim for one of the caller's
// messages. The body must really be JSON: the session cookie is accepted
// here and a cross-site HTML form cannot send application/json, so the
// content type is one more proof that a script on this origin made it.
func (s *Server) issueHandoffClaim(w http.ResponseWriter, r *http.Request) {
	if mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mediaType != "application/json" {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "Content-Type must be application/json"})
		return
	}
	in, err := decode[handoffClaimBody](r)
	if err != nil {
		writeErr(w, err)
		return
	}
	claim, err := s.app.IssueHandoffClaim(r.Context(), in.Resource, in.Format, requestOrigin(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, claim)
}

// collectHandoffClaim serves the document a claim was issued for, once. Any
// claim that cannot be served — unknown, spent, expired, malformed — is a
// bare 404 that gives nothing away; only a backend failure is a 500.
func (s *Server) collectHandoffClaim(w http.ResponseWriter, r *http.Request) {
	doc, err := s.app.CollectHandoffClaim(r.Context(), r.PathValue("claim"))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", doc.ContentType)
	w.Header().Set("Content-Disposition", handoff.ContentDisposition(doc.Filename))
	w.Header().Set("Content-Length", strconv.Itoa(len(doc.Body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(doc.Body)
}

// requestOrigin is the scheme://host this request arrived on as the browser
// sees it, honouring the usual reverse-proxy headers. It is the announced
// source of a claim when the administrator has not pinned a public origin.
func requestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(firstForwarded(r.Header.Get("X-Forwarded-Proto")), "https") {
		scheme = "https"
	}
	host := firstForwarded(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = r.Host
	}
	if host == "" {
		return ""
	}
	return scheme + "://" + host
}

func firstForwarded(value string) string {
	if value, _, ok := strings.Cut(value, ","); ok {
		return strings.TrimSpace(value)
	}
	return strings.TrimSpace(value)
}

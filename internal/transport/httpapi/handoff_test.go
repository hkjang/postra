package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"postra/internal/adapters/objectstore"
	"postra/internal/adapters/persistence"
	"postra/internal/application"
	"postra/internal/domain"
	"postra/internal/platform/config"
	"postra/internal/platform/crypto"
)

// newHandoffHandler is newTestHandler with one message in the default
// user's mailbox to hand over.
func newHandoffHandler(t *testing.T, token string) http.Handler {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	kek, err := crypto.LoadOrCreateKEK(dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := persistence.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	store.EnableEncryption(kek)
	t.Cleanup(func() { store.Close() })
	local, err := objectstore.NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	app, err := application.New(cfg, store, objectstore.NewEncrypted(local, kek), nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Shutdown)
	now := time.Now().Unix()
	m := &domain.Message{
		ID: "msg_1", UserID: application.DefaultUserID, AccountID: "acc_1", UIDL: "u1",
		Subject: "분기 보고", From: domain.Address{Email: "alice@example.com"},
		RawHash: "h1", RawURI: "mem://1", Date: now, CreatedAt: now,
	}
	if err := store.InsertMessage(context.Background(), m, &domain.MessageBody{MessageID: m.ID, TextBody: "본문입니다."}, nil); err != nil {
		t.Fatal(err)
	}
	return New(app, token).Handler()
}

func issue(t *testing.T, h http.Handler, token, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/handoff/claims", strings.NewReader(body))
	req.Host = "postra.intra"
	req.Header.Set("X-Forwarded-Proto", "https")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The standard's two endpoints, end to end: issue needs the user, collect
// needs nothing but the claim, and the claim works exactly once.
func TestHandoffEndpoints(t *testing.T) {
	h := newHandoffHandler(t, "sekret")

	if rec := issue(t, h, "", "application/json", `{"resource":"msg_1","format":"markdown"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("issuing without a user: %d, want 401", rec.Code)
	}
	if rec := issue(t, h, "sekret", "text/plain", `{"resource":"msg_1","format":"markdown"}`); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("issuing with a form-capable content type: %d, want 415", rec.Code)
	}
	if rec := issue(t, h, "sekret", "application/json", `{"resource":"msg_nope","format":"markdown"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("issuing for an unknown message: %d, want 404", rec.Code)
	}
	if rec := issue(t, h, "sekret", "application/json", `{"resource":"msg_1","format":"pptx"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("issuing a format we do not send: %d, want 400", rec.Code)
	}

	rec := issue(t, h, "sekret", "application/json; charset=utf-8", `{"resource":"msg_1","format":"markdown"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("issue: %d %s", rec.Code, rec.Body.String())
	}
	var claim struct {
		Claim       string `json:"claim"`
		Source      string `json:"source"`
		Filename    string `json:"filename"`
		ContentType string `json:"content_type"`
		Bytes       int    `json:"bytes"`
		ExpiresAt   string `json:"expires_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &claim); err != nil {
		t.Fatal(err)
	}
	if claim.Claim == "" || claim.Source != "https://postra.intra" || claim.Filename != "분기 보고.md" ||
		claim.ContentType != "text/markdown; charset=utf-8" || claim.Bytes == 0 || claim.ExpiresAt == "" {
		t.Fatalf("claim answer is not the standard's shape: %s", rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("a claim must not be cached")
	}

	// Collect: no Authorization header at all.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/handoff/claims/"+claim.Claim, nil)
	got := httptest.NewRecorder()
	h.ServeHTTP(got, req)
	if got.Code != http.StatusOK {
		t.Fatalf("collect: %d %s", got.Code, got.Body.String())
	}
	if ct := got.Header().Get("Content-Type"); ct != "text/markdown; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cd := got.Header().Get("Content-Disposition"); cd != "attachment; filename*=UTF-8''%EB%B6%84%EA%B8%B0%20%EB%B3%B4%EA%B3%A0.md" {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	if got.Body.Len() != claim.Bytes || !strings.Contains(got.Body.String(), "# 분기 보고\n") || !strings.Contains(got.Body.String(), "본문입니다.") {
		t.Fatalf("collected body does not match the claim (%d bytes announced):\n%s", claim.Bytes, got.Body.String())
	}

	// Second time: 404, and so is any other claim, without a reason.
	for _, path := range []string{"/api/v1/handoff/claims/" + claim.Claim, "/api/v1/handoff/claims/" + strings.Repeat("A", 43), "/api/v1/handoff/claims/nope"} {
		again := httptest.NewRecorder()
		h.ServeHTTP(again, httptest.NewRequest(http.MethodGet, path, nil))
		if again.Code != http.StatusNotFound || !strings.Contains(again.Body.String(), `"not found"`) {
			t.Fatalf("%s: %d %s, want a bare 404", path, again.Code, again.Body.String())
		}
	}
}

// Only the collect path is public; the rest of the API still needs a token.
// publicPath sees the path after versionedAPI has stripped /v1.
func TestHandoffPublicPathIsNarrow(t *testing.T) {
	for path, public := range map[string]bool{
		"/api/handoff/claims/abc":  true,
		"/api/handoff/claims/a/b":  false,
		"/api/handoff/claims/":     false,
		"/api/handoff/claims":      false,
		"/api/handoff/targets":     false,
		"/api/handoff":             false,
		"/api/v1/handoff/claims/x": false,
		"/api/messages":            false,
	} {
		if publicPath(path) != public {
			t.Errorf("publicPath(%q) = %v, want %v", path, !public, public)
		}
	}
}

// The message screen asks which services it may offer; a fresh installation
// answers none, and only markdown readers on the allow list are named.
func TestHandoffTargetsEndpoint(t *testing.T) {
	h := newHandoffHandler(t, "sekret")
	get := func(auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/handoff/targets", nil)
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := get(""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("targets without a user: %d, want 401", rec.Code)
	}
	rec := get("sekret")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"format":"markdown","targets":[]}` {
		t.Fatalf("fresh installation should offer nothing: %d %s", rec.Code, rec.Body.String())
	}
	patch := httptest.NewRequest(http.MethodPatch, "/api/v1/admin/settings", strings.NewReader(`{"values":{"handoff.targets":"[{\"name\":\"Ptium\",\"origin\":\"https://ptium.intra\",\"formats\":[\"markdown\"]},{\"name\":\"Kanpic\",\"origin\":\"https://kanpic.intra\",\"formats\":[\"csv\"]}]"}}`))
	patch.Header.Set("Authorization", "Bearer sekret")
	patch.Header.Set("Content-Type", "application/json")
	saved := httptest.NewRecorder()
	h.ServeHTTP(saved, patch)
	if saved.Code != http.StatusOK {
		t.Fatalf("saving the allow list: %d %s", saved.Code, saved.Body.String())
	}
	rec = get("sekret")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"format":"markdown","targets":[{"name":"Ptium","origin":"https://ptium.intra"}]}` {
		t.Fatalf("only markdown readers are offered: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRequestOrigin(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Host = "localhost:8480"
	if got := requestOrigin(r); got != "http://localhost:8480" {
		t.Fatalf("plain: %q", got)
	}
	r.Header.Set("X-Forwarded-Proto", "https, http")
	r.Header.Set("X-Forwarded-Host", "postra.intra, inner")
	if got := requestOrigin(r); got != "https://postra.intra" {
		t.Fatalf("proxied: %q", got)
	}
}

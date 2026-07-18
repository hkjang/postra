package httpapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"postra/internal/adapters/objectstore"
	"postra/internal/adapters/persistence"
	"postra/internal/application"
	"postra/internal/platform/config"
	"postra/internal/platform/crypto"
	"postra/internal/platform/metrics"
)

func newTestHandler(t *testing.T, token string) http.Handler {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.MetricsEnabled = true

	kek, err := crypto.LoadOrCreateKEK(dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := persistence.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	local, err := objectstore.NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Mail adapters are unused by the /metrics and /healthz paths under test.
	app, err := application.New(cfg, store, objectstore.NewEncrypted(local, kek), nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Shutdown)
	return New(app, token).Handler()
}

// TestMetricsEndpoint verifies /metrics is served without a token and that a
// counted request bumps the request counter under its matched route label.
func TestMetricsEndpoint(t *testing.T) {
	h := newTestHandler(t, "sekret")

	// /metrics bypasses auth entirely.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "postra_") {
		t.Fatalf("/metrics body missing postra_ series")
	}

	// A normal authenticated request is counted under its route pattern.
	// /api/accounts requires the token (unlike the public probe endpoints).
	route := "GET /api/accounts"
	before := testutil.ToFloat64(metrics.HTTPRequests.WithLabelValues(route, "GET", "200"))
	req := httptest.NewRequest("GET", "/api/accounts", nil)
	req.Header.Set("Authorization", "Bearer sekret")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("/api/accounts status = %d, want 200", rec2.Code)
	}
	if got := testutil.ToFloat64(metrics.HTTPRequests.WithLabelValues(route, "GET", "200")) - before; got < 1 {
		t.Fatalf("http_requests_total delta = %v, want >= 1", got)
	}

	// A bad token is rejected and counted as 401.
	before401 := testutil.ToFloat64(metrics.HTTPRequests.WithLabelValues(route, "GET", "401"))
	reqBad := httptest.NewRequest("GET", "/api/accounts", nil)
	reqBad.Header.Set("Authorization", "Bearer wrong")
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, reqBad)
	if rec3.Code != http.StatusUnauthorized {
		t.Fatalf("bad-token status = %d, want 401", rec3.Code)
	}
	if got := testutil.ToFloat64(metrics.HTTPRequests.WithLabelValues(route, "GET", "401")) - before401; got < 1 {
		t.Fatalf("http_requests_total{401} delta = %v, want >= 1", got)
	}
}

// TestProbesUnauthenticated verifies liveness/readiness probes are reachable
// without the API token and report the store's health.
func TestProbesUnauthenticated(t *testing.T) {
	h := newTestHandler(t, "sekret") // token set, but probes must bypass it
	for _, path := range []string{"/api/livez", "/api/readyz", "/api/healthz"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil)) // no Authorization header
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d, want 200 (store is up, no token required)", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
			t.Fatalf("%s body = %s", path, rec.Body.String())
		}
	}
}

// TestMetricsDisabled ensures the endpoint is absent when disabled.
func TestMetricsDisabled(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.MetricsEnabled = false

	kek, _ := crypto.LoadOrCreateKEK(dir)
	store, err := persistence.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	local, _ := objectstore.NewLocal(dir)
	app, err := application.New(cfg, store, objectstore.NewEncrypted(local, kek), nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Shutdown)

	rec := httptest.NewRecorder()
	New(app, "").Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("/metrics status = %d, want 404 when disabled", rec.Code)
	}
}

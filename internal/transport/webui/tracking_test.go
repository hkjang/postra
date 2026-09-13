package webui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"postra/internal/platform/tracking"
)

var noncePattern = regexp.MustCompile(`'nonce-([A-Za-z0-9_-]+)'`)

// scriptDirective returns the script-src directive of a policy header.
func scriptDirective(policy string) string {
	for _, directive := range strings.Split(policy, ";") {
		if directive = strings.TrimSpace(directive); strings.HasPrefix(directive, "script-src ") {
			return directive
		}
	}
	return ""
}

func policyNonce(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	match := noncePattern.FindStringSubmatch(rec.Header().Get("Content-Security-Policy"))
	if match == nil {
		t.Fatalf("no nonce in policy %q", rec.Header().Get("Content-Security-Policy"))
	}
	return match[1]
}

func TestTrackingOffByDefault(t *testing.T) {
	app, _ := newTestApp(t)
	h := New(app, "").Handler()

	rec := do(t, h, http.MethodGet, "/ui/", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("search page: %d", rec.Code)
	}
	policy := rec.Header().Get("Content-Security-Policy")
	nonce := policyNonce(t, rec)
	body := rec.Body.String()
	if scriptDirective(policy) != "script-src 'self' 'nonce-"+nonce+"'" || strings.Contains(policy, "report-uri") {
		t.Fatalf("default policy should be strict and silent: %s", policy)
	}
	if !strings.Contains(body, `<script nonce="`+nonce+`">`) {
		t.Fatal("the layout's own script must carry the request nonce")
	}
	if strings.Contains(body, "tracker.js") || strings.Contains(body, "googletagmanager") {
		t.Fatal("a fresh installation must not carry any tracking markup")
	}
	if strings.Contains(body, " onsubmit=") || strings.Contains(body, " onclick=") {
		t.Fatal("inline event handlers cannot run under the nonce policy")
	}
	// Two requests, two nonces.
	if other := policyNonce(t, do(t, h, http.MethodGet, "/ui/", nil, nil)); other == nonce {
		t.Fatal("nonce must differ per request")
	}
	// The proxy path does not exist while Momento is off.
	if rec := do(t, h, http.MethodGet, "/momento/tracker.js", nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("momento proxy should be absent when off: %d", rec.Code)
	}
}

func TestTrackingSnippetCarriesNonceAndPolicySources(t *testing.T) {
	app, _ := newTestApp(t)
	ctx := context.Background()
	if err := app.Store.UpsertSettings(ctx, map[string]string{
		tracking.SettingEnabled: "true", tracking.SettingProvider: "custom", tracking.SettingPlacement: "body",
		tracking.SettingCustomSnippet: "İİ<SCRIPT src=\"https://tracker.example/t.js\"></SCRIPT>\n<script>window.__t='https://collect.example/v1';</script>",
		tracking.SettingAllowedHosts:  "https://pixel.example",
	}); err != nil {
		t.Fatal(err)
	}
	h := New(app, "").Handler()

	rec := do(t, h, http.MethodGet, "/ui/", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("search page: %d", rec.Code)
	}
	nonce := policyNonce(t, rec)
	policy := rec.Header().Get("Content-Security-Policy")
	body := rec.Body.String()
	for _, want := range []string{
		"script-src 'self' 'nonce-" + nonce + "' https://tracker.example https://collect.example https://pixel.example",
		"connect-src 'self' https://tracker.example https://collect.example https://pixel.example",
		"img-src 'self' data: https://tracker.example https://collect.example https://pixel.example",
		"report-uri " + cspReportPath,
	} {
		if !strings.Contains(policy, want) {
			t.Fatalf("policy missing %q: %s", want, policy)
		}
	}
	if strings.Contains(scriptDirective(policy), "unsafe-inline") {
		t.Fatalf("script policy must not be relaxed: %s", policy)
	}
	if strings.Count(body, `nonce="`+nonce+`"`) != 3 {
		t.Fatalf("expected the layout script and both snippet tags to carry the nonce: %s", body)
	}
	if !strings.Contains(body, `İİ<SCRIPT nonce="`+nonce+`" src="https://tracker.example/t.js">`) {
		t.Fatalf("nonce must land inside the tag even after multibyte letters: %s", body)
	}
	// Placement body: the snippet sits after the layout script, before </body>.
	if strings.LastIndex(body, "tracker.example") < strings.LastIndex(body, "checkSyncStatus") || !strings.Contains(body, "</script>\n</body>") {
		t.Fatalf("body placement should put the snippet at the end of the document")
	}

	// Head placement.
	if err := app.Store.UpsertSettings(ctx, map[string]string{tracking.SettingPlacement: "head"}); err != nil {
		t.Fatal(err)
	}
	body = do(t, h, http.MethodGet, "/ui/", nil, nil).Body.String()
	if strings.Index(body, "tracker.example") > strings.Index(body, "</head>") {
		t.Fatal("head placement should put the snippet before </head>")
	}

	// Admin screens are excluded until include_admin; login never.
	rec = do(t, h, http.MethodGet, "/ui/admin/settings", nil, nil)
	if strings.Contains(rec.Body.String(), "tracker.example/t.js") || strings.Contains(rec.Header().Get("Content-Security-Policy"), "tracker.example") {
		t.Fatal("admin screens must not be tracked by default")
	}
	rec = do(t, h, http.MethodGet, "/ui/login", nil, nil)
	if strings.Contains(rec.Header().Get("Content-Security-Policy"), "tracker.example") {
		t.Fatal("the login screen is never tracked")
	}

	// Turning tracking off narrows the policy back and drops the markup.
	if err := app.Store.UpsertSettings(ctx, map[string]string{tracking.SettingEnabled: "false"}); err != nil {
		t.Fatal(err)
	}
	rec = do(t, h, http.MethodGet, "/ui/", nil, nil)
	policy = rec.Header().Get("Content-Security-Policy")
	if strings.Contains(policy, "tracker.example") || strings.Contains(policy, "report-uri") || strings.Contains(rec.Body.String(), "tracker.example") {
		t.Fatalf("policy should narrow back when tracking is off: %s", policy)
	}
}

func TestTrackingNonPageResponsesGetNarrowPolicy(t *testing.T) {
	app, _ := newTestApp(t)
	if err := app.Store.UpsertSettings(context.Background(), map[string]string{
		tracking.SettingEnabled: "true", tracking.SettingProvider: "ga4", tracking.SettingMeasurementID: "G-1",
	}); err != nil {
		t.Fatal(err)
	}
	h := New(app, "").Handler()
	for _, path := range []string{"/ui/static/logo.png", "/favicon.ico", "/ui/jobs/status"} {
		rec := do(t, h, http.MethodGet, path, nil, nil)
		if got := rec.Header().Get("Content-Security-Policy"); got != "default-src 'none'; frame-ancestors 'none'" {
			t.Fatalf("%s: policy %q", path, got)
		}
	}
}

func TestTrackingMomentoProxy(t *testing.T) {
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" {
			t.Errorf("session cookie must not reach the collector: %q", r.Header.Get("Cookie"))
		}
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write([]byte("// tracker for " + r.URL.Path + "?" + r.URL.RawQuery))
	}))
	defer collector.Close()

	app, _ := newTestApp(t)
	if err := app.Store.UpsertSettings(context.Background(), map[string]string{
		tracking.SettingEnabled: "true", tracking.SettingProvider: "momento",
		tracking.SettingMomentoURL: collector.URL + "/base/", tracking.SettingMomentoSiteID: "postra",
	}); err != nil {
		t.Fatal(err)
	}
	h := New(app, "").Handler()

	rec := do(t, h, http.MethodGet, "/ui/", nil, nil)
	policy := rec.Header().Get("Content-Security-Policy")
	nonce := policyNonce(t, rec)
	if strings.Contains(policy, "127.0.0.1") {
		t.Fatalf("proxied momento must not add the collector origin: %s", policy)
	}
	if !strings.Contains(rec.Body.String(), `<script nonce="`+nonce+`" async src="/momento/tracker.js" data-site-id="postra"`) {
		t.Fatalf("momento snippet: %s", rec.Body.String())
	}

	rec = do(t, h, http.MethodGet, "/momento/tracker.js?v=1", nil, &http.Cookie{Name: cookieName, Value: "secret"})
	if rec.Code != http.StatusOK || rec.Body.String() != "// tracker for /base/tracker.js?v=1" {
		t.Fatalf("proxy: code=%d body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Security-Policy"); got != "default-src 'none'; frame-ancestors 'none'" {
		t.Fatalf("proxy response policy: %q", got)
	}
}

func TestTrackingViolationReportsAndAdminAllow(t *testing.T) {
	app, _ := newTestApp(t)
	app.Cfg.Auth.Enabled = true
	if _, err := app.SetupInitialAdmin(context.Background(), "admin", "Administrator", "a-secure-password"); err != nil {
		t.Fatal(err)
	}
	if err := app.Store.UpsertSettings(context.Background(), map[string]string{
		tracking.SettingEnabled: "true", tracking.SettingProvider: "custom",
		tracking.SettingCustomSnippet: `<script src="https://tracker.example/t.js"></script>`,
	}); err != nil {
		t.Fatal(err)
	}
	h := New(app, "").Handler()

	// Browsers post reports on their own, without a session.
	report := `{"csp-report":{"blocked-uri":"https://collect.example/v1/events","effective-directive":"connect-src","document-uri":"http://postra.example/ui/?q=secret"}}`
	req := httptest.NewRequest(http.MethodPost, cspReportPath, strings.NewReader(report))
	req.Header.Set("Content-Type", "application/csp-report")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("report: %d", rec.Code)
	}
	for i := 0; i < 3; i++ {
		req = httptest.NewRequest(http.MethodPost, cspReportPath, strings.NewReader(report))
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	req = httptest.NewRequest(http.MethodPost, cspReportPath, strings.NewReader(`{"csp-report":{"blocked-uri":"inline","violated-directive":"script-src-elem"}}`))
	h.ServeHTTP(httptest.NewRecorder(), req)

	rec = do(t, h, http.MethodPost, "/ui/login", url.Values{"login_id": {"admin"}, "password": {"a-secure-password"}}, nil)
	var session *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			session = c
		}
	}
	if session == nil {
		t.Fatal("no session cookie")
	}

	rec = do(t, h, http.MethodGet, "/ui/admin/settings", nil, session)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "<code>https://collect.example</code>") || !strings.Contains(body, "<td>connect-src</td>") || !strings.Contains(body, "<td>4</td>") {
		t.Fatalf("admin settings should list the blocked origin once with its count: code=%d body=%s", rec.Code, body)
	}
	if strings.Contains(body, "q=secret") {
		t.Fatal("the report's query string must not be shown")
	}
	if !strings.Contains(body, `name="tracking.enabled" value="true" checked`) {
		t.Fatal("tracking settings should render as stored")
	}

	rec = do(t, h, http.MethodPost, "/ui/admin/tracking/allow", url.Values{"origin": {"https://collect.example"}}, session)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("allow: code=%d body=%s", rec.Code, rec.Body.String())
	}
	settings, err := app.Store.GetSettings(context.Background())
	if err != nil || settings[tracking.SettingAllowedHosts] != "https://collect.example" {
		t.Fatalf("allowed hosts after allow: %q (%v)", settings[tracking.SettingAllowedHosts], err)
	}
	rec = do(t, h, http.MethodGet, "/ui/admin/settings", nil, session)
	if !strings.Contains(rec.Body.String(), "허용됨") {
		t.Fatal("an allowed origin should be marked as such")
	}
	rec = do(t, h, http.MethodPost, "/ui/admin/tracking/allow", url.Values{"origin": {"javascript:alert(1)"}}, session)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-http origin must be refused: %d", rec.Code)
	}

	rec = do(t, h, http.MethodPost, "/ui/admin/tracking/forget", nil, session)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("forget: %d", rec.Code)
	}
	rec = do(t, h, http.MethodGet, "/ui/admin/settings", nil, session)
	if !strings.Contains(rec.Body.String(), "기록된 차단이 없습니다") {
		t.Fatal("forget should clear the list")
	}

	// An oversized snippet is refused by the settings form.
	rec = do(t, h, http.MethodPost, "/ui/admin/settings", url.Values{
		"tracking.enabled": {"true"}, "tracking.provider": {"custom"},
		"tracking.custom_snippet": {"<script>" + strings.Repeat("x", tracking.MaxSnippetBytes) + "</script>"},
	}, session)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "8192바이트") {
		t.Fatalf("oversized snippet: code=%d", rec.Code)
	}
	settings, _ = app.Store.GetSettings(context.Background())
	if strings.Contains(settings[tracking.SettingCustomSnippet], "xxxx") {
		t.Fatal("oversized snippet must not be stored")
	}
	// Saving with a provider that lacks its id is refused too, but disabled
	// partial input is accepted.
	rec = do(t, h, http.MethodPost, "/ui/admin/settings", url.Values{"tracking.enabled": {"true"}, "tracking.provider": {"momento"}}, session)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("momento without url/site id: %d", rec.Code)
	}
	rec = do(t, h, http.MethodPost, "/ui/admin/settings", url.Values{"tracking.provider": {"momento"}, "tracking.momento_url": {"https://m.example"}}, session)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("disabled partial input: code=%d body=%s", rec.Code, rec.Body.String())
	}
	settings, _ = app.Store.GetSettings(context.Background())
	if settings[tracking.SettingEnabled] != "false" || settings[tracking.SettingProvider] != "momento" {
		t.Fatalf("settings after partial save: %v", settings)
	}
}

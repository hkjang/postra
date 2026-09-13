package tracking

import (
	"strings"
	"testing"
	"time"
)

func TestDefaultsAreOff(t *testing.T) {
	c := ReadConfig(map[string]string{})
	if c.Enabled || c.Provider != ProviderNone || c.Placement != "head" || !c.MomentoProxy {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if c.Active("/ui/") || c.Snippet("n") != "" {
		t.Fatal("a fresh installation must not inject anything")
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
	scripts, connects, images := c.PolicySources()
	if len(scripts)+len(connects)+len(images) != 0 {
		t.Fatalf("defaults must add no policy sources: %v %v %v", scripts, connects, images)
	}
}

func TestMomentoProxySnippetNeedsNoExternalOrigin(t *testing.T) {
	c := ReadConfig(map[string]string{
		SettingEnabled: "true", SettingProvider: "momento",
		SettingMomentoURL: "https://momento.corp.example/", SettingMomentoSiteID: "postra",
	})
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if !c.ProxyEnabled() {
		t.Fatal("proxy should be on by default for momento")
	}
	snippet := c.Snippet("abc")
	for _, want := range []string{`src="/momento/tracker.js"`, `data-endpoint="/momento"`, `data-site-id="postra"`, `nonce="abc"`, `data-contract-version="1"`} {
		if !strings.Contains(snippet, want) {
			t.Fatalf("snippet missing %q: %s", want, snippet)
		}
	}
	if strings.Contains(snippet, "momento.corp.example") {
		t.Fatalf("proxied snippet must not name the collector: %s", snippet)
	}
	scripts, connects, images := c.PolicySources()
	if len(scripts)+len(connects)+len(images) != 0 {
		t.Fatalf("proxied momento must add no origins: %v %v %v", scripts, connects, images)
	}

	c.MomentoProxy = false
	snippet = c.Snippet("abc")
	if !strings.Contains(snippet, `src="https://momento.corp.example/tracker.js"`) || strings.Contains(snippet, "data-endpoint") {
		t.Fatalf("direct snippet: %s", snippet)
	}
	scripts, connects, _ = c.PolicySources()
	if len(scripts) != 1 || scripts[0] != "https://momento.corp.example" || connects[0] != "https://momento.corp.example" {
		t.Fatalf("direct momento must allow the collector: %v %v", scripts, connects)
	}
}

func TestValidateRequiresProviderFields(t *testing.T) {
	cases := map[string]map[string]string{
		"momento": {SettingEnabled: "true", SettingProvider: "momento", SettingMomentoURL: "https://m.example"},
		"ga4":     {SettingEnabled: "true", SettingProvider: "ga4"},
		"matomo":  {SettingEnabled: "true", SettingProvider: "matomo", SettingMatomoSiteID: "1"},
		"custom":  {SettingEnabled: "true", SettingProvider: "custom"},
		"unknown": {SettingProvider: "piwik"},
	}
	for name, values := range cases {
		if err := ReadConfig(values).Validate(); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
	// Disabled with partial input is fine: the admin can save before enabling.
	if err := ReadConfig(map[string]string{SettingProvider: "ga4"}).Validate(); err != nil {
		t.Fatalf("disabled partial config should validate: %v", err)
	}
	big := Config{Provider: ProviderCustom, CustomSnippet: strings.Repeat("x", MaxSnippetBytes+1)}
	if err := big.Validate(); err == nil {
		t.Fatal("oversized snippet must be rejected even when disabled")
	}
}

func TestWithNonceSurvivesMultibyteFolding(t *testing.T) {
	// U+0130 folds to three bytes and U+212A KELVIN SIGN to one; an index from
	// a lowercased copy would land inside the tag name.
	for _, snippet := range []string{
		"İİİİ<script>1</script>",
		"KK<SCRIPT src='https://t.example/a.js'></SCRIPT>",
		`<script nonce="keep">x</script><script>y</script>`,
	} {
		out := withNonce(snippet, "n1")
		if strings.Contains(out, "<sc nonce") || strings.Contains(out, "<SC nonce") {
			t.Fatalf("nonce landed inside the tag name: %s", out)
		}
		opens := strings.Count(strings.ToLower(out), "<script")
		nonces := strings.Count(out, `nonce="n1"`) + strings.Count(out, `nonce="keep"`)
		if opens != nonces {
			t.Fatalf("every script tag needs exactly one nonce: %s", out)
		}
	}
	if withNonce(`<script nonce="keep">x</script>`, "n1") != `<script nonce="keep">x</script>` {
		t.Fatal("an existing nonce must be left alone")
	}
}

func TestSnippetOriginsAndAllowedHosts(t *testing.T) {
	snippet := `<script src="HTTPS://Tracker.Example/t.js"></script>
<script>window.__t={endpoint:"https://tracker.example/collect",pixel:'http://pixel.example/p.gif?id=1'};</script>
<script src="/local.js"></script>`
	origins := SnippetOrigins(snippet)
	if len(origins) != 2 || origins[0] != "https://tracker.example" || origins[1] != "http://pixel.example" {
		t.Fatalf("origins: %v", origins)
	}
	c := Config{Enabled: true, Provider: ProviderCustom, CustomSnippet: snippet, AllowedHosts: "https://extra.example, https://*.wild.example"}
	scripts, connects, images := c.PolicySources()
	for _, group := range [][]string{scripts, connects, images} {
		if len(group) != 4 || group[2] != "https://extra.example" || group[3] != "https://*.wild.example" {
			t.Fatalf("policy sources: %v", group)
		}
	}
	if got := AddAllowedHost("", "https://a.example/"); got != "https://a.example" {
		t.Fatalf("AddAllowedHost empty: %q", got)
	}
	if got := AddAllowedHost("https://a.example", "HTTPS://A.EXAMPLE"); got != "https://a.example" {
		t.Fatalf("AddAllowedHost duplicate: %q", got)
	}
	if got := AddAllowedHost("https://a.example", "https://b.example"); got != "https://a.example, https://b.example" {
		t.Fatalf("AddAllowedHost append: %q", got)
	}
}

func TestActiveRespectsAdminAndAuthPaths(t *testing.T) {
	c := ReadConfig(map[string]string{SettingEnabled: "true", SettingProvider: "ga4", SettingMeasurementID: "G-1"})
	if !c.Active("/ui/") || !c.Active("/ui/messages/m1") {
		t.Fatal("user pages should be tracked")
	}
	if c.Active("/ui/admin/settings") || c.Active("/ui/login") || c.Active("/ui/setup") || c.Active("/ui/auth/oidc/callback") {
		t.Fatal("admin and credential screens must not be tracked by default")
	}
	c.IncludeAdmin = true
	if !c.Active("/ui/admin/settings") {
		t.Fatal("include_admin should track admin screens")
	}
	if c.Active("/ui/login") {
		t.Fatal("credential screens stay untracked")
	}
}

func TestRecorderKeepsDistinctOrigins(t *testing.T) {
	r := NewRecorder()
	clock := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	r.now = func() time.Time { clock = clock.Add(time.Second); return clock }
	for i := 0; i < 5; i++ {
		r.Record("https://momento.corp.example/collect/v1/events", "connect-src https://x", "/ui/")
	}
	r.Record("chrome-extension://abc/x.js", "script-src-elem", "/ui/")
	r.Record("inline", "script-src-elem", "/ui/")
	r.Record("https://pixel.example/p.gif", "", "/ui/messages/1")
	items := r.List(Config{})
	if len(items) != 2 {
		t.Fatalf("expected two distinct origins, got %+v", items)
	}
	if items[0].Origin != "https://pixel.example" || items[0].Directive != "connect-src" {
		t.Fatalf("most recent first with defaulted directive: %+v", items[0])
	}
	if items[1].Origin != "https://momento.corp.example" || items[1].Count != 5 || items[1].Directive != "connect-src" {
		t.Fatalf("repeated origin should be counted, not duplicated: %+v", items[1])
	}

	allowed := Config{Enabled: true, Provider: ProviderGA4, MeasurementID: "G-1", AllowedHosts: "https://momento.corp.example"}
	r.Record("https://stats.google-analytics.com/g/collect", "connect-src", "/ui/")
	for _, item := range r.List(allowed) {
		switch item.Origin {
		case "https://momento.corp.example", "https://stats.google-analytics.com":
			if !item.Allowed {
				t.Fatalf("%s should be marked allowed", item.Origin)
			}
		default:
			if item.Allowed {
				t.Fatalf("%s should not be marked allowed", item.Origin)
			}
		}
	}

	for i := 0; i < MaxViolations+20; i++ {
		r.Record("https://h"+strings.Repeat("x", i%7)+strings.Repeat("y", i/7)+".example", "img-src", "/")
	}
	if got := len(r.List(Config{})); got != MaxViolations {
		t.Fatalf("recorder should be bounded at %d, got %d", MaxViolations, got)
	}
	r.Forget()
	if len(r.List(Config{})) != 0 {
		t.Fatal("Forget should drop everything")
	}
}

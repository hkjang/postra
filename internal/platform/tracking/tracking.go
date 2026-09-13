// Package tracking renders a visitor tracking snippet for the web UI and derives
// the content security policy sources it needs.
//
// The UI ships with a script policy that allows only the application origin, so
// a pasted snippet cannot simply be dropped into the page — it would be blocked
// without a word to the administrator. This package produces both halves of the
// answer: the markup, with a per-request nonce on every script tag, and the
// origins the policy must allow for that markup to run. The policy itself is
// never relaxed with 'unsafe-inline'.
//
// Momento comes first. It is the in-house collector, so it is the one option
// where visitor data never leaves the network; with the same-origin proxy no
// external origin appears in the policy at all.
package tracking

import (
	"fmt"
	"html"
	"net/url"
	"strings"
)

const (
	ProviderNone    = "none"
	ProviderMomento = "momento"
	ProviderGA4     = "ga4"
	ProviderGTM     = "gtm"
	ProviderMatomo  = "matomo"
	ProviderCustom  = "custom"

	// MaxSnippetBytes bounds a pasted snippet. A tracker loader is a few
	// hundred bytes; anything larger is a mistake or an attempt to ship code.
	MaxSnippetBytes = 8 * 1024

	// ProxyPath is the same-origin prefix the application forwards to the
	// Momento collector so the browser never talks to another origin.
	ProxyPath = "/momento"
)

// Providers lists the choices in the order the admin screen shows them.
var Providers = []string{ProviderNone, ProviderMomento, ProviderGA4, ProviderGTM, ProviderMatomo, ProviderCustom}

// Setting keys as stored in system settings.
const (
	SettingEnabled       = "tracking.enabled"
	SettingProvider      = "tracking.provider"
	SettingMomentoURL    = "tracking.momento_url"
	SettingMomentoSiteID = "tracking.momento_site_id"
	SettingMomentoProxy  = "tracking.momento_proxy"
	SettingMeasurementID = "tracking.measurement_id"
	SettingMatomoURL     = "tracking.matomo_url"
	SettingMatomoSiteID  = "tracking.matomo_site_id"
	SettingCustomSnippet = "tracking.custom_snippet"
	SettingAllowedHosts  = "tracking.allowed_hosts"
	SettingIncludeAdmin  = "tracking.include_admin"
	SettingPlacement     = "tracking.placement"
)

// SettingKeys lists every tracking setting so callers can register and
// default them in one place.
var SettingKeys = []string{
	SettingEnabled, SettingProvider, SettingMomentoURL, SettingMomentoSiteID, SettingMomentoProxy,
	SettingMeasurementID, SettingMatomoURL, SettingMatomoSiteID, SettingCustomSnippet,
	SettingAllowedHosts, SettingIncludeAdmin, SettingPlacement,
}

// Defaults are the values a fresh installation carries: tracking off, nothing
// injected, the policy unchanged.
var Defaults = map[string]string{
	SettingEnabled: "false", SettingProvider: ProviderNone, SettingMomentoURL: "", SettingMomentoSiteID: "",
	SettingMomentoProxy: "true", SettingMeasurementID: "", SettingMatomoURL: "", SettingMatomoSiteID: "",
	SettingCustomSnippet: "", SettingAllowedHosts: "", SettingIncludeAdmin: "false", SettingPlacement: "head",
}

type Config struct {
	Enabled       bool
	Provider      string
	MomentoURL    string
	MomentoSiteID string
	MomentoProxy  bool
	MeasurementID string
	MatomoURL     string
	MatomoSiteID  string
	CustomSnippet string
	AllowedHosts  string
	IncludeAdmin  bool
	Placement     string
}

// ReadConfig maps stored settings onto the configuration. Missing keys fall
// back to the defaults, so a store that predates tracking reads as "off".
func ReadConfig(values map[string]string) Config {
	get := func(key string) string {
		if value, ok := values[key]; ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
		return Defaults[key]
	}
	config := Config{
		Enabled:       get(SettingEnabled) == "true",
		Provider:      strings.ToLower(get(SettingProvider)),
		MomentoURL:    get(SettingMomentoURL),
		MomentoSiteID: get(SettingMomentoSiteID),
		MomentoProxy:  get(SettingMomentoProxy) == "true",
		MeasurementID: get(SettingMeasurementID),
		MatomoURL:     get(SettingMatomoURL),
		MatomoSiteID:  get(SettingMatomoSiteID),
		CustomSnippet: values[SettingCustomSnippet],
		AllowedHosts:  get(SettingAllowedHosts),
		IncludeAdmin:  get(SettingIncludeAdmin) == "true",
		Placement:     strings.ToLower(get(SettingPlacement)),
	}
	if config.Placement != "body" {
		config.Placement = "head"
	}
	return config
}

// Active reports whether a page at path should carry the snippet. Login and
// setup screens handle credentials and are never tracked; administration
// screens only when an administrator asks for them.
func (c Config) Active(path string) bool {
	if !c.Enabled || c.Provider == ProviderNone || c.Provider == "" {
		return false
	}
	if IsAuthPath(path) {
		return false
	}
	if !c.IncludeAdmin && IsAdminPath(path) {
		return false
	}
	return strings.TrimSpace(c.Snippet("")) != ""
}

// IsAuthPath marks the screens that handle credentials.
func IsAuthPath(path string) bool {
	return path == "/ui/login" || path == "/ui/setup" || strings.HasPrefix(path, "/ui/auth/")
}

// IsAdminPath marks the administration screens.
func IsAdminPath(path string) bool {
	return strings.HasPrefix(path, "/ui/admin/") || path == "/ui/admin"
}

// ProxyEnabled reports whether /momento/* should be forwarded to the collector.
func (c Config) ProxyEnabled() bool {
	return c.Enabled && c.Provider == ProviderMomento && c.MomentoProxy && originOf(c.MomentoURL) != ""
}

// Validate reports what is missing for the chosen provider. A disabled
// configuration is always valid so an administrator can save partial input
// before switching it on.
func (c Config) Validate() error {
	if len(c.CustomSnippet) > MaxSnippetBytes {
		return fmt.Errorf("추적 코드는 %d바이트를 넘을 수 없습니다", MaxSnippetBytes)
	}
	switch c.Provider {
	case ProviderNone, ProviderMomento, ProviderGA4, ProviderGTM, ProviderMatomo, ProviderCustom:
	default:
		return fmt.Errorf("tracking.provider 는 %s 중 하나여야 합니다", strings.Join(Providers, ", "))
	}
	if !c.Enabled {
		return nil
	}
	switch c.Provider {
	case ProviderMomento:
		if originOf(c.MomentoURL) == "" || strings.TrimSpace(c.MomentoSiteID) == "" {
			return fmt.Errorf("Momento 수집기 주소(http(s)://…)와 사이트 id 가 필요합니다")
		}
	case ProviderGA4, ProviderGTM:
		if strings.TrimSpace(c.MeasurementID) == "" {
			return fmt.Errorf("GA4·GTM 에는 measurement id 가 필요합니다")
		}
	case ProviderMatomo:
		if originOf(c.MatomoURL) == "" || strings.TrimSpace(c.MatomoSiteID) == "" {
			return fmt.Errorf("Matomo 주소(http(s)://…)와 사이트 id 가 필요합니다")
		}
	case ProviderCustom:
		if strings.TrimSpace(c.CustomSnippet) == "" {
			return fmt.Errorf("붙여 넣은 추적 코드가 비어 있습니다")
		}
	}
	return nil
}

// Snippet renders the markup to inject. The nonce is applied to every script
// tag so the page policy can stay strict.
func (c Config) Snippet(nonce string) string {
	switch c.Provider {
	case ProviderMomento:
		site := html.EscapeString(strings.TrimSpace(c.MomentoSiteID))
		base := strings.TrimRight(strings.TrimSpace(c.MomentoURL), "/")
		if site == "" || originOf(base) == "" {
			return ""
		}
		if c.MomentoProxy {
			return withNonce(fmt.Sprintf(`<script async src="%s/tracker.js" data-site-id="%s" data-environment="prd" data-contract-version="1" data-endpoint="%s"></script>`,
				ProxyPath, site, ProxyPath), nonce)
		}
		return withNonce(fmt.Sprintf(`<script async src="%s/tracker.js" data-site-id="%s" data-environment="prd" data-contract-version="1"></script>`,
			html.EscapeString(base), site), nonce)
	case ProviderGA4:
		id := html.EscapeString(strings.TrimSpace(c.MeasurementID))
		if id == "" {
			return ""
		}
		return withNonce(fmt.Sprintf(`<script async src="https://www.googletagmanager.com/gtag/js?id=%s"></script>
<script>window.dataLayer=window.dataLayer||[];function gtag(){dataLayer.push(arguments);}gtag('js',new Date());gtag('config','%s');</script>`, id, id), nonce)
	case ProviderGTM:
		id := html.EscapeString(strings.TrimSpace(c.MeasurementID))
		if id == "" {
			return ""
		}
		return withNonce(fmt.Sprintf(`<script>(function(w,d,s,l,i){w[l]=w[l]||[];w[l].push({'gtm.start':new Date().getTime(),event:'gtm.js'});var f=d.getElementsByTagName(s)[0],j=d.createElement(s),dl=l!='dataLayer'?'&l='+l:'';j.async=true;j.src='https://www.googletagmanager.com/gtm.js?id='+i+dl;f.parentNode.insertBefore(j,f);})(window,document,'script','dataLayer','%s');</script>`, id), nonce)
	case ProviderMatomo:
		base := strings.TrimRight(strings.TrimSpace(c.MatomoURL), "/")
		site := html.EscapeString(strings.TrimSpace(c.MatomoSiteID))
		if originOf(base) == "" || site == "" {
			return ""
		}
		return withNonce(fmt.Sprintf(`<script>var _paq=window._paq=window._paq||[];_paq.push(['trackPageView']);_paq.push(['enableLinkTracking']);(function(){var u="%s/";_paq.push(['setTrackerUrl',u+'matomo.php']);_paq.push(['setSiteId','%s']);var d=document,g=d.createElement('script'),s=d.getElementsByTagName('script')[0];g.async=true;g.src=u+'matomo.js';s.parentNode.insertBefore(g,s);})();</script>`, html.EscapeString(base), site), nonce)
	case ProviderCustom:
		return withNonce(strings.TrimSpace(c.CustomSnippet), nonce)
	}
	return ""
}

// PolicySources lists the extra origins the snippet needs in script-src,
// connect-src and img-src. A common setup therefore needs no policy knowledge
// at all; Momento behind the proxy needs no extra origin whatsoever.
func (c Config) PolicySources() (scripts, connects, images []string) {
	add := func(origin string) {
		scripts = append(scripts, origin)
		connects = append(connects, origin)
		images = append(images, origin)
	}
	switch c.Provider {
	case ProviderMomento:
		if !c.MomentoProxy {
			if origin := originOf(c.MomentoURL); origin != "" {
				add(origin)
			}
		}
	case ProviderGA4, ProviderGTM:
		scripts = append(scripts, "https://www.googletagmanager.com")
		connects = append(connects, "https://www.google-analytics.com", "https://analytics.google.com", "https://*.google-analytics.com")
		images = append(images, "https://www.google-analytics.com", "https://www.googletagmanager.com")
	case ProviderMatomo:
		if origin := originOf(c.MatomoURL); origin != "" {
			add(origin)
		}
	case ProviderCustom:
		// A pasted snippet names the addresses it loads and reports to, so
		// those are allowed without anybody reading a policy error first.
		for _, origin := range SnippetOrigins(c.CustomSnippet) {
			add(origin)
		}
	}
	for _, host := range splitHosts(c.AllowedHosts) {
		add(host)
	}
	return scripts, connects, images
}

func splitHosts(list string) []string {
	var out []string
	for _, host := range strings.FieldsFunc(list, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t' }) {
		if host = strings.TrimSpace(host); host != "" {
			out = append(out, host)
		}
	}
	return out
}

// SnippetOrigins lists every http(s) origin written into a snippet: the
// script it loads, the endpoint it posts to, the pixel it requests.
func SnippetOrigins(snippet string) []string {
	origins := make([]string, 0, 2)
	seen := make(map[string]struct{}, 2)
	for index := 0; index < len(snippet); {
		start := indexFold(snippet[index:], "http")
		if start < 0 {
			break
		}
		start += index
		end := start
		for end < len(snippet) && !isURLBoundary(snippet[end]) {
			end++
		}
		index = end
		origin := originOf(snippet[start:end])
		if origin == "" {
			continue
		}
		if _, duplicate := seen[origin]; duplicate {
			continue
		}
		seen[origin] = struct{}{}
		origins = append(origins, origin)
	}
	return origins
}

// isURLBoundary reports the characters that cannot appear in a URL written
// inside HTML or JavaScript, which is where each address ends.
func isURLBoundary(letter byte) bool {
	switch letter {
	case '"', '\'', '`', '<', '>', ' ', '\t', '\n', '\r', ')', ',', ';', '\\', '+':
		return true
	}
	return false
}

// originOf returns scheme://host for an absolute http(s) URL and "" for
// anything else — relative paths, data: URLs, browser extensions.
func originOf(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return ""
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return ""
	}
	return scheme + "://" + strings.ToLower(parsed.Host)
}

// withNonce adds the nonce to every script tag that does not already carry
// one, which is what lets a pasted snippet run under a strict policy.
func withNonce(snippet, nonce string) string {
	if nonce == "" || snippet == "" {
		return snippet
	}
	var builder strings.Builder
	remaining := snippet
	for {
		index := indexFold(remaining, "<script")
		if index < 0 {
			builder.WriteString(remaining)
			return builder.String()
		}
		end := index + len("<script")
		builder.WriteString(remaining[:end])
		tag := remaining[end:]
		if closing := strings.IndexByte(tag, '>'); closing >= 0 {
			tag = tag[:closing]
		}
		if !containsFold(tag, "nonce=") {
			builder.WriteString(` nonce="` + html.EscapeString(nonce) + `"`)
		}
		remaining = remaining[end:]
	}
}

// indexFold finds sub in s ignoring ASCII case and returns an index into s.
//
// strings.ToLower is the obvious way and the wrong one: it changes byte lengths
// for some runes — U+212A KELVIN SIGN is three bytes and folds to a one-byte
// 'k', U+0130 'İ' is two bytes and folds to three — so an index taken from the
// folded copy lands somewhere else in the original and the nonce is written
// into the middle of the tag name. Every needle here is ASCII, and folding
// only ASCII keeps every byte in place.
func indexFold(s, sub string) int {
	if len(sub) == 0 {
		return 0
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			if foldASCII(s[i+j]) != foldASCII(sub[j]) {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func containsFold(s, sub string) bool { return indexFold(s, sub) >= 0 }

func foldASCII(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

// AddAllowedHost appends an origin to the comma separated allow list, leaving
// the existing entries and their order alone.
func AddAllowedHost(existing, origin string) string {
	origin = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(origin), "/"))
	if origin == "" {
		return existing
	}
	for _, host := range splitHosts(existing) {
		if strings.EqualFold(host, origin) {
			return existing
		}
	}
	if strings.TrimSpace(existing) == "" {
		return origin
	}
	return strings.TrimSpace(existing) + ", " + origin
}

package mailhtml

import (
	"net/url"
	"strings"
	"testing"
)

func TestImagePoliciesAndProxyAreDeterministic(t *testing.T) {
	raw := `<p>Logo<img src="https://assets.example/logo.png?x=1&amp;y=2" alt="회사"><img src="cid:postra-att_abc@postra.local" alt="로컬"><img src="data:image/png;base64,eA=="></p>`
	blocked := SanitizeWithImages(raw, "block", "")
	if strings.Contains(blocked, "assets.example") || !strings.Contains(blocked, "cid:postra-att_abc") || strings.Contains(blocked, "data:image") {
		t.Fatalf("block policy: %s", blocked)
	}
	allowed := SanitizeWithImages(raw, "allow", "")
	if !strings.Contains(allowed, `data-postra-external-image="allow"`) || len(RemoteImageSources(allowed)) != 1 {
		t.Fatalf("allow policy: %s", allowed)
	}
	proxy := "https://proxy.corp.local/image?workspace=postra"
	proxied := SanitizeWithImages(raw, "proxy", proxy)
	sources := RemoteImageSources(proxied)
	if len(sources) != 1 || !IsProxiedImage(sources[0], proxy) {
		t.Fatalf("proxy policy: %s", proxied)
	}
	u, _ := url.Parse(sources[0])
	if u.Query().Get("url") != "https://assets.example/logo.png?x=1&y=2" || u.Query().Get("workspace") != "postra" {
		t.Fatalf("proxy target changed: %s", sources[0])
	}
	if twice := SanitizeWithImages(proxied, "proxy", proxy); twice != proxied {
		t.Fatalf("proxy wrapped twice:\n%s\n%s", proxied, twice)
	}
	if strings.Contains(SanitizeApprovedOutbound(raw), "assets.example") || !strings.Contains(SanitizeApprovedOutbound(allowed), "assets.example") {
		t.Fatal("legacy image authorization changed")
	}
	for _, bad := range []string{"http://proxy.example/image", "https://user:password@proxy.example/image", "javascript:evil()", "/proxy", "https://proxy.example/#fragment"} {
		if ValidImageProxy(bad) || strings.Contains(SanitizeWithImages(raw, "proxy", bad), "assets.example") {
			t.Fatalf("unsafe proxy accepted %s", bad)
		}
	}
}

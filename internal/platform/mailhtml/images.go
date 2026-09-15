package mailhtml

import (
	"golang.org/x/net/html"
	"net/url"
	"strings"
)

func validRemoteImage(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil && !strings.ContainsAny(raw, "\r\n\x00")
}

func ValidImageProxy(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.Fragment == ""
}

func IsProxiedImage(raw, proxy string) bool {
	if !ValidImageProxy(proxy) {
		return false
	}
	target, err := url.Parse(raw)
	if err != nil {
		return false
	}
	base, _ := url.Parse(proxy)
	if target.Scheme != base.Scheme || target.Host != base.Host || target.Path != base.Path || target.Fragment != "" || !validRemoteImage(target.Query().Get("url")) {
		return false
	}
	for key, values := range base.Query() {
		if key == "url" {
			continue
		}
		actual := target.Query()[key]
		if strings.Join(actual, "\x00") != strings.Join(values, "\x00") {
			return false
		}
	}
	return true
}

// SanitizeWithImages never fetches images or acts as an open proxy. In proxy
// mode it only creates an operator-selected HTTPS URL with an encoded target.
func SanitizeWithImages(raw, mode, proxy string) string {
	if mode != "allow" && mode != "proxy" {
		return Sanitize(raw)
	}
	safe := SanitizeOutbound(raw)
	if mode == "proxy" && !ValidImageProxy(proxy) {
		return Sanitize(raw)
	}
	tokens := html.NewTokenizer(strings.NewReader(safe))
	var out strings.Builder
	for {
		kind := tokens.Next()
		if kind == html.ErrorToken {
			break
		}
		rawToken := string(tokens.Raw())
		if kind == html.StartTagToken || kind == html.SelfClosingTagToken {
			token := tokens.Token()
			if token.Data == "img" {
				remote := false
				for i, attr := range token.Attr {
					if attr.Key == "src" && validRemoteImage(attr.Val) {
						remote = true
					}
					if mode == "proxy" && attr.Key == "src" && validRemoteImage(attr.Val) && !IsProxiedImage(attr.Val, proxy) {
						target, _ := url.Parse(proxy)
						query := target.Query()
						query.Set("url", attr.Val)
						target.RawQuery = query.Encode()
						token.Attr[i].Val = target.String()
					}
				}
				if remote {
					attrs := token.Attr[:0]
					for _, attr := range token.Attr {
						if attr.Key != "data-postra-external-image" {
							attrs = append(attrs, attr)
						}
					}
					token.Attr = append(attrs, html.Attribute{Key: "data-postra-external-image", Val: mode})
				}
				rawToken = token.String()
			}
		}
		out.WriteString(rawToken)
	}
	return out.String()
}

// Legacy/imported drafts never had authorization to retain remote images.
// Only images produced by the policy-aware compose path may reach SMTP;
// tightening that policy is then detected by the final application gate.
func SanitizeApprovedOutbound(raw string) string {
	safe := SanitizeOutbound(raw)
	tokens := html.NewTokenizer(strings.NewReader(safe))
	var out strings.Builder
	for {
		kind := tokens.Next()
		if kind == html.ErrorToken {
			break
		}
		rawToken := string(tokens.Raw())
		keep := true
		if kind == html.StartTagToken || kind == html.SelfClosingTagToken {
			token := tokens.Token()
			if token.Data == "img" {
				remote, marked := false, false
				for _, attr := range token.Attr {
					if attr.Key == "src" && validRemoteImage(attr.Val) {
						remote = true
					}
					if attr.Key == "data-postra-external-image" && (attr.Val == "allow" || attr.Val == "proxy") {
						marked = true
					}
				}
				keep = !remote || marked
			}
		}
		if keep {
			out.WriteString(rawToken)
		}
	}
	return out.String()
}

func RemoteImageSources(raw string) []string {
	var sources []string
	tokens := html.NewTokenizer(strings.NewReader(SanitizeOutbound(raw)))
	for {
		kind := tokens.Next()
		if kind == html.ErrorToken {
			break
		}
		if kind != html.StartTagToken && kind != html.SelfClosingTagToken {
			continue
		}
		token := tokens.Token()
		if token.Data != "img" {
			continue
		}
		for _, attr := range token.Attr {
			if attr.Key == "src" && validRemoteImage(attr.Val) {
				sources = append(sources, attr.Val)
			}
		}
	}
	return sources
}

func ImageMarkup(raw string) string {
	tokens := html.NewTokenizer(strings.NewReader(SanitizeOutbound(raw)))
	var out strings.Builder
	for {
		kind := tokens.Next()
		if kind == html.ErrorToken {
			break
		}
		if kind != html.StartTagToken && kind != html.SelfClosingTagToken {
			continue
		}
		token := tokens.Token()
		if token.Data == "img" {
			out.WriteString(token.String())
		}
	}
	return out.String()
}

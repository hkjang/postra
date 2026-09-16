package application

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"

	"postra/internal/domain"
)

// discoveryServer serves a discovery document shaped by handler at the
// well-known path and 404s everything else, mirroring a real issuer.
func discoveryServer(t *testing.T, tlsMode bool, handler func(w http.ResponseWriter, self string)) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		handler(w, srv.URL)
	})
	if tlsMode {
		srv = httptest.NewTLSServer(h)
	} else {
		srv = httptest.NewServer(h)
	}
	t.Cleanup(srv.Close)
	return srv
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestOIDCDiscoveryGuidanceClassifiesWithoutEchoingProvider(t *testing.T) {
	const leak = "provider-controlled-private-text-5137"
	discoveryJSON := func(self string) []byte {
		b, _ := json.Marshal(map[string]any{"issuer": self, "authorization_endpoint": self + "/auth", "token_endpoint": self + "/token", "jwks_uri": self + "/keys"})
		return b
	}
	for _, tc := range []struct {
		name string
		err  error // constructed errors for causes that depend on the network
		srv  func(t *testing.T) *httptest.Server
		want string
	}{
		{name: "issuer_mismatch", want: "issuer 가 설정된 Issuer URL 과 다릅니다", srv: func(t *testing.T) *httptest.Server {
			return discoveryServer(t, false, func(w http.ResponseWriter, self string) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(discoveryJSON("https://" + leak + ".invalid"))
			})
		}},
		{name: "not_json", want: "OpenID 설정 JSON 이 아닙니다", srv: func(t *testing.T) *httptest.Server {
			return discoveryServer(t, false, func(w http.ResponseWriter, self string) {
				w.Header().Set("Content-Type", "text/html")
				_, _ = w.Write([]byte("<html><title>" + leak + "</title></html>"))
			})
		}},
		{name: "http_status", want: "HTTP 404", srv: func(t *testing.T) *httptest.Server {
			return discoveryServer(t, false, func(w http.ResponseWriter, self string) {
				http.Error(w, leak, http.StatusNotFound)
			})
		}},
		{name: "untrusted_certificate", want: "TLS 인증서를 검증하지 못했습니다", srv: func(t *testing.T) *httptest.Server {
			return discoveryServer(t, true, func(w http.ResponseWriter, self string) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(discoveryJSON(self))
			})
		}},
		{name: "connection_refused", want: "연결을 거부했습니다", srv: func(t *testing.T) *httptest.Server {
			srv := discoveryServer(t, false, func(w http.ResponseWriter, self string) {})
			srv.Close() // the port is now closed; the URL stays reachable-looking
			return srv
		}},
		{name: "dns", want: "호스트 이름을 찾을 수 없습니다", err: &url.Error{Op: "Get", URL: "https://idp." + leak, Err: &net.DNSError{Err: "no such host", Name: leak, IsNotFound: true}}},
		{name: "timeout", want: "시간 안에 끝나지 않았습니다", err: &url.Error{Op: "Get", URL: "https://idp." + leak, Err: timeoutErr{}}},
		{name: "refused_wrapped", want: "연결을 거부했습니다", err: &url.Error{Op: "Get", URL: "https://idp." + leak, Err: &net.OpError{Op: "dial", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}}},
		{name: "other_transport", want: "인증 서버에 연결하지 못했습니다", err: &url.Error{Op: "Get", URL: "https://idp." + leak, Err: errors.New(leak)}},
		{name: "unknown", want: "Issuer URL, 인증 서버 연결과 TLS 인증서", err: errors.New(leak)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			if tc.srv != nil {
				app, _, _, _ := newTestApp(t)
				srv := tc.srv(t)
				app.Cfg.Auth.OIDCIssuer = srv.URL
				app.Cfg.Auth.OIDCClientID = "postra"
				app.Cfg.Auth.OIDCRedirectURL = "https://postra.invalid/auth/oidc/callback"
				_, _, err := app.BeginOIDC(context.Background(), OIDCStartOptions{})
				if err == nil {
					t.Fatal("broken discovery was accepted")
				}
				got = err.Error()
				incidents, listErr := app.Store.ListIncidents(context.Background(), domain.IncidentFilter{})
				if listErr != nil {
					t.Fatal(listErr)
				}
				if len(incidents) != 1 || incidents[0].Component != "oidc" || incidents[0].Message != "OIDC Discovery 실패" || !strings.Contains(incidents[0].Detail, tc.want) || !strings.Contains(incidents[0].Detail, srv.URL) {
					t.Fatalf("discovery failure was not recorded as one classified incident naming the issuer: %+v", incidents)
				}
				if strings.Contains(incidents[0].Detail, leak) {
					t.Fatal("provider-controlled text leaked into the incident")
				}
			} else {
				got = oidcDiscoveryGuidance(tc.err)
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("guidance %q does not name the cause %q", got, tc.want)
			}
			if strings.Contains(got, leak) {
				t.Fatalf("provider-controlled text leaked into guidance: %q", got)
			}
		})
	}
	// A verification failure wrapped the way crypto/tls reports it is
	// classified as a certificate problem even without a live server.
	wrapped := &url.Error{Op: "Get", URL: "https://idp.invalid", Err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}}
	if !strings.Contains(oidcDiscoveryGuidance(wrapped), "TLS 인증서") {
		t.Fatal("wrapped certificate verification error was not classified")
	}
}

func TestRecordOIDCProviderErrorSeparatesKnownCodesAndDropsUnknown(t *testing.T) {
	const unknown = "provider-private-error-code-9931"
	app, _, _, _ := newTestApp(t)
	ctx := context.Background()
	app.RecordOIDCProviderError("invalid_scope")
	app.RecordOIDCProviderError("invalid_scope")
	app.RecordOIDCProviderError("access_denied")
	app.RecordOIDCProviderError(unknown)
	incidents, err := app.Store.ListIncidents(ctx, domain.IncidentFilter{})
	if err != nil {
		t.Fatal(err)
	}
	bySuffix := map[string]domain.Incident{}
	for _, inc := range incidents {
		if inc.Component != "oidc" {
			t.Fatalf("unexpected component: %+v", inc)
		}
		bySuffix[strings.TrimPrefix(inc.Message, "OIDC 인증 서버가 로그인 요청을 거절했습니다")] = inc
	}
	if len(incidents) != 3 {
		t.Fatalf("expected one row per known code plus one for unknown codes, got %d: %+v", len(incidents), incidents)
	}
	scope, ok := bySuffix[" (invalid_scope)"]
	if !ok || scope.Count != 2 || scope.Severity != domain.SeverityError || !strings.Contains(scope.Detail, "groups") {
		t.Fatalf("repeated client misconfiguration did not fold into one error row: %+v", scope)
	}
	denied, ok := bySuffix[" (access_denied)"]
	if !ok || denied.Severity != domain.SeverityWarning {
		t.Fatalf("a user's denial should be a separate warning row: %+v", denied)
	}
	generic, ok := bySuffix[""]
	if !ok || strings.Contains(generic.Message, unknown) || strings.Contains(generic.Detail, unknown) {
		t.Fatalf("unknown provider code leaked into the incident: %+v", generic)
	}
}

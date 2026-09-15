package application

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"postra/internal/domain"
)

func TestOIDCProviderFailuresNeverExposeCredentials(t *testing.T) {
	const clientSecret = "fixture-private-client-secret-4921"
	const authorizationCode = "fixture-private-authorization-code-7832"
	const upstreamJWT = "fixture-private-jwt-value-3718"
	const arbitraryCode = "provider-private-error-code-6284"
	leak := strings.Join([]string{clientSecret, authorizationCode, upstreamJWT}, " ")
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, code, want string
	}{
		{"invalid_client", "invalid_client", "invalid_client"},
		{"invalid_grant", "invalid_grant", "PKCE"},
		{"invalid_scope", "invalid_scope", "invalid_scope"},
		{"unknown_error", arbitraryCode, "Client 설정"},
		{"raw_error_body", "", "Client 설정"},
		{"invalid_audience", "", "서명/JWKS"},
		{"discovery", "", "Issuer URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, _, _, _ := newTestApp(t)
			var issuer *httptest.Server
			issuer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/.well-known/openid-configuration":
					if tc.name == "discovery" {
						http.Error(w, leak, http.StatusBadGateway)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer.URL, "authorization_endpoint": issuer.URL + "/auth", "token_endpoint": issuer.URL + "/token", "jwks_uri": issuer.URL + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}})
				case "/keys":
					_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "fixture-key", Algorithm: "RS256", Use: "sig"}}})
				case "/token":
					if tc.name == "raw_error_body" {
						http.Error(w, leak, http.StatusBadGateway)
						return
					}
					if tc.name != "invalid_audience" {
						w.WriteHeader(http.StatusBadRequest)
						_ = json.NewEncoder(w).Encode(map[string]string{"error": tc.code, "error_description": leak, "error_uri": "https://idp.invalid/" + upstreamJWT})
						return
					}
					signer, signErr := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "fixture-key"))
					if signErr != nil {
						http.Error(w, "fixture signer unavailable", http.StatusInternalServerError)
						return
					}
					claims, _ := json.Marshal(map[string]any{"iss": issuer.URL, "sub": "fixture-subject", "aud": leak, "exp": time.Now().Add(time.Minute).Unix(), "nonce": "fixture-nonce"})
					signed, signErr := signer.Sign(claims)
					if signErr != nil {
						http.Error(w, "fixture signing failed", http.StatusInternalServerError)
						return
					}
					jwt, _ := signed.CompactSerialize()
					_ = json.NewEncoder(w).Encode(map[string]string{"access_token": upstreamJWT, "token_type": "Bearer", "id_token": jwt})
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(issuer.Close)
			app.Cfg.Auth.OIDCIssuer = issuer.URL
			app.Cfg.Auth.OIDCClientID = "fixture-client"
			app.Cfg.Auth.OIDCClientSecret = clientSecret
			app.Cfg.Auth.OIDCRedirectURL = "https://postra.invalid/auth/oidc/callback"
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(previous)
			_, loginErr := app.CompleteOIDC(context.Background(), authorizationCode, OIDCFlow{Nonce: "fixture-nonce", CodeVerifier: "fixture-verifier"})
			if loginErr == nil || !strings.Contains(loginErr.Error(), tc.want) {
				t.Fatal("provider failure did not return fixed actionable guidance")
			}
			incidents, err := app.Store.ListIncidents(context.Background(), domain.IncidentFilter{})
			if err != nil {
				t.Fatal(err)
			}
			if tc.name != "discovery" && len(incidents) != 1 {
				t.Fatalf("expected one sanitized OIDC incident, got %d", len(incidents))
			}
			encoded, _ := json.Marshal(incidents)
			outputs := []string{loginErr.Error(), string(encoded), logs.String()}
			if tc.name == "discovery" {
				_, _, startErr := app.BeginOIDC(context.Background(), OIDCStartOptions{})
				if startErr == nil {
					t.Fatal("invalid discovery was accepted")
				}
				outputs = append(outputs, startErr.Error())
			}
			for _, output := range outputs {
				for _, marker := range []string{clientSecret, authorizationCode, upstreamJWT, arbitraryCode} {
					if strings.Contains(output, marker) {
						t.Fatal("provider-controlled credential or unknown code leaked into public error, incident or log")
					}
				}
			}
			if tc.code != "" && tc.code != arbitraryCode && (!strings.Contains(string(encoded), tc.code) || !strings.Contains(logs.String(), tc.code)) {
				t.Fatal("allowlisted OAuth code was lost from sanitized diagnostics")
			}
		})
	}
}

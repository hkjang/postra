package application

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"postra/internal/domain"
)

func TestAIProbeSeparatesProviderRejections(t *testing.T) {
	for _, target := range []string{"ai", "embedding"} {
		for _, test := range []struct {
			name, body, message string
			status              int
		}{
			{"authentication", diagnosticSecret, providerAIAuth, 401},
			{"permission", diagnosticSecret, providerAIForbidden, 403},
			{"rate", diagnosticSecret, providerAIRate, 429},
			{"quota", `{"error":{"code":"insufficient_quota","message":"` + diagnosticSecret + `"}}`, providerAIQuota, 429},
			{"quota_message_is_not_a_code", `{"error":{"message":"insufficient_quota ` + diagnosticSecret + `"}}`, providerAIRate, 429},
			{"invalid_request", diagnosticSecret, providerAIRejected, 422},
		} {
			t.Run(target+"/"+test.name, func(t *testing.T) {
				app, _, _, _ := newTestApp(t)
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.Method != http.MethodPost {
						t.Error("manual-limit connectivity probe made an extra request")
					}
					w.WriteHeader(test.status)
					_, _ = w.Write([]byte(test.body))
				}))
				defer server.Close()
				result, err := app.ProbeSettings(settingsAdmin(), SettingsProbe{Target: target, Values: map[string]string{
					"ai.base_url": server.URL, "ai.embed_base_url": server.URL, "ai.auto_context_length": "false",
				}})
				if err != nil || result.OK || result.Message != test.message || calls.Load() != 1 {
					t.Fatalf("incorrect classification or duplicate call: %+v err=%v calls=%d", result, err, calls.Load())
				}
				assertNoProviderEcho(t, result)
				audit, err := app.Store.SearchAudit(settingsAdmin(), DefaultUserID, 20)
				if err != nil {
					t.Fatal(err)
				}
				assertNoProviderEcho(t, audit)
			})
		}
	}
}

func TestModelLimitProbeExplainsMetadataFailure(t *testing.T) {
	for _, test := range []struct {
		name, body, reason, guidance string
		status                       int
	}{
		{"authentication", diagnosticSecret, "auth_failed", "인증에 실패", 401},
		{"permission", diagnosticSecret, "forbidden", "권한이 없습니다", 403},
		{"unsupported", diagnosticSecret, "not_supported", "지원하지 않거나", 404},
		{"rate", diagnosticSecret, "rate_limited", "요청 제한", 429},
		{"server", diagnosticSecret, "server_error", "오류를 반환", 500},
		{"malformed", diagnosticSecret, "invalid_response", "응답 형식", 200},
		{"no_limits", `{"data":[{"id":"selected"}]}`, "no_metadata", "한도를 제공하지", 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, _, _, _ := newTestApp(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/models" {
					t.Error("metadata probe tried to generate content")
				}
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			for _, target := range []string{"ai_models", "embedding_models"} {
				result, err := app.ProbeSettings(settingsAdmin(), SettingsProbe{Target: target, Values: map[string]string{
					"ai.base_url": server.URL, "ai.embed_base_url": server.URL, "ai.model": "selected", "ai.embed_model": "selected",
				}})
				if err != nil || result.OK || result.Limits == nil || result.Limits.Reason != test.reason || !strings.Contains(result.Message, test.guidance) {
					t.Fatalf("metadata failure was not explained: %+v limits=%+v err=%v", result, result.Limits, err)
				}
				assertNoProviderEcho(t, result)
				if target == "embedding_models" && (result.Limits.ContextLength != 0 || !strings.Contains(result.Message, "Chat Context 설정을 대신 적용하지")) {
					t.Fatal("unknown embedding cap was replaced with Chat cap")
				}
			}
		})
	}
}

func TestModelLimitDiagnosticUnknownReasonsAndMissingEmbeddingModel(t *testing.T) {
	for _, reason := range []string{"timeout", "unreachable", "request_rejected", diagnosticSecret} {
		message := modelLimitsDiagnostic(domain.AIModelLimits{ContextLength: 8192, Status: "unavailable", Reason: reason})
		assertNoProviderEcho(t, message)
		if !strings.Contains(message, "대체 한도") {
			t.Fatal("lost safe Context fallback guidance")
		}
	}
	message := modelLimitsDiagnostic(domain.AIModelLimits{Status: "model_not_found"})
	if !strings.Contains(message, "선택한 ID를 찾지 못") || !strings.Contains(message, "임베딩 입력 한도는 미확인") {
		t.Fatal("unknown embedding limit hid the missing-model diagnosis")
	}
}

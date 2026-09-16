package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"

	"postra/internal/domain"
)

// Provider replies are an untrusted channel. Error codes and recovery advice
// are locally defined; response bodies, headers and URLs never escape here.
// Upstream authentication errors use 502, not 401/403: a provider credential
// failure must not invalidate the user's Postra login session.
func providerError(code string) error {
	status, message := http.StatusBadGateway, "AI 요청이 거부되었습니다. 모델과 API 호환 설정을 확인하세요."
	switch code {
	case "ai_auth_failed":
		message = "AI 서버 인증에 실패했습니다. 관리자에게 API Key와 인증 설정 확인을 요청하세요."
	case "ai_forbidden":
		message = "AI 서버가 접근을 거부했습니다. 모델 사용 권한과 서버 접근 정책을 확인하세요."
	case "ai_rate_limited":
		status, message = http.StatusTooManyRequests, "AI 서버의 요청 한도에 도달했습니다. 잠시 후 다시 시도하세요. 자동으로 재요청하지 않았습니다."
	case "ai_quota_exceeded":
		status, message = http.StatusTooManyRequests, "AI 서버의 사용 할당량이 소진되었습니다. 관리자에게 계정 한도와 결제 설정 확인을 요청하세요."
	case "ai_timeout":
		status, message = http.StatusGatewayTimeout, "AI 서버 응답 시간이 초과되었습니다. 중복 생성을 방지하기 위해 자동으로 재요청하지 않았습니다."
	case "ai_unreachable":
		message = "AI 서버에 연결할 수 없습니다. 서버 주소·네트워크·TLS 설정을 확인하세요."
	}
	return &domain.PublicError{Code: code, Message: message, Status: status}
}

func providerHTTPError(status int, body []byte) error {
	switch status {
	case http.StatusUnauthorized:
		return providerError("ai_auth_failed")
	case http.StatusForbidden:
		return providerError("ai_forbidden")
	case http.StatusTooManyRequests:
		var response struct {
			Error struct {
				Code string `json:"code"`
				Type string `json:"type"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &response) == nil {
			// Billing and spend/usage limits are not transient request throttling.
			// Keep an exact allowlist and the compatible legacy error type; never
			// classify by a free-form provider message or substring.
			switch response.Error.Code {
			case "insufficient_quota", "credit_balance_exhausted", "organization_spend_limit_exceeded", "project_spend_limit_exceeded", "organization_usage_limit_exceeded":
				return providerError("ai_quota_exceeded")
			}
			if response.Error.Type == "insufficient_quota" {
				return providerError("ai_quota_exceeded")
			}
		}
		return providerError("ai_rate_limited")
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return providerError("ai_timeout")
	default:
		if status >= 500 && status <= 599 {
			// Preserve configured task-to-default fallback for a server rejection,
			// while keeping its arbitrary upstream body out of logs and clients.
			return errors.New("AI server is temporarily unavailable")
		}
		return providerError("ai_request_rejected")
	}
}

func providerNetworkError(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if providerNetworkReason(err) == "timeout" || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return providerError("ai_timeout")
	}
	return providerError("ai_unreachable")
}

func providerNetworkReason(err error) string {
	var network net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &network) && network.Timeout()) {
		return "timeout"
	}
	return "unreachable"
}

func credentialError(ctx context.Context, err error) error {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return providerNetworkError(ctx, err)
	}
	return providerError("ai_auth_failed")
}

func metadataHTTPReason(status int) string {
	switch status {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return "not_supported"
	case http.StatusUnauthorized:
		return "auth_failed"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusTooManyRequests:
		return "rate_limited"
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return "timeout"
	default:
		if status >= 500 && status <= 599 {
			return "server_error"
		}
		return "request_rejected"
	}
}

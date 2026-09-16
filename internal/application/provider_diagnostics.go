package application

import (
	"context"
	"errors"
	"net"

	"postra/internal/domain"
)

// Provider diagnostics are an untrusted channel: an SMTP/IMAP server or AI
// adapter can echo passwords, Authorization headers, or message data in an
// error. Never persist/serialize that text, even inside a successful DTO.
// Fixed messages retain useful recovery guidance without guessing secret
// patterns. Historical rows pass the same read boundary without DB rewrites.
const (
	providerFailed      = "외부 서비스 요청에 실패했습니다. 서버 연결·인증·정책 설정을 확인하세요."
	providerAuthFailed  = "메일 인증에 실패했습니다. 관리자에게 메일 계정 비밀값 확인을 요청하세요."
	providerTimeout     = "서비스 응답 시간이 초과되었습니다. 작업·발송 상태를 확인한 뒤 다시 시도하세요."
	providerCancelled   = "작업이 취소되었습니다."
	providerUnexpected  = "작업 중 내부 오류가 발생했습니다. 관리자에게 작업 ID를 전달하세요."
	providerListFailed  = "메일 목록을 가져오지 못했습니다. 서버 상태와 계정 권한을 확인하세요."
	providerSyncFailed  = "메일 동기화에 실패했습니다. 서버 연결·인증 설정과 계정 상태를 확인하세요."
	providerEmbedFailed = "메일 색인 작업에 실패했습니다. AI 연결과 벡터 저장소 설정을 확인하세요."
	providerHidden      = "서버 진단 내용은 보안을 위해 숨겼습니다. 작업 상태와 통계를 확인하세요."
	providerAIContext   = "AI 입력이 모델 Context 한도를 초과했습니다. 입력 범위를 줄이거나 관리자에서 모델 한도 자동 감지·대체 한도를 확인하세요."
	providerAIOutput    = "AI 출력이 토큰 한도에서 중단되었습니다. 연결·인증 실패가 아닙니다. 출력 상한을 확인하거나 더 짧은 답변을 요청하세요."
	providerAIEmpty     = "AI 서버에 연결했으나 사용할 텍스트가 없습니다. 모델의 생성 방식과 출력 토큰 설정을 확인하세요."
	providerAIDisabled  = "관리자가 비활성화한 AI 모델입니다. 사용할 모델을 확인하세요."
	providerAIEmbedding = "임베딩 응답의 개수·순서·벡터 형식이 올바르지 않아 저장하지 않았습니다. 모델과 임베딩 서버를 확인하세요."
	providerAIResponse  = "AI 응답이 중간에 끊기거나 형식이 올바르지 않아 사용하지 않았습니다. 서버 상태를 확인한 뒤 다시 시도하세요."
	providerAIAuth      = "AI 서버 인증에 실패했습니다. API Key 또는 게이트웨이 인증 설정을 확인하세요. Postra 로그인 세션 문제는 아닙니다."
	providerAIForbidden = "AI 서버에서 모델 접근을 허용하지 않았습니다. 모델 권한과 게이트웨이 정책을 확인하세요."
	providerAIRate      = "AI 서버의 요청 제한에 도달했습니다. 자동으로 다른 모델을 호출하지 않았습니다. 잠시 후 다시 시도하세요."
	providerAIQuota     = "AI 계정의 사용 한도에 도달했습니다. 관리자에게 할당량을 확인하세요. 자동으로 다른 모델을 호출하지 않았습니다."
	providerAITimeout   = "AI 서버 응답 시간이 초과되었습니다. 중복 처리를 피하기 위해 자동으로 다시 호출하지 않았습니다. 서버 상태와 Timeout 설정을 확인하세요."
	providerAINetwork   = "AI 서버에 연결할 수 없습니다. Endpoint·내부 DNS·네트워크·인증서를 확인하세요."
	providerAIRejected  = "AI 서버가 요청을 거부했습니다. 모델 이름·요청 형식·API 호환성을 확인하세요."
)

func providerDiagnostic(err error) string {
	if err == nil {
		return ""
	}
	var auth *domain.AuthError
	var public *domain.PublicError
	var network net.Error
	// Only recognize locally defined codes, never trust Message/Details even
	// when an adapter wraps an upstream response in a PublicError.
	if errors.As(err, &public) {
		switch public.Code {
		case "context_limit":
			return providerAIContext
		case "output_limit":
			return providerAIOutput
		case "empty_response":
			return providerAIEmpty
		case "model_disabled":
			return providerAIDisabled
		case "invalid_embedding":
			return providerAIEmbedding
		case "invalid_response":
			return providerAIResponse
		case "ai_auth_failed":
			return providerAIAuth
		case "ai_forbidden":
			return providerAIForbidden
		case "ai_rate_limited":
			return providerAIRate
		case "ai_quota_exceeded":
			return providerAIQuota
		case "ai_timeout":
			return providerAITimeout
		case "ai_unreachable":
			return providerAINetwork
		case "ai_request_rejected":
			return providerAIRejected
		}
	}
	switch {
	case errors.Is(err, context.Canceled):
		return providerCancelled
	case errors.Is(err, context.DeadlineExceeded):
		return providerTimeout
	case errors.As(err, &auth):
		return providerAuthFailed
	case errors.As(err, &network) && network.Timeout():
		return providerTimeout
	default:
		return providerFailed
	}
}

func safeOutbound(out *domain.OutboundMessage) *domain.OutboundMessage {
	if out == nil {
		return nil
	}
	copy := *out
	copy.SMTPResponse = outboundDiagnostic(copy.Status)
	return &copy
}

func outboundDiagnostic(status domain.OutboundStatus) string {
	switch status {
	case domain.OutboundQueued:
		return ""
	case domain.OutboundSent:
		return "SMTP 서버가 메시지를 수락했습니다."
	case domain.OutboundRetryWait:
		return "일시적인 발송 실패로 재시도 대기 중입니다. 중복 발송을 요청하지 말고 결과를 확인하세요."
	case domain.OutboundUncertain:
		return "서버 수락 여부를 확인할 수 없습니다. 자동 재시도하지 않으므로 관리자에게 발송 ID를 전달해 확인하세요."
	case domain.OutboundFailed:
		return "메일 발송에 실패했습니다. 서버 연결·인증·수신자 및 발송 정책을 확인하세요."
	default:
		return "발송 상태를 확인할 수 없습니다. 관리자에게 발송 ID를 전달하세요."
	}
}

func safeJob(job *domain.Job) *domain.Job {
	if job == nil {
		return nil
	}
	copy := *job
	copy.Error = jobDiagnostic(copy.Type, copy.Status, copy.Error)
	return &copy
}

func jobDiagnostic(kind string, status domain.JobStatus, message string) string {
	// Only exact application-owned literals survive; never preserve a server
	// message merely because it begins with a known error prefix.
	switch message {
	case providerFailed, providerAuthFailed, providerTimeout, providerCancelled,
		providerUnexpected, providerListFailed, providerSyncFailed, providerEmbedFailed, providerHidden,
		providerAIContext, providerAIOutput, providerAIEmpty, providerAIDisabled, providerAIEmbedding, providerAIResponse,
		providerAIAuth, providerAIForbidden, providerAIRate, providerAIQuota, providerAITimeout, providerAINetwork, providerAIRejected:
		return message
	}
	if status == domain.JobCancelled {
		return providerCancelled
	}
	if status == domain.JobFailed || status == domain.JobPartial {
		if kind == "sync" {
			return providerSyncFailed
		}
		if kind == "embed" {
			return providerEmbedFailed
		}
		return providerFailed
	}
	if message != "" {
		return providerHidden
	}
	return ""
}

func safeConnectionDiagnostics(diag domain.ConnDiagnostics, target string) domain.ConnDiagnostics {
	diag.Target = target
	diag.Steps = append([]domain.ConnStep(nil), diag.Steps...)
	for i := range diag.Steps {
		step := &diag.Steps[i]
		switch step.Step {
		case "dns", "secret_acquire", "protocol", "connect", "connect_tls_auth", "smtp_ehlo_auth", "uidl":
		default:
			step.Step = "connect"
		}
		step.Detail = ""
		if !step.OK {
			switch step.Step {
			case "dns":
				step.Detail = "서버 주소를 확인할 수 없습니다. 호스트 설정과 내부 DNS를 확인하세요."
			case "secret_acquire":
				step.Detail = "등록된 메일 비밀값을 사용할 수 없습니다. 관리자에게 재등록을 요청하세요."
			case "uidl":
				step.Detail = providerListFailed
			default:
				step.Detail = providerFailed
			}
		}
	}
	return diag
}

func safeAuditDiagnostic(event domain.AuditEvent) domain.AuditEvent {
	if event.Result != "error" && event.Result != "failed" && event.Result != "uncertain" {
		return event // business changes, policy decisions and counters remain intact
	}
	switch event.Action {
	case "mail_send":
		status := domain.OutboundFailed
		if event.Result == "uncertain" {
			status = domain.OutboundUncertain
		}
		event.Detail = outboundDiagnostic(status)
	case "ai_analysis", "embed_batch_failed", "oidc_initial_sync", "oidc_mail_auto_provision", "secret_register", "secret_rotate":
		event.Detail = providerFailed
	}
	return event
}

func safeIncidentDiagnostic(incident *domain.Incident) *domain.Incident {
	if incident == nil {
		return nil
	}
	copy := *incident
	switch copy.Component {
	case "sync", "sync-worker":
		copy.Message = jobDiagnostic("sync", domain.JobFailed, copy.Message)
	case "embeddings":
		copy.Message = jobDiagnostic("embed", domain.JobFailed, copy.Message)
	case "oidc", "oidc-mail":
		copy.Message = "SSO 연결 또는 메일 프로비저닝에 실패했습니다. 인증 설정과 연결 상태를 확인하세요."
	default:
		return &copy
	}
	copy.Detail = "" // historical provider chains/stack payloads are not trusted
	return &copy
}

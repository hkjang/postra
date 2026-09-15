package application

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"postra/internal/domain"
	"postra/internal/platform/mask"
)

type requestTraceKey struct{}

var publicTraceID = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// WithRequestTrace accepts bounded opaque IDs, never arbitrary header values.
// Passing an empty or malformed ID generates an independent server trace.
func WithRequestTrace(ctx context.Context, id string) context.Context {
	if !publicTraceID.MatchString(id) {
		id = rand.Text()
	}
	return context.WithValue(ctx, requestTraceKey{}, id)
}

func RequestTrace(ctx context.Context) string {
	id, _ := ctx.Value(requestTraceKey{}).(string)
	return id
}

// PublicError never serializes unclassified system/provider failures. The
// caller may correlate server diagnostics using trace_id instead.
func PublicError(ctx context.Context, err error) (int, domain.ErrorResponse) {
	id := RequestTrace(ctx)
	if id == "" {
		id = rand.Text()
	}
	response := domain.ErrorResponse{Code: "internal_error", Message: "요청을 처리하지 못했습니다. 관리자에게 추적 ID를 전달하세요.", TraceID: id}
	status := http.StatusInternalServerError
	var public *domain.PublicError
	var user *UserError
	switch {
	case errors.As(err, &public):
		status = public.Status
		if status < 400 || status > 599 {
			status = http.StatusBadRequest
		}
		response.Code, response.Message, response.Details = public.Code, public.Message, public.Details
	case errors.Is(err, context.DeadlineExceeded):
		status, response.Code, response.Message = http.StatusGatewayTimeout, "timeout", "요청 시간이 초과되었습니다. 작업 상태를 확인한 뒤 다시 시도하세요."
	case errors.Is(err, context.Canceled):
		status, response.Code, response.Message = http.StatusRequestTimeout, "canceled", "요청이 취소되었습니다."
	case errors.Is(err, domain.ErrNotFound):
		status, response.Code, response.Message = http.StatusNotFound, "not_found", "요청한 항목을 찾을 수 없습니다."
	case errors.As(err, &user):
		status, response.Code = http.StatusBadRequest, "invalid_request"
		response.Message, _ = mask.Mask(user.Msg)
		lower := strings.ToLower(user.Msg)
		switch {
		case strings.Contains(lower, "authentication required"):
			status, response.Code = http.StatusUnauthorized, "unauthorized"
		case strings.Contains(lower, "access denied"), strings.Contains(lower, "admin role required"), strings.Contains(lower, "administrator required"), strings.Contains(lower, "administrator permission required"):
			status, response.Code = http.StatusForbidden, "forbidden"
		case strings.Contains(lower, "approval"):
			response.Code = "approval_required"
		case strings.Contains(lower, "already"), strings.Contains(lower, "version changed"), strings.Contains(lower, "idempotency"):
			status, response.Code = http.StatusConflict, "conflict"
		}
	}
	return status, response
}

package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"postra/internal/application"
	"postra/internal/domain"
	"postra/internal/platform/metrics"
	"postra/internal/platform/telemetry"
)

func toolError(ctx context.Context, err error) *mcp.CallToolResult {
	_, response := application.PublicError(ctx, err)
	data, _ := json.Marshal(response)
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}, StructuredContent: response, Meta: mcp.Meta{"trace_id": response.TraceID}}
}

func resourceTool(uri string) string {
	switch {
	case uri == "schema://mail/tools":
		return "mail_capabilities"
	case uri == "policy://mail/current":
		return "mail_system_info"
	case strings.Contains(uri, "/drafts/"), strings.HasPrefix(uri, "postra://draft/"):
		return "mail_draft_get"
	case strings.Contains(uri, "/accounts/"), strings.HasPrefix(uri, "postra://account/"):
		return "mail_account_get"
	case strings.Contains(uri, "/threads/"), strings.HasPrefix(uri, "postra://thread/"):
		return "mail_thread_get"
	case strings.HasPrefix(uri, "postra://action/"):
		return "mail_action_cards_list"
	case strings.Contains(uri, "/sync-jobs/"), strings.HasPrefix(uri, "postra://job/"):
		return "job_status"
	default:
		return "mail_message_get"
	}
}

// Both the HTTP OAuth step-up challenge and the SDK's authoritative gateway
// use this policy. Transport preflight never performs application operations.
func checkMCPRequestPolicy(ctx context.Context, app *application.App, tool string, arguments json.RawMessage) error {
	tool = application.CanonicalMCPTool(tool)
	var in struct {
		Instructions string `json:"instructions"`
		SmartFormat  bool   `json:"smart_format"`
		Action       string `json:"action"`
	}
	validArguments := len(arguments) > 0 && json.Unmarshal(arguments, &in) == nil
	needsAI := validArguments && (in.SmartFormat || (tool == "mail_draft_create" && strings.TrimSpace(in.Instructions) != ""))
	needsDelete := validArguments && tool == "mail_batch_update" && in.Action == "delete"
	required := append([]string{}, application.MCPToolScopes(tool)...)
	if needsAI && !slices.Contains(required, "mail.ai") {
		required = append(required, "mail.ai")
	}
	if needsDelete {
		for _, scope := range application.MCPToolScopes("mail_local_delete") {
			if !slices.Contains(required, scope) {
				required = append(required, scope)
			}
		}
	}
	err := app.CheckMCPToolPolicy(ctx, tool)
	if err == nil && needsAI {
		err = app.CheckMCPPermissionScopes(ctx, "mail.ai")
	}
	if err == nil && needsDelete {
		err = app.CheckMCPToolPolicy(ctx, "mail_local_delete")
	}
	var public *domain.PublicError
	if errors.As(err, &public) && public.Code == "insufficient_scope" {
		copy := *public
		copy.Details = map[string]any{"required_scopes": required}
		return &copy
	}
	return err
}

func gatewayMiddleware(app *application.App) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			// SDK session contexts may retain initialization-time values. The
			// authenticated RequestExtra is refreshed on each HTTP call, so a
			// scope reduction or role change takes effect within an open session.
			if extra := req.GetExtra(); extra != nil && extra.TokenInfo != nil {
				if principal, ok := extra.TokenInfo.Extra["postra_principal"].(domain.Principal); ok {
					ctx = application.WithPrincipal(ctx, principal)
				}
				if trace, ok := extra.TokenInfo.Extra["postra_trace_id"].(string); ok {
					ctx = application.WithRequestTrace(ctx, trace)
				}
			}
			if method != "tools/call" && method != "resources/read" {
				return next(ctx, method, req)
			}
			ctx = application.WithRequestTrace(ctx, application.RequestTrace(ctx))
			started := time.Now()
			tool, inputBytes := "unknown", 0
			var arguments json.RawMessage
			switch params := req.GetParams().(type) {
			case *mcp.CallToolParamsRaw:
				tool, inputBytes, arguments = application.CanonicalMCPTool(params.Name), len(params.Arguments), params.Arguments
			case *mcp.ReadResourceParams:
				tool, inputBytes = resourceTool(params.URI), len(params.URI)
			}
			label := tool
			if _, known := application.ClassifyMCPTool(tool); !known {
				label = "unknown"
			}
			ctx, span := telemetry.Start(ctx, "mcp.call", telemetry.Attr("mcp.tool", label), telemetry.Attr("request.trace_id", application.RequestTrace(ctx)))
			defer span.End()
			// Long-running operations create a job using the existing application
			// service. This deadline only bounds the request, never invents a retry.
			ctx, cancel := context.WithTimeout(ctx, time.Duration(app.SettingInt("mcp.request_timeout_sec"))*time.Second)
			defer cancel()
			var result mcp.Result
			err := checkMCPRequestPolicy(ctx, app, tool, arguments)
			if err == nil {
				notifyProgress(ctx, req, 0, "처리 중")
				result, err = next(ctx, method, req)
				if ctr, ok := result.(*mcp.CallToolResult); ok && ctr != nil && ctr.IsError {
					err = ctr.GetError()
					if err == nil {
						err = errors.New("tool failed")
					}
					if strings.HasPrefix(err.Error(), "validating \"arguments\"") {
						err = &domain.PublicError{Code: "invalid_request", Message: "도구 입력이 JSON 스키마와 일치하지 않습니다.", Status: 400}
					}
				}
			}
			outcome, errorCode := "ok", ""
			if err != nil {
				outcome = "error"
				_, public := application.PublicError(ctx, err)
				errorCode = public.Code
				if method == "tools/call" {
					result = toolError(ctx, err)
					err = nil
				} else {
					// Resource failures are protocol errors; only the shared public
					// contract is exposed, never driver/provider error text.
					encoded, _ := json.Marshal(public)
					err = errors.New(string(encoded))
					result = nil
				}
			}
			if result != nil {
				metadata := result.GetMeta()
				if metadata == nil {
					metadata = map[string]any{}
				}
				metadata["trace_id"] = application.RequestTrace(ctx)
				result.SetMeta(metadata)
			}
			encoded, _ := json.Marshal(result)
			recordMCPCall(ctx, app, label, outcome, errorCode, inputBytes, len(encoded), time.Since(started))
			notifyProgress(ctx, req, 1, "처리 완료")
			return result, err
		}
	}
}

// Audit transport denials and SDK calls identically, recording only bounded
// identifiers and byte counts, never bearer tokens, arguments or mail content.
func recordMCPCall(ctx context.Context, app *application.App, label, outcome, errorCode string, inputBytes, outputBytes int, elapsed time.Duration) {
	metrics.MCPRequests.WithLabelValues(label, outcome).Inc()
	metrics.MCPLatency.WithLabelValues(label).Observe(elapsed.Seconds())
	metrics.MCPBytes.WithLabelValues(label, "input").Add(float64(inputBytes))
	metrics.MCPBytes.WithLabelValues(label, "output").Add(float64(outputBytes))
	p, ok := application.PrincipalFrom(ctx)
	if !ok {
		p.UserID = application.DefaultUserID
	}
	detail, _ := json.Marshal(map[string]any{"trace_id": application.RequestTrace(ctx), "key_id": p.MCPKeyID, "auth_method": p.AuthMethod, "oauth_client_id": p.OAuthClientID, "tool": label, "latency_ms": elapsed.Milliseconds(), "input_bytes": inputBytes, "output_bytes": outputBytes, "error_code": errorCode})
	auditCtx, auditCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer auditCancel()
	_ = app.Store.AppendAudit(auditCtx, domain.AuditEvent{UserID: p.UserID, Actor: "mcp", Action: "mcp_call", Resource: "tool:" + label, Result: outcome, Detail: string(detail)})
}

func notifyProgress(ctx context.Context, req mcp.Request, progress float64, message string) {
	params, ok := req.GetParams().(interface{ GetProgressToken() any })
	if !ok {
		return
	}
	if token := params.GetProgressToken(); token != nil {
		if session, ok := req.GetSession().(*mcp.ServerSession); ok && session != nil {
			_ = session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: token, Progress: progress, Total: 1, Message: message})
		}
	}
}

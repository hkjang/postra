// Package mcpserver exposes the application use cases as MCP tools over
// stdio (local) and Streamable HTTP (remote), using the official Go SDK.
// Every tool handler delegates to the same application.App the REST API
// uses (§16); secret values never appear in any tool argument (SEC-KEY-001).
package mcpserver

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"postra/internal/application"
	"postra/internal/domain"
	"postra/internal/platform/build"
	"postra/internal/platform/metrics"
	"postra/internal/platform/telemetry"
)

func boolPtr(b bool) *bool { return &b }

var (
	readOnly    = &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: boolPtr(false), OpenWorldHint: boolPtr(false)}
	readExtern  = &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: boolPtr(false), OpenWorldHint: boolPtr(true)}
	writeLocal  = &mcp.ToolAnnotations{DestructiveHint: boolPtr(false), OpenWorldHint: boolPtr(false)}
	writeExtern = &mcp.ToolAnnotations{DestructiveHint: boolPtr(false), OpenWorldHint: boolPtr(true)}
	destructive = &mcp.ToolAnnotations{DestructiveHint: boolPtr(true), OpenWorldHint: boolPtr(true)}
)

// metricsMiddleware records every MCP tool invocation (§18.1). It labels by
// tool name and result (ok/error), covering both handler errors and tool
// results flagged IsError. Non-tool methods (initialize, list, …) pass through
// unmeasured to keep the series set focused on tool usage.
func metricsMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method != "tools/call" {
			return next(ctx, method, req)
		}
		tool := "unknown"
		if p, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok && p.Name != "" {
			tool = p.Name
		}
		ctx, span := telemetry.Start(ctx, "mcp.tool", telemetry.Attr("mcp.tool", tool))
		defer span.End()
		res, err := next(ctx, method, req)
		result := "ok"
		if err != nil {
			result = "error"
		} else if ctr, ok := res.(*mcp.CallToolResult); ok && ctr.IsError {
			result = "error"
		}
		metrics.MCPRequests.WithLabelValues(tool, result).Inc()
		return res, err
	}
}

// policyMiddleware enforces the central MCP gateway policy before a tool runs
// (§MCP 정책 게이트웨이). Local stdio callers have no principal and are allowed;
// remote callers are checked against the configured policy for their role.
func policyMiddleware(app *application.App) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			if p, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok && p.Name != "" {
				if err := app.CheckMCPToolPolicy(ctx, p.Name); err != nil {
					return &mcp.CallToolResult{
						IsError: true,
						Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
					}, nil
				}
			}
			return next(ctx, method, req)
		}
	}
}

// NewServer builds the MCP server with the full tool catalog.
func NewServer(app *application.App) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "postra-mail", Version: build.Version}, nil)
	s.AddReceivingMiddleware(metricsMiddleware)
	s.AddReceivingMiddleware(policyMiddleware(app))
	registerAccountTools(s, app)
	registerSyncTools(s, app)
	registerQueryTools(s, app)
	registerAITools(s, app)
	registerComposeTools(s, app)
	registerDeleteTools(s, app)
	registerRuleTools(s, app)
	registerActionCardTools(s, app)
	registerCollabTools(s, app)
	registerAuditTools(s, app)
	registerResources(s, app)
	registerPrompts(s, app)
	return s
}

// registerDeleteTools exposes the local and 2-stage server-delete flows
// (§5.2). Server deletion requires a fresh approval token bound to the exact
// UIDL set; local deletion is destructive but does not touch the server.
func registerDeleteTools(s *mcp.Server, app *application.App) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_local_delete",
		Description: "Delete a message from LOCAL storage only (DB rows + stored blobs). The mail server copy is untouched. Destructive.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPtr(true), OpenWorldHint: boolPtr(false)},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		MessageID string `json:"message_id" jsonschema:"the internal message ID to delete locally"`
		Confirm   bool   `json:"confirm" jsonschema:"must be true after the user approved the local deletion"`
	}) (*mcp.CallToolResult, any, error) {
		if !in.Confirm {
			return nil, nil, &application.UserError{Msg: "set confirm=true after the user explicitly approved deleting this local message"}
		}
		if err := app.LocalDelete(ctx, in.MessageID); err != nil {
			return nil, nil, err
		}
		return nil, map[string]string{"status": "deleted", "message_id": in.MessageID}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_server_delete_preview",
		Description: "Preview which maildrop messages could be deleted on the POP3 server. Only messages already stored locally are eligible. Deletes nothing; returns a payload hash for approval.",
		Annotations: readExtern,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in accountIDInput) (*mcp.CallToolResult, any, error) {
		pv, err := app.ServerDeletePreview(ctx, in.AccountID)
		if err != nil {
			return nil, nil, err
		}
		return nil, pv, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_server_delete_request_approval",
		Description: "Request a one-time approval token to delete the currently-eligible messages from the POP3 server. Show the preview to the user and obtain explicit confirmation before mail_server_delete.",
		Annotations: writeExtern,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		AccountID  string `json:"account_id" jsonschema:"the mail account ID"`
		Approver   string `json:"approver,omitempty"`
		TTLSeconds int    `json:"ttl_seconds,omitempty" jsonschema:"token lifetime, default 600"`
	}) (*mcp.CallToolResult, any, error) {
		pv, tok, err := app.RequestServerDeleteApproval(ctx, in.AccountID, in.Approver, in.TTLSeconds)
		if err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"preview": pv, "approval": tok}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_server_delete",
		Description: "Delete the approved UIDLs from the POP3 server. Requires an approval token bound to the exact account + UIDL set. Very destructive and externally visible — only call after explicit user confirmation.",
		Annotations: destructive,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		AccountID     string   `json:"account_id" jsonschema:"the mail account ID"`
		UIDLs         []string `json:"uidls" jsonschema:"the UIDLs to delete (from the approved preview)"`
		ApprovalToken string   `json:"approval_token" jsonschema:"token from mail_server_delete_request_approval"`
	}) (*mcp.CallToolResult, any, error) {
		res, err := app.ServerDelete(ctx, in.AccountID, in.UIDLs, in.ApprovalToken)
		if err != nil {
			return nil, nil, err
		}
		return nil, res, nil
	})
}

// toolCatalogSummary backs the schema://mail/tools resource: a stable,
// non-sensitive listing of tool names grouped by capability.
func toolCatalogSummary() map[string]any {
	return map[string]any{
		"protocol_version": "2025-11-25 (baseline)",
		"groups": map[string][]string{
			"account_secret": {"mail_account_list", "mail_account_get", "mail_account_create",
				"mail_account_update", "mail_account_test", "mail_account_disable",
				"secret_registration_begin", "secret_rotation_begin", "secret_revoke"},
			"sync": {"mail_sync_start", "job_status", "job_cancel"},
			"query": {"mail_search", "mail_message_get", "mail_thread_get", "mail_thread_timeline",
				"mail_attachment_list", "mail_hybrid_search", "mail_batch_update", "mail_work_inbox"},
			"ai": {"mail_summarize", "mail_classify", "mail_action_items_extract",
				"mail_entities_extract", "mail_phishing_inspect", "mail_auth_inspect", "mail_thread_summarize",
				"mail_question_answer", "mail_embeddings_build", "mail_semantic_search",
				"mail_attachment_summarize", "mail_eval_prompt"},
			"compose_send": {"mail_draft_create", "mail_draft_update", "mail_draft_rewrite",
				"mail_send_preview", "mail_send_request_approval", "mail_send", "mail_outbound_status"},
			"automation":    {"mail_rules_list", "mail_rule_create", "mail_rule_update", "mail_rule_delete", "mail_apply_rules"},
			"action_cards":  {"mail_action_cards_extract", "mail_action_cards_list", "mail_action_card_set_status", "mail_action_card_export"},
			"collaboration": {"mail_team_inbox", "mail_collab_get", "mail_assign", "mail_set_work_status", "mail_add_note"},
			"attachments":   {"mail_attachment_list", "mail_attachment_extract_text", "mail_attachment_summarize"},
			"audit":         {"mail_audit_search"},
		},
		"note": "Secret values are never accepted as tool arguments; use secret_registration_begin.",
	}
}

// RunStdio serves MCP over stdin/stdout (local transport, §10.1).
func RunStdio(ctx context.Context, app *application.App) error {
	return NewServer(app).Run(application.WithActor(ctx, "mcp"), &mcp.StdioTransport{})
}

// HTTPHandler serves MCP over Streamable HTTP (remote transport).
// When apiToken is set, requests must carry "Authorization: Bearer <token>";
// on offline networks the operator may run tokenless on a trusted interface.
// Origin checking for browser-based clients is enforced by the SDK handler;
// we additionally reject cross-origin browser requests explicitly.
func HTTPHandler(app *application.App, apiToken string) http.Handler {
	inner := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return NewServer(app)
	}, nil)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := application.WithActor(r.Context(), "mcp")
		if app.Cfg.Auth.Enabled || apiToken != "" {
			raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			var principal domain.Principal
			ok := false
			if apiToken != "" && subtle.ConstantTimeCompare([]byte(raw), []byte(apiToken)) == 1 {
				if u, err := app.Store.GetUser(r.Context(), application.DefaultUserID); err == nil {
					principal = domain.Principal{UserID: u.ID, LoginID: u.LoginID, DisplayName: u.DisplayName,
						Role: domain.RoleAdmin, AuthMethod: "api_token"}
					ok = true
				}
			}
			if !ok && raw != "" {
				if _, p, err := app.AuthenticateMCPKey(r.Context(), raw); err == nil {
					principal, ok = p, true
				}
			}
			if !ok && raw != "" {
				if p, err := app.AuthenticateOIDCAccessToken(r.Context(), raw); err == nil {
					principal, ok = p, true
				}
			}
			if !ok {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			ctx = application.WithPrincipal(ctx, principal)
		}
		inner.ServeHTTP(w, r.WithContext(ctx))

	})
}

// ---------- account & secret tools ----------

type emptyInput struct{}

type accountIDInput struct {
	AccountID string `json:"account_id" jsonschema:"the mail account ID"`
}

func registerAccountTools(s *mcp.Server, app *application.App) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_account_list",
		Description: "List registered mail accounts (never includes secret values).",
		Annotations: readOnly,
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, any, error) {
		accs, err := app.ListAccounts(ctx)
		if err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"accounts": accs}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_account_get",
		Description: "Get one mail account's non-secret settings.",
		Annotations: readOnly,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in accountIDInput) (*mcp.CallToolResult, any, error) {
		acc, err := app.GetAccount(ctx, in.AccountID)
		if err != nil {
			return nil, nil, err
		}
		return nil, acc, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "mail_account_create",
		Description: "Register a mail account. POP3/SMTP credentials must be referenced by secret_ref values " +
			"obtained via the secure registration flow (see secret_registration_begin) — never raw passwords.",
		Annotations: writeExtern,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in application.CreateAccountInput) (*mcp.CallToolResult, any, error) {
		acc, err := app.CreateAccount(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return nil, acc, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_account_update",
		Description: "Update non-secret account settings (hosts, ports, security mode, usernames).",
		Annotations: writeExtern,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in application.UpdateAccountInput) (*mcp.CallToolResult, any, error) {
		acc, err := app.UpdateAccount(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return nil, acc, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_account_test",
		Description: "Run staged connection diagnostics (DNS, TLS, AUTH, UIDL / SMTP EHLO) for an account.",
		Annotations: readExtern,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in accountIDInput) (*mcp.CallToolResult, any, error) {
		diags, err := app.TestAccount(ctx, in.AccountID)
		if err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"diagnostics": diags}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_account_disable",
		Description: "Disable an account: stops sync and send immediately; stored data is preserved.",
		Annotations: writeLocal,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in accountIDInput) (*mcp.CallToolResult, any, error) {
		if err := app.DisableAccount(ctx, in.AccountID); err != nil {
			return nil, nil, err
		}
		return nil, map[string]string{"status": "disabled"}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "secret_registration_begin",
		Description: "Begin secure credential registration. Returns instructions for the out-of-band input path " +
			"(CLI TTY or authenticated REST). Secret values are NEVER accepted as MCP tool arguments.",
		Annotations: readOnly,
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, any, error) {
		return nil, map[string]string{"instructions": app.SecretRegistrationInstructions()}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "secret_rotation_begin",
		Description: "Begin credential rotation for an existing secret reference. Returns the out-of-band " +
			"instructions; the new value is never passed through MCP.",
		Annotations: readOnly,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		SecretRef string `json:"secret_ref" jsonschema:"the secret reference to rotate"`
	}) (*mcp.CallToolResult, any, error) {
		return nil, map[string]string{
			"instructions": "Rotate via CLI: postra secret rotate --ref " + in.SecretRef +
				" (TTY prompt), or REST: POST /api/secrets/" + in.SecretRef + "/rotate. " +
				"Accounts pick up the new value on next use without restart.",
		}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "secret_revoke",
		Description: "Revoke a secret reference permanently. Destructive: accounts using it will fail until rotated.",
		Annotations: destructive,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		SecretRef string `json:"secret_ref" jsonschema:"the secret reference to revoke"`
		Confirm   bool   `json:"confirm" jsonschema:"must be true to actually revoke"`
	}) (*mcp.CallToolResult, any, error) {
		if !in.Confirm {
			return nil, nil, &application.UserError{Msg: "set confirm=true after the user has explicitly approved revoking this secret"}
		}
		if err := app.RevokeSecret(ctx, domain.SecretRef(in.SecretRef)); err != nil {
			return nil, nil, err
		}
		return nil, map[string]string{"status": "revoked"}, nil
	})
}

// ---------- sync & job tools ----------

func registerSyncTools(s *mcp.Server, app *application.App) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_sync_start",
		Description: "Start an asynchronous POP3 sync for an account. Returns a job_id to poll with job_status.",
		Annotations: writeExtern,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		AccountID   string `json:"account_id" jsonschema:"the mail account ID"`
		MaxMessages int    `json:"max_messages,omitempty" jsonschema:"optional cap on messages fetched this run"`
	}) (*mcp.CallToolResult, any, error) {
		fullSync := in.MaxMessages <= 0
		job, err := app.StartSync(ctx, in.AccountID, application.SyncOptions{MaxMessages: in.MaxMessages, FullSync: fullSync})
		if err != nil {
			return nil, nil, err
		}
		return nil, job, nil
	})

	type jobIDInput struct {
		JobID string `json:"job_id" jsonschema:"the job ID"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "job_status",
		Description: "Get status, progress, and statistics of an asynchronous job (sync, analysis, ...).",
		Annotations: readOnly,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in jobIDInput) (*mcp.CallToolResult, any, error) {
		job, err := app.GetJob(ctx, in.JobID)
		if err != nil {
			return nil, nil, err
		}
		return nil, job, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "job_cancel",
		Description: "Cancel a running asynchronous job.",
		Annotations: writeLocal,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in jobIDInput) (*mcp.CallToolResult, any, error) {
		if err := app.CancelJob(ctx, in.JobID); err != nil {
			return nil, nil, err
		}
		return nil, map[string]string{"status": "cancelling"}, nil
	})
}

// ---------- search & read tools ----------

func registerQueryTools(s *mcp.Server, app *application.App) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_search",
		Description: "Search mail by keyword and filters (from, to, subject, date range, attachments). Cursor-paginated.",
		Annotations: readOnly,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in domain.SearchQuery) (*mcp.CallToolResult, any, error) {
		res, err := app.Search(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return nil, res, nil
	})

	type messageIDInput struct {
		MessageID string `json:"message_id" jsonschema:"the internal message ID (msg_...)"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_message_get",
		Description: "Get a parsed message: headers, text body, sanitized HTML, attachment list. Set mask=true to redact PII/secrets.",
		Annotations: readOnly,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		MessageID string `json:"message_id" jsonschema:"the internal message ID (msg_...)"`
		Mask      bool   `json:"mask,omitempty" jsonschema:"redact PII/secrets in subject and body"`
	}) (*mcp.CallToolResult, any, error) {
		get := app.GetMessage
		if in.Mask {
			get = app.GetMessageMasked
		}
		mv, err := get(ctx, in.MessageID, true)
		if err != nil {
			return nil, nil, err
		}
		return nil, mv, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_thread_get",
		Description: "Get all messages of a conversation thread in chronological order.",
		Annotations: readOnly,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		ThreadID      string `json:"thread_id" jsonschema:"the thread ID (thr_...)"`
		IncludeBodies bool   `json:"include_bodies,omitempty"`
	}) (*mcp.CallToolResult, any, error) {
		tv, err := app.GetThread(ctx, in.ThreadID, in.IncludeBodies)
		if err != nil {
			return nil, nil, err
		}
		return nil, tv, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_attachment_list",
		Description: "List attachments of a message (name, type, size, hash) without downloading content.",
		Annotations: readOnly,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in messageIDInput) (*mcp.CallToolResult, any, error) {
		atts, err := app.ListAttachments(ctx, in.MessageID)
		if err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"attachments": atts}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_hybrid_search",
		Description: "Hybrid search combining full-text keyword search and semantic vector search via Reciprocal Rank Fusion (RRF). Returns messages ranked by fused score, optionally collapsed to one hit per thread.",
		Annotations: readExtern,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		Query         string  `json:"query" jsonschema:"natural-language or keyword query"`
		AccountID     string  `json:"account_id,omitempty" jsonschema:"optional account scope"`
		Limit         int     `json:"limit,omitempty" jsonschema:"max results, default 20"`
		RRFKConstant  float64 `json:"rrf_k,omitempty" jsonschema:"RRF k constant, default 60"`
		FTSWeight     float64 `json:"fts_weight,omitempty" jsonschema:"weight for keyword ranking, default 1"`
		VectorWeight  float64 `json:"vector_weight,omitempty" jsonschema:"weight for semantic ranking, default 1"`
		GroupByThread bool    `json:"group_by_thread,omitempty" jsonschema:"aggregate to one hit per thread"`
		Rerank        bool    `json:"rerank,omitempty" jsonschema:"apply an LLM cross-encoder reranking pass"`
	}) (*mcp.CallToolResult, any, error) {
		views, err := app.HybridSearch(ctx, application.HybridSearchOptions{
			Query: in.Query, AccountID: in.AccountID, Limit: in.Limit,
			RRFKConstant: in.RRFKConstant, FTSWeight: in.FTSWeight,
			VectorWeight: in.VectorWeight, GroupByThread: in.GroupByThread, Rerank: in.Rerank,
		})
		if err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"results": views, "count": len(views)}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_thread_timeline",
		Description: "Get a conversation thread as an ordered timeline (oldest first) with bodies and attachments for an interactive view.",
		Annotations: readOnly,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		ThreadID string `json:"thread_id" jsonschema:"the thread ID (thr_...)"`
	}) (*mcp.CallToolResult, any, error) {
		tl, err := app.GetThreadTimeline(ctx, in.ThreadID)
		if err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"timeline": tl, "count": len(tl)}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_work_inbox",
		Description: "Task-oriented triage of the active inbox, grouped into important / snoozed_due / attention / reference buckets with counts.",
		Annotations: readOnly,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		AccountID string `json:"account_id,omitempty" jsonschema:"optional account scope"`
		Limit     int    `json:"limit,omitempty" jsonschema:"inbox window size, default 100"`
	}) (*mcp.CallToolResult, any, error) {
		inbox, err := app.WorkInbox(ctx, in.AccountID, in.Limit)
		if err != nil {
			return nil, nil, err
		}
		return nil, inbox, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_batch_update",
		Description: "Bulk-update messages. action ∈ {archive, unarchive, mark_important, unmark_important, snooze, unsnooze, add_label, remove_label, delete}. Returns per-message success/failure. Deleting in bulk requires confirm=true.",
		Annotations: writeLocal,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		MessageIDs   []string `json:"message_ids" jsonschema:"internal message IDs to update"`
		Action       string   `json:"action" jsonschema:"batch action to apply"`
		SnoozedUntil int64    `json:"snoozed_until,omitempty" jsonschema:"unix seconds; required for snooze"`
		Label        string   `json:"label,omitempty" jsonschema:"label; required for add_label/remove_label"`
		Confirm      bool     `json:"confirm,omitempty" jsonschema:"must be true to delete in bulk"`
	}) (*mcp.CallToolResult, any, error) {
		if application.BatchAction(in.Action) == application.BatchActionDelete && !in.Confirm {
			return nil, nil, &application.UserError{Msg: "set confirm=true after the user approved bulk-deleting these messages"}
		}
		res, err := app.BatchUpdateMessages(ctx, application.BatchUpdateOptions{
			MessageIDs: in.MessageIDs, Action: application.BatchAction(in.Action),
			SnoozedUntil: in.SnoozedUntil, Label: in.Label,
		})
		if err != nil {
			return nil, nil, err
		}
		return nil, res, nil
	})
}

// ---------- AI tools ----------

func registerAITools(s *mcp.Server, app *application.App) {
	type messageIDInput struct {
		MessageID string `json:"message_id" jsonschema:"the internal message ID"`
	}
	analyze := func(analysisType string) func(context.Context, *mcp.CallToolRequest, messageIDInput) (*mcp.CallToolResult, any, error) {
		return func(ctx context.Context, req *mcp.CallToolRequest, in messageIDInput) (*mcp.CallToolResult, any, error) {
			an, err := app.AnalyzeMessage(ctx, in.MessageID, analysisType)
			if err != nil {
				return nil, nil, err
			}
			return nil, an, nil
		}
	}
	aiAnn := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: boolPtr(true)}

	mcp.AddTool(s, &mcp.Tool{Name: "mail_summarize",
		Description: "AI-summarize one message: key points, requests, dates.", Annotations: aiAnn}, analyze("summarize"))
	mcp.AddTool(s, &mcp.Tool{Name: "mail_classify",
		Description: "AI-classify a message (work/ad/notification/personal/security) with importance.", Annotations: aiAnn}, analyze("classify"))
	mcp.AddTool(s, &mcp.Tool{Name: "mail_action_items_extract",
		Description: "Extract action items (task, assignee, due date, evidence) from a message.", Annotations: aiAnn}, analyze("action_items"))
	mcp.AddTool(s, &mcp.Tool{Name: "mail_entities_extract",
		Description: "Extract entities (people, companies, projects, amounts, contacts) from a message.", Annotations: aiAnn}, analyze("entities"))
	mcp.AddTool(s, &mcp.Tool{Name: "mail_phishing_inspect",
		Description: "AI phishing-risk assessment of a message including authentication headers.", Annotations: aiAnn}, analyze("phishing"))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_thread_summarize",
		Description: "AI-summarize a whole thread: progress, decisions, open items.",
		Annotations: aiAnn,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		ThreadID string `json:"thread_id" jsonschema:"the thread ID"`
	}) (*mcp.CallToolResult, any, error) {
		an, err := app.SummarizeThread(ctx, in.ThreadID)
		if err != nil {
			return nil, nil, err
		}
		return nil, an, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_attachment_extract_text",
		Description: "Extract plain text from a text-based attachment (text/*, JSON, CSV, HTML). Binary/OCR formats return unsupported.",
		Annotations: readOnly,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		MessageID    string `json:"message_id" jsonschema:"the internal message ID"`
		AttachmentID string `json:"attachment_id" jsonschema:"the attachment ID (att_...)"`
	}) (*mcp.CallToolResult, any, error) {
		res, err := app.ExtractAttachmentText(ctx, in.MessageID, in.AttachmentID)
		if err != nil {
			return nil, nil, err
		}
		return nil, res, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_attachment_summarize",
		Description: "Extract a text-based attachment and AI-summarize it (key points, tables/figures, risks).",
		Annotations: aiAnn,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		MessageID    string `json:"message_id" jsonschema:"the internal message ID"`
		AttachmentID string `json:"attachment_id" jsonschema:"the attachment ID (att_...)"`
	}) (*mcp.CallToolResult, any, error) {
		an, err := app.SummarizeAttachment(ctx, in.MessageID, in.AttachmentID)
		if err != nil {
			return nil, nil, err
		}
		return nil, an, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_auth_inspect",
		Description: "Structured SPF/DKIM/DMARC/ARC verdicts, From-domain alignment, and a 0-100 sender-domain risk score for a message. Deterministic and offline (reads the receiving MTA's recorded Authentication-Results).",
		Annotations: readOnly,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		MessageID string `json:"message_id" jsonschema:"the internal message ID"`
	}) (*mcp.CallToolResult, any, error) {
		res, err := app.InspectAuthentication(ctx, in.MessageID)
		if err != nil {
			return nil, nil, err
		}
		return nil, res, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_embeddings_build",
		Description: "Build embeddings for stored messages lacking them, enabling semantic search. Returns a job_id.",
		Annotations: aiAnn,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		AccountID string `json:"account_id,omitempty" jsonschema:"optional account scope"`
		Max       int    `json:"max,omitempty" jsonschema:"max messages to embed this run"`
	}) (*mcp.CallToolResult, any, error) {
		job, err := app.BuildEmbeddings(ctx, in.AccountID, in.Max)
		if err != nil {
			return nil, nil, err
		}
		return nil, job, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_semantic_search",
		Description: "Meaning-based search over embedded messages. Returns messages ranked by similarity with scores and a short reason.",
		Annotations: aiAnn,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		Query     string `json:"query" jsonschema:"natural-language query"`
		AccountID string `json:"account_id,omitempty" jsonschema:"optional account scope"`
		Limit     int    `json:"limit,omitempty" jsonschema:"max results, default 10"`
	}) (*mcp.CallToolResult, any, error) {
		hits, err := app.SemanticSearch(ctx, in.Query, in.AccountID, in.Limit)
		if err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"results": hits}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_eval_prompt",
		Description: "Evaluate an analysis type over labeled cases (message_id + expected [+ field]) and report accuracy and latency for the active prompt version.",
		Annotations: aiAnn,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		AnalysisType string                 `json:"analysis_type" jsonschema:"e.g. classify, summarize, phishing"`
		Cases        []application.EvalCase `json:"cases" jsonschema:"labeled cases"`
	}) (*mcp.CallToolResult, any, error) {
		res, err := app.EvaluatePrompt(ctx, in.AnalysisType, in.Cases)
		if err != nil {
			return nil, nil, err
		}
		return nil, res, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_question_answer",
		Description: "Answer a question from the user's own mailbox, with evidence message IDs.",
		Annotations: aiAnn,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		Question  string `json:"question" jsonschema:"the question to answer from the mailbox"`
		AccountID string `json:"account_id,omitempty" jsonschema:"optional account scope"`
	}) (*mcp.CallToolResult, any, error) {
		an, err := app.AnswerQuestion(ctx, in.Question, in.AccountID)
		if err != nil {
			return nil, nil, err
		}
		return nil, an, nil
	})
}

// ---------- compose & send tools ----------

func registerComposeTools(s *mcp.Server, app *application.App) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "mail_draft_create",
		Description: "Create a mail draft (new/reply/reply_all/forward). With 'instructions', the AI writes the body. " +
			"Drafts are never sent automatically — sending requires mail_send_request_approval + mail_send.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: false, DestructiveHint: boolPtr(false), OpenWorldHint: boolPtr(false)},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in application.CreateDraftInput) (*mcp.CallToolResult, any, error) {
		dv, err := app.CreateDraft(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return nil, dv, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_draft_update",
		Description: "Update a draft's subject, body, or recipients. Creates a new user-authored version and invalidates prior approvals.",
		Annotations: writeLocal,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in application.UpdateDraftInput) (*mcp.CallToolResult, any, error) {
		dv, err := app.UpdateDraft(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return nil, dv, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_draft_rewrite",
		Description: "AI-rewrite a draft in a given style (formal, concise, friendly, translate to <lang>, ...).",
		Annotations: writeLocal,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		DraftID string `json:"draft_id" jsonschema:"the draft ID"`
		Style   string `json:"style" jsonschema:"target style, e.g. formal, concise, friendly"`
	}) (*mcp.CallToolResult, any, error) {
		dv, err := app.RewriteDraft(ctx, in.DraftID, in.Style)
		if err != nil {
			return nil, nil, err
		}
		return nil, dv, nil
	})

	type draftIDInput struct {
		DraftID string `json:"draft_id" jsonschema:"the draft ID"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_send_preview",
		Description: "Preview exactly what would be sent: from, recipients, subject, body, external-domain warnings, payload hash.",
		Annotations: readOnly,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in draftIDInput) (*mcp.CallToolResult, any, error) {
		p, err := app.PreviewSend(ctx, in.DraftID)
		if err != nil {
			return nil, nil, err
		}
		return nil, p, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "mail_send_request_approval",
		Description: "Request a one-time send-approval token for the draft's CURRENT version. " +
			"Show the returned preview to the user and obtain their explicit confirmation before calling mail_send. " +
			"Any later edit to the draft invalidates the token.",
		Annotations: writeLocal,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		DraftID    string `json:"draft_id" jsonschema:"the draft ID"`
		Approver   string `json:"approver,omitempty" jsonschema:"who approved (display name)"`
		TTLSeconds int    `json:"ttl_seconds,omitempty" jsonschema:"token lifetime, default 600"`
	}) (*mcp.CallToolResult, any, error) {
		preview, tok, err := app.RequestSendApproval(ctx, in.DraftID, in.Approver, in.TTLSeconds)
		if err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"preview": preview, "approval": tok}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "mail_send",
		Description: "Send an approved draft via SMTP. Requires a valid approval token bound to the draft's current content. " +
			"Destructive and externally visible — only call after the user explicitly confirmed the preview.",
		Annotations: destructive,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in application.SendInput) (*mcp.CallToolResult, any, error) {
		out, err := app.Send(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_outbound_status",
		Description: "Get the delivery status of an outbound message (sent / failed / send_uncertain).",
		Annotations: readOnly,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		OutboundID string `json:"outbound_id" jsonschema:"the outbound message ID (out_...)"`
	}) (*mcp.CallToolResult, any, error) {
		out, err := app.GetOutbound(ctx, in.OutboundID)
		if err != nil {
			return nil, nil, err
		}
		return nil, out, nil
	})
}

// ---------- rule (automation) tools ----------

func registerRuleTools(s *mcp.Server, app *application.App) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_rules_list",
		Description: "List the caller's mail automation rules (conditions → actions), ordered by priority.",
		Annotations: readOnly,
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, any, error) {
		rules, err := app.ListRules(ctx)
		if err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"rules": rules}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "mail_rule_create",
		Description: "Create a mail automation rule. Fields: name, match(all|any), priority, conditions[{field,operator,value}], " +
			"actions[{type,value}], stop_on_match. Condition fields: from,to,subject,body,account,has_attachment,is_important. " +
			"Operators: contains,equals,starts_with,ends_with,regex,is_true,is_false. Actions: add_label,remove_label,archive,mark_important,snooze,delete.",
		Annotations: writeLocal,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in domain.MailRule) (*mcp.CallToolResult, any, error) {
		rule, err := app.CreateRule(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return nil, rule, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_rule_update",
		Description: "Update an existing mail automation rule (must include id).",
		Annotations: writeLocal,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in domain.MailRule) (*mcp.CallToolResult, any, error) {
		rule, err := app.UpdateRule(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return nil, rule, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_rule_delete",
		Description: "Delete a mail automation rule.",
		Annotations: writeLocal,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		RuleID string `json:"rule_id" jsonschema:"the rule ID (rule_...)"`
	}) (*mcp.CallToolResult, any, error) {
		if err := app.DeleteRule(ctx, in.RuleID); err != nil {
			return nil, nil, err
		}
		return nil, map[string]string{"status": "deleted"}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_apply_rules",
		Description: "Evaluate the caller's rules against one stored message and apply matching actions. Returns which rules matched.",
		Annotations: writeLocal,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		MessageID string `json:"message_id" jsonschema:"the internal message ID"`
	}) (*mcp.CallToolResult, any, error) {
		res, err := app.ApplyRulesToMessage(ctx, in.MessageID)
		if err != nil {
			return nil, nil, err
		}
		return nil, res, nil
	})
}

// ---------- action card tools ----------

func registerActionCardTools(s *mcp.Server, app *application.App) {
	aiAnn := &mcp.ToolAnnotations{ReadOnlyHint: false, OpenWorldHint: boolPtr(true)}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_action_cards_extract",
		Description: "AI-extract actionable cards (meeting/todo/approval/inquiry) from a message and store them as pending for review.",
		Annotations: aiAnn,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		MessageID string `json:"message_id" jsonschema:"the internal message ID"`
	}) (*mcp.CallToolResult, any, error) {
		cards, err := app.ExtractActionCards(ctx, in.MessageID)
		if err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"cards": cards, "count": len(cards)}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_action_cards_list",
		Description: "List extracted action cards, optionally filtered by status (pending|approved|rejected|done|exported).",
		Annotations: readOnly,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		Status string `json:"status,omitempty"`
		Limit  int    `json:"limit,omitempty"`
	}) (*mcp.CallToolResult, any, error) {
		cards, err := app.ListActionCards(ctx, in.Status, in.Limit)
		if err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"cards": cards}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_action_card_set_status",
		Description: "Set an action card's status (approved|rejected|done|pending).",
		Annotations: writeLocal,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		CardID string `json:"card_id" jsonschema:"the action card ID (act_...)"`
		Status string `json:"status" jsonschema:"approved|rejected|done|pending"`
	}) (*mcp.CallToolResult, any, error) {
		card, err := app.SetActionCardStatus(ctx, in.CardID, in.Status)
		if err != nil {
			return nil, nil, err
		}
		return nil, card, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_action_card_export",
		Description: "Export an APPROVED action card to a target system (calendar/jira/itsm). Returns a structured payload for the integration to apply; Postra performs no external write itself.",
		Annotations: writeLocal,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		CardID      string `json:"card_id" jsonschema:"the action card ID"`
		Target      string `json:"target" jsonschema:"calendar|jira|itsm|..."`
		ExternalRef string `json:"external_ref,omitempty" jsonschema:"optional external record id"`
	}) (*mcp.CallToolResult, any, error) {
		exp, err := app.ExportActionCard(ctx, in.CardID, in.Target, in.ExternalRef)
		if err != nil {
			return nil, nil, err
		}
		return nil, exp, nil
	})
}

// ---------- collaboration (shared mailbox) tools ----------

func registerCollabTools(s *mcp.Server, app *application.App) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_team_inbox",
		Description: "List messages with shared-mailbox collaboration state, optionally filtered by status (open|pending|resolved) and assignee.",
		Annotations: readOnly,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		Status   string `json:"status,omitempty"`
		Assignee string `json:"assignee,omitempty"`
		Limit    int    `json:"limit,omitempty"`
	}) (*mcp.CallToolResult, any, error) {
		items, err := app.TeamInbox(ctx, in.Status, in.Assignee, in.Limit)
		if err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"items": items, "count": len(items)}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_collab_get",
		Description: "Get a message's collaboration state (assignee, status, SLA) and internal team notes.",
		Annotations: readOnly,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		MessageID string `json:"message_id" jsonschema:"the internal message ID"`
	}) (*mcp.CallToolResult, any, error) {
		v, err := app.GetMessageCollab(ctx, in.MessageID)
		if err != nil {
			return nil, nil, err
		}
		return nil, v, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_assign",
		Description: "Assign a message to a team member (or clear with an empty assignee).",
		Annotations: writeLocal,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		MessageID string `json:"message_id" jsonschema:"the internal message ID"`
		Assignee  string `json:"assignee" jsonschema:"assignee identifier (login/email); empty clears"`
	}) (*mcp.CallToolResult, any, error) {
		mc, err := app.AssignMessage(ctx, in.MessageID, in.Assignee)
		if err != nil {
			return nil, nil, err
		}
		return nil, mc, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_set_work_status",
		Description: "Set a message's collaboration work status (open|pending|resolved).",
		Annotations: writeLocal,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		MessageID string `json:"message_id" jsonschema:"the internal message ID"`
		Status    string `json:"status" jsonschema:"open|pending|resolved"`
	}) (*mcp.CallToolResult, any, error) {
		mc, err := app.SetMessageWorkStatus(ctx, in.MessageID, in.Status)
		if err != nil {
			return nil, nil, err
		}
		return nil, mc, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_add_note",
		Description: "Add an internal team note to a message (never sent to anyone).",
		Annotations: writeLocal,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		MessageID string `json:"message_id" jsonschema:"the internal message ID"`
		Body      string `json:"body" jsonschema:"the note text"`
	}) (*mcp.CallToolResult, any, error) {
		n, err := app.AddMessageNote(ctx, in.MessageID, in.Body)
		if err != nil {
			return nil, nil, err
		}
		return nil, n, nil
	})
}

// ---------- audit tools ----------

func registerAuditTools(s *mcp.Server, app *application.App) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "mail_audit_search",
		Description: "Read recent audit events (account changes, secret usage, sync, AI analysis, approvals, sends).",
		Annotations: readOnly,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
		Limit int `json:"limit,omitempty" jsonschema:"max events to return, default 100"`
	}) (*mcp.CallToolResult, any, error) {
		evs, err := app.SearchAudit(ctx, in.Limit)
		if err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"events": evs}, nil
	})
}

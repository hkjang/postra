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
)

const serverVersion = "0.1.0"

func boolPtr(b bool) *bool { return &b }

var (
	readOnly    = &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: boolPtr(false), OpenWorldHint: boolPtr(false)}
	readExtern  = &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: boolPtr(false), OpenWorldHint: boolPtr(true)}
	writeLocal  = &mcp.ToolAnnotations{DestructiveHint: boolPtr(false), OpenWorldHint: boolPtr(false)}
	writeExtern = &mcp.ToolAnnotations{DestructiveHint: boolPtr(false), OpenWorldHint: boolPtr(true)}
	destructive = &mcp.ToolAnnotations{DestructiveHint: boolPtr(true), OpenWorldHint: boolPtr(true)}
)

// NewServer builds the MCP server with the full tool catalog.
func NewServer(app *application.App) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "postra-mail", Version: serverVersion}, nil)
	registerAccountTools(s, app)
	registerSyncTools(s, app)
	registerQueryTools(s, app)
	registerAITools(s, app)
	registerComposeTools(s, app)
	registerAuditTools(s, app)
	registerResources(s, app)
	registerPrompts(s, app)
	return s
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
			"sync":  {"mail_sync_start", "job_status", "job_cancel"},
			"query": {"mail_search", "mail_message_get", "mail_thread_get", "mail_attachment_list"},
			"ai": {"mail_summarize", "mail_classify", "mail_action_items_extract",
				"mail_entities_extract", "mail_phishing_inspect", "mail_thread_summarize", "mail_question_answer"},
			"compose_send": {"mail_draft_create", "mail_draft_update", "mail_draft_rewrite",
				"mail_send_preview", "mail_send_request_approval", "mail_send", "mail_outbound_status"},
			"audit": {"mail_audit_search"},
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
		if apiToken != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(apiToken)) != 1 {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
		}
		inner.ServeHTTP(w, r.WithContext(application.WithActor(r.Context(), "mcp")))
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
		job, err := app.StartSync(ctx, in.AccountID, application.SyncOptions{MaxMessages: in.MaxMessages})
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
		Description: "Get a parsed message: headers, text body, sanitized HTML, attachment list.",
		Annotations: readOnly,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in messageIDInput) (*mcp.CallToolResult, any, error) {
		mv, err := app.GetMessage(ctx, in.MessageID, true)
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

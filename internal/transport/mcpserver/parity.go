package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"postra/internal/application"
)

func registerParityTools(s *mcp.Server, app *application.App) {
	addTool(s, &mcp.Tool{Name: "mail_action_card_create", Description: "Create a local pending action linked to an owned message after the user requests it. Accepts a title, optional details/assignee and explicit ISO due date. Does not call AI, send mail, or create an external task.", Annotations: writeLocal},
		func(ctx context.Context, _ *mcp.CallToolRequest, input application.CreateActionCardInput) (*mcp.CallToolResult, any, error) {
			out, err := app.CreateActionCard(ctx, input)
			return nil, out, err
		})
	addTool(s, &mcp.Tool{Name: "mail_events", Description: "Read the current user's notification metadata snapshot, honoring organization and personal notification preferences. Includes job/send/action/security status only, never message bodies, recipients, error details or secrets. Same data as REST /api/events; poll at poll_seconds rather than inventing successful progress.", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, any, error) {
			out, err := app.NotificationEvents(ctx)
			return nil, out, err
		})
	addTool(s, &mcp.Tool{Name: "mail_drafts_list", Description: "List the current user's persisted drafts with pagination. status defaults to open; cursor must come from next_cursor. Never returns another user's drafts.", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Status string `json:"status,omitempty"`
			Limit  int    `json:"limit,omitempty"`
			Cursor string `json:"cursor,omitempty"`
		}) (*mcp.CallToolResult, any, error) {
			out, err := app.ListDrafts(ctx, in.Status, in.Limit, in.Cursor)
			return nil, out, err
		})
	addTool(s, &mcp.Tool{Name: "mail_draft_delete", Description: "Discard a saved unsent draft after explicit confirmation. Sent drafts cannot be discarded.", Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPtr(true), OpenWorldHint: boolPtr(false)}},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			DraftID string `json:"draft_id"`
			Confirm bool   `json:"confirm"`
		}) (*mcp.CallToolResult, any, error) {
			if !in.Confirm {
				return nil, nil, &application.UserError{Msg: "set confirm=true after the user approved discarding the draft"}
			}
			if err := app.DiscardDraft(ctx, in.DraftID); err != nil {
				return nil, nil, err
			}
			return nil, map[string]string{"status": "discarded"}, nil
		})
	addTool(s, &mcp.Tool{Name: "mail_outbound_list", Description: "List current-user outbound delivery status. Use this before retrying an uncertain or timed-out send.", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Limit int `json:"limit,omitempty"`
		}) (*mcp.CallToolResult, any, error) {
			out, err := app.ListOutbound(ctx, in.Limit)
			return nil, map[string]any{"outbound": out}, err
		})
	addTool(s, &mcp.Tool{Name: "job_list", Description: "List the current user's jobs, including background sync progress. Reading does not start a new job.", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Limit int `json:"limit,omitempty"`
		}) (*mcp.CallToolResult, any, error) {
			out, err := app.ListJobs(ctx, in.Limit)
			return nil, map[string]any{"jobs": out}, err
		})
	addTool(s, &mcp.Tool{Name: "mail_set_sla", Description: "Set a current-user message's SLA deadline as Unix seconds; zero clears the deadline. Does not send mail or contact a remote system.", Annotations: writeLocal},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			MessageID string `json:"message_id"`
			SLADue    int64  `json:"sla_due"`
		}) (*mcp.CallToolResult, any, error) {
			out, err := app.SetMessageSLA(ctx, in.MessageID, in.SLADue)
			return nil, out, err
		})
}

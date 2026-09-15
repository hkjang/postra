package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"postra/internal/application"
	"postra/internal/mailrender"
)

func registerRenderTools(s *mcp.Server, app *application.App) {
	addTool(s, &mcp.Tool{Name: "mail_text_rewrite", Description: "AI-rewrite only the provided selected text without attaching a mail signature. Returns a proposal; does not change any saved draft. Ask the user before applying the result.", Annotations: readExtern},
		func(ctx context.Context, _ *mcp.CallToolRequest, in application.RewriteMailTextInput) (*mcp.CallToolResult, any, error) {
			out, err := app.RewriteMailText(ctx, in)
			return nil, map[string]string{"text": out}, err
		})
	addTool(s, &mcp.Tool{Name: "mail_render", Description: "Render text, Markdown or HTML into matching safe HTML/plain alternatives using the same application service as the web composer. format defaults to auto; template supports clean/formal/concise/notice/report/newsletter/plain. Never sends or saves a draft. smart_format is opt-in and additionally requires mail.ai.", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in application.RenderMailInput) (*mcp.CallToolResult, any, error) {
			out, err := app.RenderMail(ctx, in)
			return nil, out, err
		})
	addTool(s, &mcp.Tool{Name: "mail_templates", Description: "List the local mail formatting templates supported by both web and MCP. Does not call any external service.", Annotations: readOnly},
		func(context.Context, *mcp.CallToolRequest, emptyInput) (*mcp.CallToolResult, any, error) {
			return nil, map[string]any{"templates": mailrender.Templates()}, nil
		})
	addTool(s, &mcp.Tool{Name: "mail_signature_list", Description: "List only the current user's saved mail signatures; never lists another user's signatures.", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, any, error) {
			out, err := app.ListMailSignatures(ctx)
			return nil, map[string]any{"signatures": out}, err
		})
	addTool(s, &mcp.Tool{Name: "mail_signature_get", Description: "Read one current-user mail signature by ID.", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			ID string `json:"id"`
		}) (*mcp.CallToolResult, any, error) {
			out, err := app.GetMailSignature(ctx, in.ID)
			return nil, out, err
		})
	addTool(s, &mcp.Tool{Name: "mail_signature_save", Description: "Create or update the current user's mail signature. Body is sanitized by the shared renderer; account_id optionally restricts where it can be used.", Annotations: writeLocal},
		func(ctx context.Context, _ *mcp.CallToolRequest, in application.SaveMailSignatureInput) (*mcp.CallToolResult, any, error) {
			out, err := app.SaveMailSignature(ctx, in)
			return nil, out, err
		})
	addTool(s, &mcp.Tool{Name: "mail_signature_delete", Description: "Delete a current-user mail signature after explicit confirmation. Does not delete mail or another user's data.", Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPtr(true), OpenWorldHint: boolPtr(false)}},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			ID      string `json:"id"`
			Confirm bool   `json:"confirm"`
		}) (*mcp.CallToolResult, any, error) {
			if !in.Confirm {
				return nil, nil, &application.UserError{Msg: "set confirm=true after the user approved deleting this signature"}
			}
			if err := app.DeleteMailSignature(ctx, in.ID); err != nil {
				return nil, nil, err
			}
			return nil, map[string]string{"status": "deleted"}, nil
		})
}

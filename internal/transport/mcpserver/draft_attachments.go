package mcpserver

import (
	"context"
	"encoding/base64"
	"net/url"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"postra/internal/application"
	"postra/internal/domain"
)

const mcpAttachmentBytes = 1 << 20

func registerDraftAttachmentTools(s *mcp.Server, app *application.App) {
	addTool(s, &mcp.Tool{Name: "mail_draft_attachment_add", Description: "Attach at most 1 MiB of base64 file content to an owned draft using the web composer's shared scan/policy service. Creates a new draft version and invalidates previous approvals. For larger files use the authenticated REST attachment upload. inline supports only approved image types.", Annotations: writeLocal},
		func(ctx context.Context, _ *mcp.CallToolRequest, in application.AddDraftAttachmentInput) (*mcp.CallToolResult, any, error) {
			if len(in.DataBase64) > base64.StdEncoding.EncodedLen(mcpAttachmentBytes) {
				return nil, nil, &domain.PublicError{Code: "payload_too_large", Message: "MCP 첨부는 1 MiB까지 지원합니다. 큰 파일은 웹 작성기를 사용하세요.", Status: 413}
			}
			out, err := app.AddDraftAttachment(ctx, in)
			return nil, out, err
		})
	addTool(s, &mcp.Tool{Name: "mail_draft_attachment_remove", Description: "Remove one attachment from the current user's draft after explicit confirmation. Creates a new version and invalidates prior send approvals; does not send mail.", Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPtr(true), OpenWorldHint: boolPtr(false)}},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			DraftID      string `json:"draft_id"`
			AttachmentID string `json:"attachment_id"`
			Confirm      bool   `json:"confirm"`
		}) (*mcp.CallToolResult, any, error) {
			if !in.Confirm {
				return nil, nil, &application.UserError{Msg: "set confirm=true after the user approved removing this attachment"}
			}
			out, err := app.RemoveDraftAttachment(ctx, in.DraftID, in.AttachmentID)
			return nil, out, err
		})
	addTool(s, &mcp.Tool{Name: "mail_draft_attachment_get", Description: "Read owned draft attachment metadata and a relative authenticated REST download URL. File content is excluded unless include_content=true; base64 responses are capped at 1 MiB. version=0 selects the current draft version. Download URLs require the same authenticated user's mail.draft permission.", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			DraftID        string `json:"draft_id"`
			AttachmentID   string `json:"attachment_id"`
			Version        int    `json:"version,omitempty"`
			IncludeContent bool   `json:"include_content,omitempty"`
		}) (*mcp.CallToolResult, any, error) {
			attachment, data, err := app.GetDraftAttachment(ctx, in.DraftID, in.AttachmentID, in.Version)
			if err != nil {
				return nil, nil, err
			}
			path := "/api/drafts/" + url.PathEscape(in.DraftID) + "/attachments/" + url.PathEscape(in.AttachmentID)
			if in.Version > 0 {
				path += "?version=" + strconv.Itoa(in.Version)
			}
			out := map[string]any{"id": attachment.ID, "name": attachment.Name, "mime_type": attachment.MIMEType, "size": attachment.Size, "inline": attachment.Inline, "content_id": attachment.ContentID, "scan_status": attachment.ScanStatus, "download_url": path}
			if in.IncludeContent {
				if len(data) > mcpAttachmentBytes {
					return nil, nil, &domain.PublicError{Code: "payload_too_large", Message: "파일은 1 MiB 응답 한도를 초과합니다. 인증된 다운로드 경로를 사용하세요.", Status: 413, Details: map[string]any{"download_url": path}}
				}
				out["data_base64"] = base64.StdEncoding.EncodeToString(data)
			}
			return nil, out, nil
		})
}

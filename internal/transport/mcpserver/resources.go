package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"postra/internal/application"
)

// registerResources exposes application data as MCP resources (§10.3).
// Resource reads go through the same use cases as tools, so account/user
// scoping and auditing are identical. Secret material is never exposed.
func registerResources(s *mcp.Server, app *application.App) {
	jsonContents := func(uri string, v any) (*mcp.ReadResourceResult, error) {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return nil, err
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{
			{URI: uri, MIMEType: "application/json", Text: string(b)},
		}}, nil
	}

	// Fixed resources.
	s.AddResource(&mcp.Resource{
		URI: "policy://mail/current", Name: "mail-policy",
		Description: "Currently applied non-sensitive mail policy.", MIMEType: "application/json",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return jsonContents(req.Params.URI, app.PolicySnapshot())
	})

	s.AddResource(&mcp.Resource{
		URI: "schema://mail/tools", Name: "mail-tool-schema",
		Description: "Names and input schema summary of available MCP tools.", MIMEType: "application/json",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return jsonContents(req.Params.URI, toolCatalogSummary())
	})

	// Templated resources (RFC 6570). The SDK matches these against read URIs.
	s.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "mail://accounts/{account_id}", Name: "mail-account",
		Description: "Non-sensitive settings of a mail account.", MIMEType: "application/json",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		id := lastSegment(req.Params.URI)
		acc, err := app.GetAccount(ctx, id)
		if err != nil {
			return nil, mcp.ResourceNotFoundError(req.Params.URI)
		}
		return jsonContents(req.Params.URI, acc)
	})

	s.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "mail://messages/{message_id}", Name: "mail-message",
		Description: "Parsed message with body and attachment metadata.", MIMEType: "application/json",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		mv, err := app.GetMessage(ctx, lastSegment(req.Params.URI), true)
		if err != nil {
			return nil, mcp.ResourceNotFoundError(req.Params.URI)
		}
		return jsonContents(req.Params.URI, mv)
	})

	s.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "mail://messages/{message_id}/raw", Name: "mail-message-raw",
		Description: "Original RFC822 MIME bytes (access-controlled, audited).", MIMEType: "message/rfc822",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		// URI form: mail://messages/<id>/raw
		id := lastSegment(strings.TrimSuffix(req.Params.URI, "/raw"))
		rc, err := app.GetRawMessage(ctx, id)
		if err != nil {
			return nil, mcp.ResourceNotFoundError(req.Params.URI)
		}
		defer rc.Close()
		raw, err := io.ReadAll(rc)
		if err != nil {
			return nil, err
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{
			{URI: req.Params.URI, MIMEType: "message/rfc822", Text: string(raw)},
		}}, nil
	})

	s.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "mail://threads/{thread_id}", Name: "mail-thread",
		Description: "Thread messages in chronological order.", MIMEType: "application/json",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		tv, err := app.GetThread(ctx, lastSegment(req.Params.URI), true)
		if err != nil {
			return nil, mcp.ResourceNotFoundError(req.Params.URI)
		}
		return jsonContents(req.Params.URI, tv)
	})

	s.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "mail://drafts/{draft_id}", Name: "mail-draft",
		Description: "Draft with its current version.", MIMEType: "application/json",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		dv, err := app.GetDraft(ctx, lastSegment(req.Params.URI))
		if err != nil {
			return nil, mcp.ResourceNotFoundError(req.Params.URI)
		}
		return jsonContents(req.Params.URI, dv)
	})

	s.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "mail://sync-jobs/{job_id}", Name: "mail-sync-job",
		Description: "Status and statistics of a sync/analysis job.", MIMEType: "application/json",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		job, err := app.GetJob(ctx, lastSegment(req.Params.URI))
		if err != nil {
			return nil, mcp.ResourceNotFoundError(req.Params.URI)
		}
		return jsonContents(req.Params.URI, job)
	})
}

func lastSegment(uri string) string {
	i := strings.LastIndex(uri, "/")
	if i < 0 {
		return uri
	}
	return uri[i+1:]
}

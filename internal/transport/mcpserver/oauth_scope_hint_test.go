package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A scope denial must stay inside the MCP contract. The scope hint only fires
// on the configured MCP endpoint, so mounting the handler at the root — as the
// other tests do — never exercises it; every real deployment serves /mcp, and
// there an HTTP error status tears the client's transport down. Fixture users
// have mail.read and mail.search only.
func TestMCPOAuthScopeDenialKeepsTheSessionAlive(t *testing.T) {
	f := newMCPOAuthFixture(t)
	root := http.NewServeMux()
	root.Handle("/mcp", HTTPHandler(f.app, ""))
	server := httptest.NewServer(root)
	t.Cleanup(server.Close)
	if f.app.MCPOAuthConnection().ActiveEndpoint != "/mcp" {
		t.Fatal("fixture no longer serves the endpoint the hint matches")
	}

	token := f.token(t, nil)
	transport := &bearerRoundTripper{key: token}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "1"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: &http.Client{Transport: transport}}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "mail_draft", Arguments: map[string]any{"account_id": "acc_mcp", "kind": "new"}})
	if err != nil {
		t.Fatalf("out-of-scope tool call broke the transport instead of returning a tool error: %v", err)
		denial := requireToolError(t, result, "insufficient_scope")
		raw, _ := json.Marshal(denial.Details)
		if !strings.Contains(string(raw), "mail.draft") {
			t.Fatalf("scope denial did not name its missing scope: %s", raw)
		}
	}
	// The session must still carry the calls the token does cover.
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "mail_identity", Arguments: map[string]any{}}); err != nil {
		t.Fatalf("session did not survive the scope denial: %v", err)
	}

	// The step-up challenge still rides along, so an OAuth client can ask its
	// user for the missing scope.
	call := `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"mail_draft","arguments":{"account_id":"acc_mcp","kind":"new"}}}`
	req, _ := http.NewRequest("POST", server.URL+"/mcp", strings.NewReader(call))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(res.Header.Get("WWW-Authenticate"), `scope="mail.draft"`) ||
		!strings.Contains(res.Header.Get("WWW-Authenticate"), `error="insufficient_scope"`) {
		t.Fatalf("lost the scope step-up hint: status=%d challenge=%q", res.StatusCode, res.Header.Get("WWW-Authenticate"))
	}
}

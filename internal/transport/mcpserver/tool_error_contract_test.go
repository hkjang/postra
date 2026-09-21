package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"postra/internal/application"
)

// Tools that need an AI or embedding provider this fixture does not stub.
var toolErrorSweepSkip = map[string]bool{
	"mail_hybrid_search": true, "mail_semantic_search": true, "mail_embeddings_build": true,
	"mail_summarize": true, "mail_classify": true, "mail_action_items_extract": true,
	"mail_entities_extract": true, "mail_phishing_inspect": true, "mail_thread_summarize": true,
	"mail_attachment_summarize": true, "mail_daily_digest": true, "mail_eval_prompt": true,
	"mail_question_answer": true, "mail_calendar_extract": true, "mail_suggest_replies": true,
	"mail_rule_draft_from_text": true, "mail_text_rewrite": true, "mail_draft_rewrite": true,
	"mail_action_cards_extract": true, "mail_draft": true,
}

// Every response a client receives must satisfy the contract that client was
// given: when structured content is present it conforms to the tool's declared
// output schema, and a failure carries none, because an error envelope can
// never conform. An OAuth token holds only the scopes Keycloak granted, so
// most calls in this sweep are refused — which is exactly the traffic that
// made a non-conforming error envelope visible on nearly every response.
func TestToolResponsesAlwaysSatisfyTheirPublishedSchema(t *testing.T) {
	f := newMCPOAuthFixture(t)
	root := http.NewServeMux()
	root.Handle("/mcp", HTTPHandler(f.app, ""))
	server := httptest.NewServer(root)
	t.Cleanup(server.Close)

	client, _ := convergenceClient(t, server.URL+"/mcp", f.token(t, nil)) // mail.read mail.search
	list, err := client.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(list.Tools, func(i, j int) bool { return list.Tools[i].Name < list.Tools[j].Name })

	structured, refused := 0, 0
	for _, tool := range list.Tools {
		if application.CanonicalMCPTool(tool.Name) != tool.Name || toolErrorSweepSkip[tool.Name] {
			continue
		}
		args := map[string]any{}
		if meta, isMap := tool.Meta["postra"].(map[string]any); isMap {
			if examples, isList := meta["examples"].([]any); isList && len(examples) > 0 {
				if first, isMap := examples[0].(map[string]any); isMap {
					args = first
				}
			}
		}
		res, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: tool.Name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: transport failure: %v", tool.Name, err)
		}
		if res.IsError {
			refused++
			if res.StructuredContent != nil {
				raw, _ := json.Marshal(res.StructuredContent)
				t.Errorf("%s: failure carries structured content that cannot match its output schema: %s", tool.Name, raw)
			}
			continue
		}
		if res.StructuredContent == nil {
			continue
		}
		structured++
		raw, _ := json.Marshal(res.StructuredContent)
		var wire any
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatal(err)
		}
		resolved, err := outputSchema(tool.Name).Resolve(nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := resolved.Validate(wire); err != nil {
			t.Errorf("%s: response does not match the schema the client was given: %v\n  got: %.400s", tool.Name, err, raw)
		}
	}
	if structured == 0 || refused == 0 {
		t.Fatalf("sweep proved nothing: %d structured successes, %d refusals", structured, refused)
	}
	t.Logf("validated %d structured successes and %d refusals", structured, refused)
}

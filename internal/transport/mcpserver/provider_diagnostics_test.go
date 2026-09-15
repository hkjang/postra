package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"postra/internal/application"
	"postra/internal/domain"
)

const mcpProviderEcho = "unpatterned-provider-password-echo-319"

type mcpEchoSMTP struct{}

func (mcpEchoSMTP) TestConnection(context.Context, domain.SMTPSendOptions) (*domain.ConnDiagnostics, error) {
	return &domain.ConnDiagnostics{Target: "smtp", Steps: []domain.ConnStep{{Step: "smtp_ehlo_auth", Detail: mcpProviderEcho}}}, nil
}
func (mcpEchoSMTP) Send(context.Context, domain.SMTPSendOptions, domain.Envelope, io.Reader) (domain.SendReceipt, error) {
	return domain.SendReceipt{}, errors.New(mcpProviderEcho)
}

func TestMCPProviderDiagnosticsSuccessPayloadsDoNotEchoSecrets(t *testing.T) {
	app, ctx, _ := convergenceApp(t)
	app.SMTP = mcpEchoSMTP{}
	principal, _ := application.PrincipalFrom(ctx)
	if err := app.Store.CreateJob(ctx, &domain.Job{ID: "diagnostic-job", UserID: principal.UserID, Type: "embed", Status: domain.JobFailed, Error: mcpProviderEcho}); err != nil {
		t.Fatal(err)
	}
	_, key, err := app.CreateMCPKeyWithScopes(ctx, "safe diagnostics", []string{"mail.read", "mail.draft", "mail.send"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(HTTPHandler(app, ""))
	t.Cleanup(server.Close)
	client, _ := convergenceClient(t, server.URL, key)
	call := func(name string, input any) map[string]any {
		t.Helper()
		result := mcpCall(t, client, name, input)
		wire, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(wire), mcpProviderEcho) {
			t.Fatalf("%s exposed provider secret in content/structuredContent", name)
		}
		return mcpDecode[map[string]any](t, result)
	}
	draft := call("mail_draft_create", map[string]any{"account_id": "acc_mcp", "kind": "new", "to": []string{"colleague@corp.local"}, "subject": "진단 검증", "body": "본문"})
	draftID := draft["draft"].(map[string]any)["id"].(string)
	approved := call("mail_send_request_approval", map[string]any{"draft_id": draftID})
	out := call("mail_send", map[string]any{"draft_id": draftID, "approval_token": approved["approval"].(map[string]any)["token"], "idempotency_key": "diagnostic-once"})
	if out["status"] != "failed" {
		t.Fatalf("provider failure changed delivery state: %v", out["status"])
	}
	for _, test := range []struct {
		name string
		args map[string]any
	}{
		{"mail_outbound_status", map[string]any{"outbound_id": out["id"]}},
		{"mail_outbound_list", map[string]any{}},
		{"job_status", map[string]any{"job_id": "diagnostic-job"}},
		{"job_list", map[string]any{}},
		{"mail_account_test", map[string]any{"account_id": "acc_mcp"}},
	} {
		call(test.name, test.args)
	}
}

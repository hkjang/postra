package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"postra/internal/domain"
)

func TestRESTHistoricalProviderDiagnosticsAreNotExposed(t *testing.T) {
	app := browserTestApp(t, true)
	owner, _ := browserIdentity(t, app, "diagnostic-owner", domain.RoleUser)
	other, _ := browserIdentity(t, app, "diagnostic-admin", domain.RoleAdmin)
	const secret = "unpatterned-provider-password-echo-319"
	ctx := context.Background()
	if err := app.Store.CreateJob(ctx, &domain.Job{ID: "diagnostic-job", UserID: "diagnostic-owner", Type: "sync", Status: domain.JobFailed, Error: secret}); err != nil {
		t.Fatal(err)
	}
	if err := app.Store.CreateOutbound(ctx, &domain.OutboundMessage{ID: "diagnostic-out", UserID: "diagnostic-owner", DraftID: "retired-draft", IdempotencyKey: "diagnostic-idem", Status: domain.OutboundUncertain, SMTPResponse: secret}); err != nil {
		t.Fatal(err)
	}
	handler := New(app, "").Handler()
	for _, path := range []string{"/api/jobs", "/api/jobs/diagnostic-job", "/api/outbound", "/api/outbound/diagnostic-out", "/api/v1/jobs", "/api/v1/outbound"} {
		response := browserRequest(handler, "GET", path, owner, "", "", "")
		if response.Code != http.StatusOK || strings.Contains(response.Body.String(), secret) {
			t.Fatalf("unsafe historical DTO at %s (status %d)", path, response.Code)
		}
	}
	for _, path := range []string{"/api/jobs/diagnostic-job", "/api/outbound/diagnostic-out"} {
		response := browserRequest(handler, "GET", path, other, "", "", "")
		if response.Code != http.StatusNotFound || strings.Contains(response.Body.String(), secret) {
			t.Fatalf("administrator crossed diagnostic owner boundary: %s", path)
		}
	}
}

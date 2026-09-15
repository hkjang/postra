package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"postra/internal/application"
	"postra/internal/domain"
	"postra/internal/platform/telemetry"
)

func TestEventsAuthOriginAndSnapshotPrivacy(t *testing.T) {
	app := browserTestApp(t, true)
	owner, _ := browserIdentity(t, app, "stream-owner", domain.RoleAdmin)
	other, _ := browserIdentity(t, app, "stream-other", domain.RoleUser)
	for _, id := range []string{"stream-owner", "stream-other"} {
		if err := app.Store.CreateJob(context.Background(), &domain.Job{ID: "job-" + id, UserID: id, Type: "sync", Status: domain.JobRunning, Error: "secret-provider-password", Progress: "private-message-body"}); err != nil {
			t.Fatal(err)
		}
	}
	handler := New(app, "").Handler()
	for _, test := range []struct {
		cookie, origin string
		want           int
	}{
		{"", "", http.StatusUnauthorized},
		{owner, "https://attacker.invalid", http.StatusForbidden},
		{owner, "https://postra.test", http.StatusOK},
		{other, "", http.StatusOK},
	} {
		response := browserRequest(handler, "GET", "/api/events?once=true", test.cookie, "", test.origin, "")
		if response.Code != test.want {
			t.Fatalf("events response %d, want %d: %s", response.Code, test.want, response.Body)
		}
		if test.want != http.StatusOK {
			continue
		}
		body := response.Body.String()
		own, foreign := "stream-owner", "stream-other"
		if test.cookie == other {
			own, foreign = foreign, own
		}
		if !strings.Contains(body, "job-"+own) || strings.Contains(body, foreign) || strings.Contains(body, "secret-provider-password") || strings.Contains(body, "private-message-body") {
			t.Fatalf("incorrect snapshot: %s", body)
		}
	}
}

func readSSE(t *testing.T, reader *bufio.Scanner) (string, string) {
	t.Helper()
	name, data := "", ""
	for reader.Scan() {
		line := reader.Text()
		if line == "" && name != "" {
			return name, data
		}
		if strings.HasPrefix(line, "event: ") {
			name = strings.TrimPrefix(line, "event: ")
		}
		if strings.HasPrefix(line, "data: ") {
			data = strings.TrimPrefix(line, "data: ")
		}
	}
	t.Fatalf("stream closed before event: %v", reader.Err())
	return "", ""
}

func TestEventsSSEFlushAndLiveSessionRevocation(t *testing.T) {
	app := browserTestApp(t, true)
	raw, _ := browserIdentity(t, app, "stream-live", domain.RoleUser)
	server := httptest.NewServer(telemetry.HTTPMiddleware(New(app, "").Handler()))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/events", nil)
	request.AddCookie(&http.Cookie{Name: "postra_session", Value: raw})
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") || response.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("SSE not established: %d %+v", response.StatusCode, response.Header)
	}
	scanner := bufio.NewScanner(response.Body)
	name, data := readSSE(t, scanner)
	var snapshot application.NotificationSnapshot
	if err := json.Unmarshal([]byte(data), &snapshot); err != nil || name != "snapshot" || snapshot.UserID != "stream-live" {
		t.Fatalf("missing initial flushed snapshot: %s %s %v", name, data, err)
	}
	session, _, err := app.AuthenticateSession(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Logout(context.Background(), session.ID); err != nil {
		t.Fatal(err)
	}
	name, data = readSSE(t, scanner)
	if name != "session_expired" || !strings.Contains(data, "authentication_required") || strings.Contains(data, raw) {
		t.Fatalf("revoked session remained open: %s %s", name, data)
	}
}

func TestEventsSSELiveMCPKeyScopeReduction(t *testing.T) {
	app := browserTestApp(t, true)
	rawSession, _ := browserIdentity(t, app, "stream-key", domain.RoleUser)
	_, principal, err := app.AuthenticateSession(context.Background(), rawSession)
	if err != nil {
		t.Fatal(err)
	}
	owner := application.WithPrincipal(context.Background(), principal)
	key, rawKey, err := app.CreateMCPKey(owner, "event-client")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(telemetry.HTTPMiddleware(New(app, "").Handler()))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/events", nil)
	request.Header.Set("Authorization", "Bearer "+rawKey)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("key stream status %d", response.StatusCode)
	}
	scanner := bufio.NewScanner(response.Body)
	name, _ := readSSE(t, scanner)
	if name != "snapshot" {
		t.Fatal("key did not receive snapshot")
	}
	if _, err := app.UpdateMCPKeyScopes(owner, key.ID, []string{}, false); err != nil {
		t.Fatal(err)
	}
	name, data := readSSE(t, scanner)
	if name != "stream_error" || !strings.Contains(data, "insufficient_scope") || strings.Contains(data, rawKey) {
		t.Fatalf("scope reduction not applied: %s %s", name, data)
	}
}

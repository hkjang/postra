package application

import (
	"context"
	"io"
	"strings"
	"testing"

	"postra/internal/domain"
)

// fakeInbound is a stub InboundDialer/Session used to prove the sync loop
// selects the IMAP adapter for imap accounts and ingests through it.
type fakeInbound struct{ raw map[string]string } // "validity.uid" -> RFC822

type fakeInboundSess struct {
	d    *fakeInbound
	msgs []domain.RemoteMessage
}

func (f *fakeInbound) Dial(context.Context, domain.InboundDialOptions) (domain.InboundSession, error) {
	s := &fakeInboundSess{d: f}
	n := 0
	for id := range f.raw {
		n++
		s.msgs = append(s.msgs, domain.RemoteMessage{Number: n, UIDL: id, Size: int64(len(f.raw[id]))})
	}
	return s, nil
}

func (s *fakeInboundSess) List(context.Context) ([]domain.RemoteMessage, error) { return s.msgs, nil }
func (s *fakeInboundSess) UIDL(context.Context) ([]domain.RemoteMessage, error) { return s.msgs, nil }
func (s *fakeInboundSess) Top(context.Context, int, int) (io.ReadCloser, error) { return nil, nil }
func (s *fakeInboundSess) Delete(context.Context, int) error                    { return nil }
func (s *fakeInboundSess) Quit(context.Context) error                           { return nil }
func (s *fakeInboundSess) Close() error                                         { return nil }
func (s *fakeInboundSess) Retrieve(_ context.Context, n int) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(s.d.raw[s.msgs[n-1].UIDL])), nil
}

func TestSyncViaIMAP(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	app.IMAP = &fakeInbound{raw: map[string]string{
		"777.101": testMail("i1", "imap one", "hello via imap"),
	}}
	ctx := WithActor(context.Background(), "test")

	ref, err := app.RegisterSecret(ctx, domain.SecretMailPassword, "t", domain.NewSecretHandle([]byte("pw")))
	if err != nil {
		t.Fatal(err)
	}
	acc, err := app.CreateAccount(ctx, CreateAccountInput{
		Name: "IMAP", Email: "me@corp.local", InboundProtocol: "imap",
		POP3Host: "127.0.0.1", POP3Security: "none", POP3Username: "me", POP3SecretRef: string(ref),
		SMTPHost: "127.0.0.1", SMTPSecurity: "none", SMTPAuth: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	if acc.InboundProtocol != "imap" {
		t.Fatalf("account protocol = %q, want imap", acc.InboundProtocol)
	}

	job := syncAndWait(t, app, acc.ID)
	if job.Status != domain.JobSucceeded {
		t.Fatalf("sync status = %s (%s)", job.Status, job.Error)
	}
	res, err := app.Search(ctx, domain.SearchQuery{UserID: DefaultUserID, AccountID: acc.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 1 || res.Messages[0].Subject != "imap one" {
		t.Fatalf("ingested via IMAP = %+v, want 1 message 'imap one'", res.Messages)
	}

	// A second sync is idempotent on the same UID checkpoint (no duplicate).
	_ = syncAndWait(t, app, acc.ID)
	res2, _ := app.Search(ctx, domain.SearchQuery{UserID: DefaultUserID, AccountID: acc.ID, Limit: 10})
	if len(res2.Messages) != 1 {
		t.Fatalf("after re-sync = %d messages, want 1 (idempotent)", len(res2.Messages))
	}
}

package application

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"postra/internal/adapters/imap"
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

// Exercise UIDL -> List through the production adapter: a metadata rejection
// must not turn into a successful sync of an apparently empty mailbox.
func TestSyncIMAPEnumerationFailureIsNotSuccess(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	app.IMAP = imap.Dialer{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	const marker = "imap-enumeration-server-marker"
	var mu sync.Mutex
	var commands []string
	done := make(chan struct{})
	t.Cleanup(func() {
		_ = ln.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("IMAP server did not stop")
		}
	})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		br := bufio.NewReader(conn)
		_, _ = io.WriteString(conn, "* OK IMAP4rev1 ready\r\n")
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			sp := strings.SplitN(strings.TrimSpace(line), " ", 3)
			if len(sp) < 2 {
				return
			}
			tag, cmd := sp[0], sp[1]
			recorded := cmd
			if cmd == "FETCH" && len(sp) == 3 {
				recorded += " " + sp[2]
			}
			mu.Lock()
			commands = append(commands, recorded)
			mu.Unlock()
			switch cmd {
			case "LOGIN", "LIST":
				fmt.Fprintf(conn, "%s OK\r\n", tag)
			case "SELECT":
				fmt.Fprintf(conn, "* 1 EXISTS\r\n* OK [UIDVALIDITY 777] ok\r\n%s OK\r\n", tag)
			case "FETCH":
				fmt.Fprintf(conn, "%s NO %s\r\n", tag, marker)
			case "LOGOUT":
				fmt.Fprintf(conn, "* BYE\r\n%s OK\r\n", tag)
				return
			default:
				fmt.Fprintf(conn, "%s BAD unsupported\r\n", tag)
			}
		}
	}()

	ctx := WithActor(context.Background(), "test")
	ref, err := app.RegisterSecret(ctx, domain.SecretMailPassword, "t", domain.NewSecretHandle([]byte("pw")))
	if err != nil {
		t.Fatal(err)
	}
	acc, err := app.CreateAccount(ctx, CreateAccountInput{
		Name: "IMAP", Email: "me@corp.local", InboundProtocol: "imap",
		POP3Host: "127.0.0.1", POP3Port: ln.Addr().(*net.TCPAddr).Port, POP3Security: "none",
		POP3Username: "me", POP3SecretRef: string(ref),
		SMTPHost: "127.0.0.1", SMTPSecurity: "none", SMTPAuth: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	job := syncAndWait(t, app, acc.ID)
	if job.Status != domain.JobFailed {
		t.Errorf("sync status = %s, want failed (stats=%v error=%q)", job.Status, job.Stats, job.Error)
	}
	d := job.Diagnostic
	if d == nil || d.Stage != domain.StageEnumerate || d.Command != "FETCH" || d.Class != "rejected" || d.Protocol != "imap" {
		t.Errorf("diagnostic = %+v, want IMAP enumerate/FETCH rejection", d)
	}
	stored, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), marker) {
		t.Error("server text leaked into the stored job error or diagnostic")
	}
	// Wait for the real session close, before cleanup can close the listener.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sync left its IMAP connection open")
	}
	mu.Lock()
	got := slices.Clone(commands)
	mu.Unlock()
	want := []string{"LOGIN", "SELECT", "FETCH 1:1 (UID RFC822.SIZE)", "FETCH 1:1 (UID RFC822.SIZE)"}
	if !slices.Equal(got, want) {
		t.Fatalf("IMAP commands = %v, want %v (UIDL and List must both try enumeration)", got, want)
	}
}

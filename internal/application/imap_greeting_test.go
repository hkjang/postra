package application

import (
	"bufio"
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"postra/internal/adapters/imap"
	"postra/internal/domain"
)

// inFrameServer answers tagged IMAP commands correctly whatever greeting it
// opened with, and reports an empty INBOX. A peer on the wrong port does not
// normally play along like this, but a fixture that does is what makes this
// test about the greeting alone: the greeting line is the only thing that
// differs between the two sub-tests below, so nothing else can explain the
// difference in what the operator is told.
func inFrameServer(t *testing.T, greeting string) (host string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// A sync retries, so every attempt gets its own handler.
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
				br := bufio.NewReader(conn)
				io.WriteString(conn, greeting)
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					sp := strings.SplitN(strings.TrimRight(line, "\r\n"), " ", 3)
					if len(sp) < 2 {
						continue
					}
					tag, cmd := sp[0], strings.ToUpper(sp[1])
					switch cmd {
					case "SELECT":
						io.WriteString(conn, "* 0 EXISTS\r\n* OK [UIDVALIDITY 42] ok\r\n")
						io.WriteString(conn, tag+" OK [READ-WRITE] SELECT completed\r\n")
					default:
						io.WriteString(conn, tag+" OK\r\n")
					}
				}
			}()
		}
	}()
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	n, _ := strconv.Atoi(p)
	return h, n
}

// imapAccountAt registers an imap account pointed at one of these fixtures,
// through the production dialer.
func imapAccountAt(t *testing.T, host string, port int) (*App, *domain.MailAccount) {
	t.Helper()
	app, _, _, _ := newTestApp(t)
	app.IMAP = imap.Dialer{}
	ctx := WithActor(context.Background(), "test")
	ref, err := app.RegisterSecret(ctx, domain.SecretMailPassword, "t", domain.NewSecretHandle([]byte("pw")))
	if err != nil {
		t.Fatal(err)
	}
	acc, err := app.CreateAccount(ctx, CreateAccountInput{
		Name: "IMAP", Email: "me@corp.local", InboundProtocol: "imap",
		POP3Host: host, POP3Port: port, POP3Security: "none", POP3Username: "me", POP3SecretRef: string(ref),
		SMTPHost: "127.0.0.1", SMTPSecurity: "none", SMTPAuth: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	return app, acc
}

// TestSyncOfANonIMAPServerFailsAtTheGreeting is the adapter's greeting check
// seen from where the operator reads it. An inbound port left at the POP3
// default is not a mail sync that needs looking at later: it is a
// misconfiguration, and the sync used to report it as a success with an empty
// mailbox — the one outcome nobody investigates. The two sub-tests share the
// fixture and differ only in its opening line.
func TestSyncOfANonIMAPServerFailsAtTheGreeting(t *testing.T) {
	t.Run("POP3 greeting", func(t *testing.T) {
		fastRetries(t)
		host, port := inFrameServer(t, "+OK POP3 ready <1234@maildrop>\r\n")
		app, acc := imapAccountAt(t, host, port)
		job := syncAndWait(t, app, acc.ID)
		if job.Status != domain.JobFailed {
			t.Fatalf("sync status = %s, want failed — a server that never greeted as IMAP was synced as if empty", job.Status)
		}
		d := job.Diagnostic
		if d == nil || d.Stage != domain.StageGreeting {
			t.Fatalf("diagnostic = %+v, want stage %q", d, domain.StageGreeting)
		}
		if syncClassLabels[d.Class] == "" {
			t.Fatalf("class %q has no operator label, so the read boundary drops it: %+v", d.Class, d)
		}
		if !strings.Contains(d.Summary, syncStageLabels[domain.StageGreeting]) {
			t.Fatalf("summary %q does not name the greeting step", d.Summary)
		}
		// Naming the step is not naming the cause: "알 수 없는 오류" sends the
		// operator looking at the network when the port is simply wrong. The
		// summary has to end on what to check.
		if !strings.Contains(d.Summary, greetingNotIMAPHint) {
			t.Fatalf("summary %q carries no hint about the first response not being an IMAP greeting", d.Summary)
		}
		// The greeting is the server's own text and diagnostics are persisted
		// and shown, so none of it may travel.
		for _, leak := range []string{"POP3 ready", "1234@maildrop"} {
			if strings.Contains(d.Summary, leak) || strings.Contains(job.Error, leak) {
				t.Fatalf("the server's own text reached a screen (%q): %q / %q", leak, d.Summary, job.Error)
			}
		}
	})

	t.Run("IMAP greeting", func(t *testing.T) {
		// Same fixture, same empty INBOX, one line different. Without this the
		// sub-test above would also pass against a fixture that never worked.
		host, port := inFrameServer(t, "* OK IMAP4rev1 ready\r\n")
		app, acc := imapAccountAt(t, host, port)
		job := syncAndWait(t, app, acc.ID)
		if job.Status != domain.JobSucceeded {
			t.Fatalf("sync status = %s (%s), want succeeded on a conforming greeting", job.Status, job.Error)
		}
	})
}

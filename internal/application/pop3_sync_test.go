package application

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"postra/internal/adapters/pop3"
	"postra/internal/domain"
)

// syncPOP3Server lies about LIST size, so only the actual RETR reader can
// enforce the cap. The client port is always the production POP3 adapter.
func syncPOP3Server(t *testing.T, raw string, stall, noUIDL bool) (int, *atomic.Int32, <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	closed := make(chan error, 4)
	retrieves := new(atomic.Int32)
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				wire := textproto.NewConn(conn)
				_ = wire.PrintfLine("+OK ready")
				for {
					cmd, err := wire.ReadLine()
					if err != nil {
						closed <- err
						return
					}
					switch cmd {
					case "UIDL":
						if noUIDL {
							_ = wire.PrintfLine("-ERR unsupported")
						} else {
							_, _ = io.WriteString(conn, "+OK\r\n1 stable-id\r\n.\r\n")
						}
					case "LIST":
						_, _ = io.WriteString(conn, "+OK\r\n1 1\r\n.\r\n")
					case "RETR 1":
						retrieves.Add(1)
						body := strings.ReplaceAll(raw, "\r\n.", "\r\n..")
						if strings.HasPrefix(body, ".") {
							body = "." + body
						}
						if !stall {
							body += ".\r\n"
						}
						if _, err := io.WriteString(conn, "+OK\r\n"+body); err != nil {
							closed <- err
							return
						}
						if stall {
							_, err := wire.ReadLine()
							closed <- err
							return
						}
					case "QUIT":
						_ = wire.PrintfLine("+OK bye")
						return
					default:
						_ = wire.PrintfLine("-ERR unexpected command")
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		select {
		case <-done:
		case <-time.After(6 * time.Second):
			t.Error("POP3 server did not finish")
		}
	})
	return ln.Addr().(*net.TCPAddr).Port, retrieves, closed
}

func pop3SyncAccount(t *testing.T, app *App, port int) *domain.MailAccount {
	t.Helper()
	app.POP3 = pop3.Dialer{}
	app.Cfg.Sync.MaxMessageBytes = 512
	app.Cfg.Sync.CommandTimeoutSec = 3
	acc, err := app.CreateAccount(WithActor(context.Background(), "test"), CreateAccountInput{
		Name: "TCP POP3", Email: "me@corp.local", POP3Host: "127.0.0.1", POP3Port: port, POP3Security: "none",
		SMTPHost: "127.0.0.1", SMTPSecurity: "none", SMTPAuth: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	return acc
}

func TestPOP3SyncRejectsOversize(t *testing.T) {
	for _, stall := range []bool{false, true} {
		t.Run(fmt.Sprintf("stall=%v", stall), func(t *testing.T) {
			app, _, _, _ := newTestApp(t)
			raw := testMail("oversize", "oversize-marker", strings.Repeat("x", 600))
			if stall {
				raw = strings.TrimSuffix(raw, "\r\n")
			}
			port, retrieves, closed := syncPOP3Server(t, raw, stall, false)
			acc := pop3SyncAccount(t, app, port)
			start := time.Now()
			job := syncAndWait(t, app, acc.ID)
			if time.Since(start) >= 2*time.Second {
				t.Fatal("sync waited for command timeout")
			}
			if job.Status != domain.JobSucceeded || job.Stats["failed"] != 1 || job.Stats["new"] != 0 || job.Stats["oversize"] != 0 {
				t.Fatalf("unexpected job: %+v", job)
			}
			if retrieves.Load() != 1 {
				t.Fatalf("RETR count=%d", retrieves.Load())
			}
			res, err := app.Search(context.Background(), domain.SearchQuery{})
			if err != nil || len(res.Messages) != 0 {
				t.Fatalf("oversize mail stored: %+v, %v", res, err)
			}
			checkpoint, err := app.Store.HasCheckpoint(context.Background(), acc.ID, "stable-id")
			if err != nil || checkpoint {
				t.Fatalf("failed message checkpointed: %v, %v", checkpoint, err)
			}
			select {
			case err := <-closed:
				if err != io.EOF {
					t.Fatalf("server did not observe close: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("connection remained open")
			}
		})
	}
}

func TestPOP3SyncPreservesRawAndDeduplicates(t *testing.T) {
	for _, noUIDL := range []bool{false, true} {
		t.Run(fmt.Sprintf("noUIDL=%v", noUIDL), func(t *testing.T) {
			app, _, _, _ := newTestApp(t)
			raw := testMail("normal", "TCP normal", ".first\r\n\r\n..last")
			port, retrieves, _ := syncPOP3Server(t, raw, false, noUIDL)
			acc := pop3SyncAccount(t, app, port)
			job := syncAndWait(t, app, acc.ID)
			if job.Stats["new"] != 1 || job.Stats["failed"] != 0 {
				t.Fatalf("first sync: %+v", job)
			}
			job = syncAndWait(t, app, acc.ID)
			if job.Stats["new"] != 0 || job.Stats["duplicate"] != 1 || job.Stats["failed"] != 0 {
				t.Fatalf("resync: %+v", job)
			}
			wantRetrieves := int32(1)
			if noUIDL {
				wantRetrieves = 2
			}
			if retrieves.Load() != wantRetrieves {
				t.Fatalf("RETR count=%d want %d", retrieves.Load(), wantRetrieves)
			}
			res, err := app.Search(context.Background(), domain.SearchQuery{})
			if err != nil || len(res.Messages) != 1 {
				t.Fatalf("stored messages: %+v, %v", res, err)
			}
			if res.Messages[0].RawHash != fmt.Sprintf("%x", sha256.Sum256([]byte(raw))) {
				t.Fatal("raw hash changed")
			}
			rc, err := app.GetRawMessage(WithActor(context.Background(), "test"), res.Messages[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil || string(got) != raw {
				t.Fatalf("stored raw changed: %q, %v", got, err)
			}
		})
	}
}

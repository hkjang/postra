package application

import (
	"context"
	"fmt"
	"math"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"testing"
	"time"

	"postra/internal/adapters/pop3"
	"postra/internal/domain"
)

// scriptedMaildrop serves a fixed maildrop over a real loopback TCP socket for
// the production pop3.Dialer: no hand-written session double, so the limit the
// sync layer applies to the body stream is the one under test.
// refuseList makes the fixture answer LIST with an error while still answering
// UIDL. runSync merges LIST sizes only to pre-screen oversize messages and
// carries on past a LIST failure (sync.go warns), so every message arrives with Size 0
// and ingestOne's own read limit is the only thing bounding the body.
func scriptedMaildrop(t *testing.T, mails []string, refuseList bool) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
				wire := textproto.NewConn(conn)
				if wire.PrintfLine("+OK postra scripted maildrop") != nil {
					return
				}
				for {
					line, err := wire.ReadLine()
					if err != nil {
						return
					}
					verb, rest, _ := strings.Cut(line, " ")
					switch strings.ToUpper(verb) {
					case "USER", "PASS", "NOOP":
						_ = wire.PrintfLine("+OK")
					case "UIDL":
						_ = wire.PrintfLine("+OK")
						for i := range mails {
							_ = wire.PrintfLine("%d u%d", i+1, i+1)
						}
						_ = wire.PrintfLine(".")
					case "LIST":
						if refuseList {
							_ = wire.PrintfLine("-ERR LIST not available")
							continue
						}
						_ = wire.PrintfLine("+OK")
						for i, m := range mails {
							_ = wire.PrintfLine("%d %d", i+1, len(m))
						}
						_ = wire.PrintfLine(".")
					case "RETR":
						n, cerr := strconv.Atoi(strings.TrimSpace(rest))
						if cerr != nil || n < 1 || n > len(mails) {
							_ = wire.PrintfLine("-ERR no such message")
							continue
						}
						_ = wire.PrintfLine("+OK %d octets", len(mails[n-1]))
						// Dot-stuffing is not needed: the fixtures carry no
						// line beginning with a period.
						if _, werr := wire.W.WriteString(mails[n-1]); werr != nil {
							return
						}
						if wire.PrintfLine(".") != nil {
							return
						}
					case "QUIT":
						_ = wire.PrintfLine("+OK bye")
						return
					default:
						_ = wire.PrintfLine("-ERR unsupported")
					}
				}
			}()
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port
}

func maildropAccount(t *testing.T, app *App, port int) *domain.MailAccount {
	t.Helper()
	ctx := WithActor(context.Background(), "test")
	secret := domain.NewSecretHandle([]byte("pop3-password"))
	ref, err := app.RegisterSecret(ctx, domain.SecretMailPassword, "test", secret)
	if err != nil {
		t.Fatal(err)
	}
	acc, err := app.CreateAccount(ctx, CreateAccountInput{
		Name: "Unlimited", Email: "me@corp.local",
		POP3Host: "127.0.0.1", POP3Port: port, POP3Security: "none",
		POP3Username: "me", POP3SecretRef: string(ref),
		SMTPHost: "127.0.0.1", SMTPSecurity: "none", SMTPAuth: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	return acc
}

// Sync.MaxMessageBytes <= 0 turns the per-message size limit off — the
// convention the setting carries everywhere else, including the IMAP adapter's
// literal cap (maxLiteral <= 0) and the oversize checks in runSync and
// ingestOne, both of which only compare when maxBytes > 0. An account
// configured that way must still ingest whole messages.
func TestSyncUnlimitedMessageSizeStoresWholeMessage(t *testing.T) {
	for _, maxBytes := range []int64{0, -1} {
		t.Run(fmt.Sprintf("max_message_bytes=%d", maxBytes), func(t *testing.T) {
			first := testMail("unbounded-1", "first", strings.Repeat("alpha ", 500))
			second := testMail("unbounded-2", "second", strings.Repeat("beta ", 400))
			port := scriptedMaildrop(t, []string{first, second}, false)

			app, _, _, _ := newTestApp(t)
			app.POP3 = pop3.Dialer{} // the production adapter, not the test fake
			app.Cfg.Sync.MaxMessageBytes = maxBytes
			acc := maildropAccount(t, app, port)

			job := syncAndWait(t, app, acc.ID)
			if job.Stats["new"] != 2 {
				t.Fatalf("new=%d duplicate=%d oversize=%d failed=%d, want new=2 (status=%s err=%s)",
					job.Stats["new"], job.Stats["duplicate"], job.Stats["oversize"],
					job.Stats["failed"], job.Status, job.Error)
			}

			res, err := app.Search(context.Background(), domain.SearchQuery{})
			if err != nil {
				t.Fatal(err)
			}
			sizes := map[string]int64{}
			for _, m := range res.Messages {
				sizes[m.Subject] = m.Size
			}
			if got := sizes["first"]; got != int64(len(first)) {
				t.Errorf("stored size of %q = %d, want the whole message (%d bytes)", "first", got, len(first))
			}
			if got := sizes["second"]; got != int64(len(second)) {
				t.Errorf("stored size of %q = %d, want the whole message (%d bytes)", "second", got, len(second))
			}
		})
	}
}

// The counterpart: a positive Sync.MaxMessageBytes still refuses a message over
// it rather than storing a truncated one. The fixture refuses LIST so the
// pre-fetch size screen in runSync has nothing to screen on and the read limit
// in ingestOne is what has to hold.
func TestSyncEnforcedMessageSizeRefusesOversizeBody(t *testing.T) {
	big := testMail("over-limit", "oversize", strings.Repeat("gamma ", 500))
	small := testMail("under-limit", "fits", "short body")
	const maxBytes = 1000
	if len(big) <= maxBytes || len(small) > maxBytes {
		t.Fatalf("fixture sizes %d/%d do not straddle the %d-byte limit", len(big), len(small), maxBytes)
	}
	port := scriptedMaildrop(t, []string{big, small}, true)

	app, _, _, _ := newTestApp(t)
	app.POP3 = pop3.Dialer{}
	app.Cfg.Sync.MaxMessageBytes = maxBytes
	acc := maildropAccount(t, app, port)

	job := syncAndWait(t, app, acc.ID)
	if job.Stats["new"] != 1 || job.Stats["oversize"] != 1 {
		t.Fatalf("new=%d oversize=%d, want 1/1 (status=%s err=%s)",
			job.Stats["new"], job.Stats["oversize"], job.Status, job.Error)
	}
	res, err := app.Search(context.Background(), domain.SearchQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 1 || res.Messages[0].Subject != "fits" {
		t.Fatalf("stored %d messages (%+v), want only the one under the limit", len(res.Messages), res.Messages)
	}
	if got := res.Messages[0].Size; got != int64(len(small)) {
		t.Errorf("stored size = %d, want the whole message (%d bytes)", got, len(small))
	}
}

// Both body readers (ingestOne and the repair path's fetchRaw) share this
// count, including the top of the range, which no maildrop fixture can reach.
func TestRawReadLimit(t *testing.T) {
	for _, tc := range []struct{ maxBytes, want int64 }{
		{0, math.MaxInt64},             // limit off
		{-1, math.MaxInt64},            // limit off
		{math.MinInt64, math.MaxInt64}, // limit off
		{1, 2},                         // +1 to tell at-limit from over
		{50 << 20, (50 << 20) + 1},     // the shipped default
		{math.MaxInt64 - 1, math.MaxInt64},
		{math.MaxInt64, math.MaxInt64}, // +1 would overflow to negative
	} {
		if got := rawReadLimit(tc.maxBytes); got != tc.want {
			t.Errorf("rawReadLimit(%d) = %d, want %d", tc.maxBytes, got, tc.want)
		}
	}
}

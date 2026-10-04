package application

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"postra/internal/adapters/pop3"
	"postra/internal/domain"
)

// lockedBuffer collects log records the sync worker goroutine writes while the
// test goroutine reads them, so the capture itself is not the race.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A server that answers UIDL but refuses LIST leaves every message with Size 0,
// which silently disables the pre-fetch oversize screen in runSync: the sync
// keeps working (ingestOne still measures the body it read) but an operator who
// configured a size limit sees large messages being downloaded anyway with no
// trace of why. The sync must stay a success and keep collecting mail, and the
// lost screen must be stated once in the log — and only when LIST actually
// failed.
func TestSyncListFailureWarnsPreFetchSizeScreenOff(t *testing.T) {
	const screenWarning = "pre-fetch size screen"
	for _, tc := range []struct {
		name       string
		refuseList bool
		wantWarn   bool
	}{
		{name: "LIST refused", refuseList: true, wantWarn: true},
		{name: "LIST answered", refuseList: false, wantWarn: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := testMail("sizes-1", "first", "body one")
			second := testMail("sizes-2", "second", "body two")
			port := scriptedMaildrop(t, []string{first, second}, tc.refuseList)

			app, _, _, _ := newTestApp(t)
			app.POP3 = pop3.Dialer{} // the production adapter, not the test fake
			// A limit large enough for both fixtures: whether or not the
			// pre-fetch screen runs, nothing here is oversize, so the only
			// difference between the two cases is the diagnostic.
			app.Cfg.Sync.MaxMessageBytes = 1 << 20
			acc := maildropAccount(t, app, port)

			logs := &lockedBuffer{}
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(logs, nil)))
			defer slog.SetDefault(previous)

			job := syncAndWait(t, app, acc.ID)
			if job.Status != domain.JobSucceeded {
				t.Fatalf("status=%s err=%s, want the sync to succeed", job.Status, job.Error)
			}
			if job.Stats["new"] != 2 {
				t.Fatalf("new=%d duplicate=%d oversize=%d failed=%d, want new=2",
					job.Stats["new"], job.Stats["duplicate"], job.Stats["oversize"], job.Stats["failed"])
			}
			res, err := app.Search(context.Background(), domain.SearchQuery{})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Messages) != 2 {
				t.Fatalf("stored %d messages, want both of them", len(res.Messages))
			}

			out := logs.String()
			if got := strings.Contains(out, screenWarning); got != tc.wantWarn {
				t.Fatalf("log mentions %q = %v, want %v\nlog: %s", screenWarning, got, tc.wantWarn, out)
			}
			// The fixed diagnostic sentence drops the classification with the
			// server text, so the warning carries the label separately.
			if tc.wantWarn && !strings.Contains(out, `"class":"rejected"`) {
				t.Errorf("warning did not classify the refusal\nlog: %s", out)
			}
			// The adapter's error text carries the server's own reply
			// ("-ERR LIST not available"); diagnostics in this repo never do.
			if strings.Contains(out, "LIST not available") {
				t.Errorf("log leaked the mail server's reply\nlog: %s", out)
			}
		})
	}
}

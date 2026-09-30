package application

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"postra/internal/domain"
)

// countingIMAP is an IMAP server that records how many sessions are open at
// once. Both the IDLE watch and the syncs it triggers dial it, so the peak is
// what a real server's per-account connection limit would see.
type countingIMAP struct {
	mu       sync.Mutex
	open     int
	peak     int
	dials    int
	idles    int
	messages map[string]string
	exists   int
	tick     time.Duration // how long Idle waits before reporting activity
}

func (d *countingIMAP) Dial(context.Context, domain.POP3DialOptions) (domain.POP3Session, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dials++
	d.open++
	if d.open > d.peak {
		d.peak = d.open
	}
	return &countingSession{d: d}, nil
}

func (d *countingIMAP) snapshot() (peak, dials, idles, open int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.peak, d.dials, d.idles, d.open
}

func (d *countingIMAP) grow(count int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.exists = count
}

type countingSession struct {
	d      *countingIMAP
	closed bool
}

func (s *countingSession) uidls() []string {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	out := []string{}
	for u := range s.d.messages {
		out = append(out, u)
	}
	return out
}
func (s *countingSession) UIDL(context.Context) ([]domain.RemoteMessage, error) {
	out := []domain.RemoteMessage{}
	for i, u := range s.uidls() {
		out = append(out, domain.RemoteMessage{Number: i + 1, UIDL: u, Size: 10})
	}
	return out, nil
}
func (s *countingSession) List(ctx context.Context) ([]domain.RemoteMessage, error) {
	return s.UIDL(ctx)
}
func (s *countingSession) Retrieve(_ context.Context, n int) (io.ReadCloser, error) {
	u := s.uidls()[n-1]
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	return io.NopCloser(strings.NewReader(s.d.messages[u])), nil
}
func (s *countingSession) Top(ctx context.Context, n, _ int) (io.ReadCloser, error) {
	return s.Retrieve(ctx, n)
}
func (s *countingSession) Delete(context.Context, int) error { return nil }
func (s *countingSession) Quit(context.Context) error        { return nil }
func (s *countingSession) Close() error {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	if !s.closed {
		s.closed = true
		s.d.open--
	}
	return nil
}
func (s *countingSession) MessageCount() int {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	return s.d.exists
}
func (s *countingSession) Idle(ctx context.Context) error {
	s.d.mu.Lock()
	s.d.idles++
	tick := s.d.tick
	s.d.mu.Unlock()
	if tick == 0 {
		<-ctx.Done()
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(tick):
		return nil
	}
}

func countingIdleApp(t *testing.T, server *countingIMAP) (*App, *domain.MailAccount) {
	t.Helper()
	app, _, _, _ := newTestApp(t)
	app.IMAP = server
	acc := imapAccount(t, app)
	original := idleReconcileInterval
	idleReconcileInterval = 50 * time.Millisecond
	t.Cleanup(func() { idleReconcileInterval = original })
	app.setLeaderState(true)
	return app, acc
}

func syncJobs(t *testing.T, app *App) int {
	t.Helper()
	jobs, err := app.Store.ListJobs(context.Background(), DefaultUserID, 200)
	if err != nil {
		t.Fatal(err)
	}
	return len(jobs)
}

// A mail server counts connections, and many allow one or two per account: the
// next one is accepted at TCP level and never greeted, which is what a failing
// sync reported. The watch must therefore hand its connection to the sync it
// triggers instead of holding a second one.
func TestIdleWatchNeverHoldsASecondConnectionWhileSyncing(t *testing.T) {
	server := &countingIMAP{messages: map[string]string{"1.1": testMail("m1", "제목", "본문")}, tick: 5 * time.Millisecond}
	app, _ := countingIdleApp(t, server)
	runIdleWorker(t, app)
	// Wait for the watch to have cycled several times: connect → sync → idle →
	// activity → sync → reconnect.
	eventually(t, "several idle cycles", func() bool {
		_, dials, idles, _ := server.snapshot()
		return dials >= 4 && idles >= 2
	})
	peak, dials, _, _ := server.snapshot()
	if peak != 1 {
		t.Fatalf("this account held %d connections at once across %d dials; a per-account limit of 1 would refuse the extra one", peak, dials)
	}
}

// Yielding must not lose mail that arrives while the connection is away, and
// must not turn the reconnection into a sync loop of its own.
func TestIdleWatchSyncsOnReconnectOnlyWhenTheMailboxGrew(t *testing.T) {
	server := &countingIMAP{messages: map[string]string{"1.1": testMail("m1", "제목", "본문")}}
	app, _ := countingIdleApp(t, server)
	runIdleWorker(t, app)
	// First connection always syncs: it is catching up on whatever arrived
	// while no watch was listening.
	eventually(t, "the first catch-up sync", func() bool { return syncJobs(t, app) >= 1 })
	eventually(t, "the watch idling after it", func() bool { _, _, idles, _ := server.snapshot(); return idles >= 1 })
	jobs := syncJobs(t, app)
	// Idling with an unchanged mailbox must not keep starting syncs.
	time.Sleep(300 * time.Millisecond)
	if got := syncJobs(t, app); got != jobs {
		t.Fatalf("an idle watch started %d more syncs with nothing new", got-jobs)
	}
	if peak, _, _, _ := server.snapshot(); peak != 1 {
		t.Fatalf("peak connections = %d", peak)
	}
}

// The watch is the second connection an account needs, so an operator whose
// server allows only one must be able to switch it off.
func TestIdleWatchCanBeTurnedOff(t *testing.T) {
	server := &countingIMAP{messages: map[string]string{}}
	app, _ := countingIdleApp(t, server)
	runIdleWorker(t, app)
	eventually(t, "the watch to connect", func() bool { _, dials, _, _ := server.snapshot(); return dials >= 1 })
	if _, err := app.AdminPatchSettings(settingsAdmin(), SettingsPatch{Values: map[string]string{SettingMailIdleEnabled: "false"}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "every watch to be dropped", func() bool { _, _, _, open := server.snapshot(); return open == 0 })
	_, before, _, _ := server.snapshot()
	time.Sleep(300 * time.Millisecond)
	if _, after, _, _ := server.snapshot(); after != before {
		t.Fatalf("the watch reconnected %d times after IDLE was turned off", after-before)
	}
}

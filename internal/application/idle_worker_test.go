package application

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"postra/internal/domain"
)

// fakeIMAP counts IDLE sessions — a session counts once it enters Idle, so the
// syncs IDLE triggers (which dial too) are not mistaken for watches.
type fakeIMAP struct {
	pop *fakePOP3

	mu        sync.Mutex
	started   int           // sessions that entered Idle
	cancelled int           // sessions whose Idle ended by cancellation
	panics    int           // first N IDLE sessions panic on entry
	tick      time.Duration // when > 0, Idle returns nil this often (a re-idle)
	hosts     []string      // host each IDLE session dialed
}

type fakeIMAPSession struct {
	*fakePOP3Session
	d     *fakeIMAP
	host  string
	begun bool
}

func (d *fakeIMAP) Dial(_ context.Context, opts domain.POP3DialOptions) (domain.POP3Session, error) {
	return &fakeIMAPSession{fakePOP3Session: &fakePOP3Session{d: d.pop}, d: d, host: opts.Host}, nil
}

func (s *fakeIMAPSession) Idle(ctx context.Context) error {
	s.d.mu.Lock()
	if !s.begun {
		s.begun = true
		s.d.started++
		s.d.hosts = append(s.d.hosts, s.host)
		if s.d.panics > 0 {
			s.d.panics--
			s.d.mu.Unlock()
			panic("simulated IMAP client fault")
		}
	}
	tick := s.d.tick
	s.d.mu.Unlock()
	var wake <-chan time.Time
	if tick > 0 {
		wake = time.After(tick)
	}
	select {
	case <-ctx.Done():
		s.d.mu.Lock()
		s.d.cancelled++
		s.d.mu.Unlock()
		return ctx.Err()
	case <-wake:
		return nil
	}
}

func (d *fakeIMAP) snapshot() (started, cancelled int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.started, d.cancelled
}

// flakyStore fails the listing the IDLE supervisor reads, or the lease the
// election renews, on demand.
type flakyStore struct {
	Storage
	failList  atomic.Bool
	leaseErr  atomic.Pointer[error]
	leaseHang atomic.Bool
}

func (s *flakyStore) ListUsers(ctx context.Context) ([]domain.User, error) {
	if s.failList.Load() {
		return nil, errors.New("database is locked")
	}
	return s.Storage.ListUsers(ctx)
}

func (s *flakyStore) TryAcquireLease(ctx context.Context, key, nodeID string, sec int) (bool, error) {
	if s.leaseHang.Load() {
		<-ctx.Done()
		return false, ctx.Err()
	}
	if err := s.leaseErr.Load(); err != nil {
		return false, *err
	}
	return s.Storage.TryAcquireLease(ctx, key, nodeID, sec)
}

func imapAccount(t *testing.T, app *App) *domain.MailAccount {
	t.Helper()
	ctx := WithActor(context.Background(), "test")
	ref, err := app.RegisterSecret(ctx, domain.SecretMailPassword, "imap", domain.NewSecretHandle([]byte("imap-password")))
	if err != nil {
		t.Fatal(err)
	}
	acc, err := app.CreateAccount(ctx, CreateAccountInput{
		Name: "IMAP", Email: "me@corp.local", InboundProtocol: domain.InboundIMAP,
		POP3Host: "127.0.0.1", POP3Security: "none", POP3Username: "me", POP3SecretRef: string(ref),
		SMTPHost: "127.0.0.1", SMTPSecurity: "none", SMTPAuth: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	return acc
}

func idleTestApp(t *testing.T, interval time.Duration) (*App, *fakeIMAP, *flakyStore, *domain.MailAccount) {
	t.Helper()
	app, pop, _, _ := newTestApp(t)
	imap := &fakeIMAP{pop: pop}
	app.IMAP = imap
	store := &flakyStore{Storage: app.Store}
	app.Store = store
	acc := imapAccount(t, app)
	old := idleReconcileInterval
	idleReconcileInterval = interval
	t.Cleanup(func() { idleReconcileInterval = old })
	return app, imap, store, acc
}

func runIdleWorker(t *testing.T, app *App) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); app.RunIdleWorker(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// A panicking watch used to stay registered as "running" with no goroutine
// behind it, so the account was never watched again.
func TestIdleWorkerRestartsAWatchThatPanicked(t *testing.T) {
	app, imap, _, _ := idleTestApp(t, 50*time.Millisecond)
	imap.panics = 1
	app.setLeaderState(true)
	runIdleWorker(t, app)
	eventually(t, "a second IDLE session after the first one panicked", func() bool {
		started, _ := imap.snapshot()
		return started >= 2
	})
}

// A standby blip shorter than the reconcile interval ended the loop without
// the supervisor ever seeing it. The interval here is effectively never, so
// only the leadership edge can bring the watch back.
func TestIdleWorkerResumesAfterALeadershipBlip(t *testing.T) {
	app, imap, _, _ := idleTestApp(t, time.Hour)
	imap.tick = 5 * time.Millisecond // the loop re-checks leadership between IDLEs
	app.setLeaderState(true)
	runIdleWorker(t, app)
	eventually(t, "the first IDLE session", func() bool { started, _ := imap.snapshot(); return started == 1 })

	app.setLeaderState(false)
	time.Sleep(30 * time.Millisecond)
	app.setLeaderState(true)
	eventually(t, "a new IDLE session once leadership came back", func() bool {
		started, _ := imap.snapshot()
		return started >= 2
	})
}

// Losing leadership must tear the watches down at once, not at the next tick.
func TestIdleWorkerStopsWatchingOnStandbyEdge(t *testing.T) {
	app, imap, _, _ := idleTestApp(t, time.Hour)
	app.setLeaderState(true)
	runIdleWorker(t, app)
	eventually(t, "the first IDLE session", func() bool { started, _ := imap.snapshot(); return started == 1 })
	app.setLeaderState(false)
	eventually(t, "the IDLE session cancelled on standby", func() bool { _, cancelled := imap.snapshot(); return cancelled == 1 })
}

// An unreadable account list is not an empty one. A transient DB error used
// to stop every IDLE connection in the process.
func TestIdleWorkerKeepsWatchesWhenTheAccountListFails(t *testing.T) {
	app, imap, store, _ := idleTestApp(t, 20*time.Millisecond)
	app.setLeaderState(true)
	runIdleWorker(t, app)
	eventually(t, "the first IDLE session", func() bool { started, _ := imap.snapshot(); return started == 1 })

	store.failList.Store(true)
	time.Sleep(200 * time.Millisecond) // ~10 reconciles against a failing store
	if started, cancelled := imap.snapshot(); cancelled != 0 || started != 1 {
		t.Fatalf("a failed listing disturbed the running watch: started=%d cancelled=%d", started, cancelled)
	}
	store.failList.Store(false)
}

// The loop holds the account as it was at start; a host change must reconnect.
func TestIdleWorkerReconnectsWhenConnectionSettingsChange(t *testing.T) {
	app, imap, _, acc := idleTestApp(t, 20*time.Millisecond)
	app.setLeaderState(true)
	runIdleWorker(t, app)
	eventually(t, "the first IDLE session", func() bool { started, _ := imap.snapshot(); return started == 1 })

	host := "localhost"
	if _, err := app.UpdateAccount(WithActor(context.Background(), "test"), UpdateAccountInput{AccountID: acc.ID, POP3Host: &host}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a new IDLE session on the new host", func() bool {
		imap.mu.Lock()
		defer imap.mu.Unlock()
		return len(imap.hosts) >= 2 && imap.hosts[len(imap.hosts)-1] == "localhost" && imap.cancelled >= 1
	})
}

// A status-only change (sync marking the account) must not reconnect.
func TestIdleWorkerIgnoresStatusOnlyChanges(t *testing.T) {
	if idleSettings(domain.MailAccount{POP3Host: "h", Status: domain.AccountActive, UpdatedAt: 1}) !=
		idleSettings(domain.MailAccount{POP3Host: "h", Status: domain.AccountActive, UpdatedAt: 2}) {
		t.Fatal("updated_at alone changed the connection fingerprint")
	}
}

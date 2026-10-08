package application

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"postra/internal/adapters/imap"
	"postra/internal/adapters/pop3"
	"postra/internal/domain"
)

// scriptedInbound fails a scripted number of connections, then hands out a
// session. Fetching can be made to fail per message, so a sync that reaches
// the mailbox and still gets nothing can be observed.
type scriptedInbound struct {
	failures  []error // one per connection attempt, then success
	dials     atomic.Int32
	messages  map[string]string
	fetchFail error
	failEvery int // fail every Nth Retrieve (1 = all of them)
	fetched   atomic.Int32
}

func (d *scriptedInbound) Dial(context.Context, domain.POP3DialOptions) (domain.POP3Session, error) {
	n := int(d.dials.Add(1))
	if n <= len(d.failures) {
		return nil, d.failures[n-1]
	}
	return &scriptedSession{d: d}, nil
}

type scriptedSession struct{ d *scriptedInbound }

func (s *scriptedSession) uidls() []string {
	out := []string{}
	for u := range s.d.messages {
		out = append(out, u)
	}
	return out
}
func (s *scriptedSession) UIDL(context.Context) ([]domain.RemoteMessage, error) {
	out := []domain.RemoteMessage{}
	for i, u := range s.uidls() {
		out = append(out, domain.RemoteMessage{Number: i + 1, UIDL: u, Size: int64(len(s.d.messages[u]))})
	}
	return out, nil
}
func (s *scriptedSession) List(ctx context.Context) ([]domain.RemoteMessage, error) {
	return s.UIDL(ctx)
}
func (s *scriptedSession) Retrieve(_ context.Context, n int) (io.ReadCloser, error) {
	if s.d.fetchFail != nil && s.d.failEvery > 0 && int(s.d.fetched.Add(1))%s.d.failEvery == 0 {
		return nil, s.d.fetchFail
	}
	return io.NopCloser(strings.NewReader(s.d.messages[s.uidls()[n-1]])), nil
}
func (s *scriptedSession) Top(ctx context.Context, n, _ int) (io.ReadCloser, error) {
	return s.Retrieve(ctx, n)
}
func (s *scriptedSession) Delete(context.Context, int) error { return nil }
func (s *scriptedSession) Quit(context.Context) error        { return nil }
func (s *scriptedSession) Close() error                      { return nil }

// waitForJob reads a job until the worker has finished with it.
func waitForJob(t *testing.T, app *App, jobID string) *domain.Job {
	t.Helper()
	ctx := WithActor(context.Background(), "test")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		job, err := app.GetJob(ctx, jobID)
		if err != nil {
			t.Fatal(err)
		}
		if job.Status != domain.JobQueued && job.Status != domain.JobRunning {
			return job
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %s did not finish", jobID)
	return nil
}

// fastRetries keeps the retry tests to milliseconds.
func fastRetries(t *testing.T) {
	t.Helper()
	normal, limited := syncDialBackoff, syncDialLimitBackoff
	syncDialBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	syncDialLimitBackoff = []time.Duration{time.Millisecond}
	t.Cleanup(func() { syncDialBackoff, syncDialLimitBackoff = normal, limited })
}

func scriptedApp(t *testing.T, d *scriptedInbound) (*App, *domain.MailAccount) {
	t.Helper()
	app, _, _, _ := newTestApp(t)
	app.POP3 = d
	return app, mustAccount(t, app)
}

func greetingTimeout() error {
	return &domain.InboundError{Stage: domain.StageGreeting, Class: "timeout", Elapsed: 60 * time.Second,
		Timeout: 60 * time.Second, Err: fmt.Errorf("read tcp 10.0.0.9:143: i/o timeout")}
}

// A failed sync must name the step it stopped at, not just report a timeout:
// "the server accepted the connection and never greeted us" is a different
// problem, with a different fix, from a slow mailbox.
func TestSyncFailureNamesTheStepAndKeepsNoServerText(t *testing.T) {
	fastRetries(t)
	app, acc := scriptedApp(t, &scriptedInbound{failures: []error{greetingTimeout(), greetingTimeout(), greetingTimeout()}})
	// A silent server is not hammered: one further attempt, not two.
	job := syncAndWait(t, app, acc.ID)
	if job.Status != domain.JobFailed || job.Error != syncStoppedTimeout {
		t.Fatalf("job = %s %q", job.Status, job.Error)
	}
	d := job.Diagnostic
	if d == nil || d.Stage != domain.StageGreeting || d.Class != "timeout" || d.Attempts != 2 {
		t.Fatalf("diagnostic = %+v", d)
	}
	if d.Protocol != "pop3" || d.Host != "127.0.0.1" || d.ElapsedMS != 60000 || d.TimeoutMS != 60000 {
		t.Fatalf("diagnostic target/timings = %+v", d)
	}
	// The connection this sync holds is counted, so an operator can tell a
	// per-account connection limit from an outage.
	if d.HostSessions < 1 || d.AccountSessions < 1 {
		t.Fatalf("sessions not counted: %+v", d)
	}
	for _, want := range []string{"서버 환영 메시지 수신", "제한 시간 안에 응답이 없었습니다", "연결 시도 2회", "동시 접속 제한"} {
		if !strings.Contains(d.Summary, want) {
			t.Fatalf("summary %q lacks %q", d.Summary, want)
		}
	}
	if strings.Contains(d.Summary, "i/o timeout") || strings.Contains(d.Summary, "10.0.0.9") || strings.Contains(job.Error, "i/o timeout") {
		t.Fatalf("the server's own text reached a screen: %q / %q", d.Summary, job.Error)
	}
	// A failed sync is the most serious thing this service reports, so it
	// files an incident carrying the same rendering. The job row is written
	// first, so the incident is looked for rather than assumed present.
	deadline := time.Now().Add(5 * time.Second)
	for {
		incidents, err := app.AdminListIncidents(settingsAdmin(), domain.IncidentFilter{})
		if err != nil {
			t.Fatal(err)
		}
		for _, inc := range incidents {
			if inc.Component == "sync" && inc.Severity == domain.SeverityError && inc.Detail == d.Summary {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no sync incident carried the diagnostic: %+v", incidents)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A server that is briefly busy, throttling, or closing sessions at its
// connection limit used to fail the whole sync on the first attempt.
func TestSyncRetriesATransientConnectionAndRecordsThatItDid(t *testing.T) {
	fastRetries(t)
	reset := &domain.InboundError{Stage: domain.StageTCPConnect, Class: "reset", Err: fmt.Errorf("read: connection reset by peer")}
	d := &scriptedInbound{failures: []error{reset}, messages: map[string]string{"u1": testMail("m1", "제목", "본문")}}
	app, acc := scriptedApp(t, d)
	job := syncAndWait(t, app, acc.ID)
	if job.Status != domain.JobSucceeded || job.Stats["new"] != 1 {
		t.Fatalf("job = %s %q stats=%v", job.Status, job.Error, job.Stats)
	}
	if d.dials.Load() != 2 {
		t.Fatalf("connection attempts = %d", d.dials.Load())
	}
	diag := job.Diagnostic
	// The failure is history, not the outcome: no stage or class, but the
	// retry and its reason stay visible.
	if diag == nil || diag.Attempts != 2 || diag.Recovered != "reset" || diag.Stage != "" || diag.Class != "" {
		t.Fatalf("diagnostic = %+v", diag)
	}
	if !strings.Contains(diag.Summary, "연결 시도 2회") || !strings.Contains(diag.Summary, "재연결 후 진행") {
		t.Fatalf("summary = %q", diag.Summary)
	}
}

// A refused password is not retried and still parks the account; a server that
// refuses for load must not be mistaken for one.
func TestSyncSeparatesARefusedPasswordFromATemporaryRefusal(t *testing.T) {
	fastRetries(t)
	t.Run("rejected credentials", func(t *testing.T) {
		d := &scriptedInbound{failures: []error{&domain.AuthError{Err: &domain.InboundRejected{Code: "AUTHENTICATIONFAILED"}}}}
		app, acc := scriptedApp(t, d)
		job := syncAndWait(t, app, acc.ID)
		if job.Status != domain.JobFailed || job.Error != providerAuthFailed || d.dials.Load() != 1 {
			t.Fatalf("job = %s %q after %d attempts", job.Status, job.Error, d.dials.Load())
		}
		// The code that made this a credential failure is on the record.
		if job.Diagnostic == nil || job.Diagnostic.Stage != domain.StageLogin || job.Diagnostic.Code != "AUTHENTICATIONFAILED" {
			t.Fatalf("diagnostic = %+v", job.Diagnostic)
		}
		stored, err := app.GetAccount(WithActor(context.Background(), "test"), acc.ID)
		if err != nil || stored.Status != domain.AccountCredentialError {
			t.Fatalf("account = %v %v", stored.Status, err)
		}
	})
	t.Run("temporary refusal", func(t *testing.T) {
		busy := &domain.InboundError{Stage: domain.StageLogin, Command: "PASS", Class: "rejected", Code: "IN-USE",
			Err: fmt.Errorf("-ERR [IN-USE] mailbox locked by another session")}
		d := &scriptedInbound{failures: []error{busy}, messages: map[string]string{"u1": testMail("m1", "제목", "본문")}}
		app, acc := scriptedApp(t, d)
		job := syncAndWait(t, app, acc.ID)
		if job.Status != domain.JobSucceeded || d.dials.Load() != 2 {
			t.Fatalf("a busy mailbox was not retried: %s after %d attempts", job.Status, d.dials.Load())
		}
		stored, _ := app.GetAccount(WithActor(context.Background(), "test"), acc.ID)
		if stored.Status != domain.AccountActive {
			t.Fatalf("a busy mailbox parked the account as %s", stored.Status)
		}
	})
}

// A sync that reached the mailbox and failed to fetch anything is not a
// success: reporting it as one hides the outage an operator has to act on.
func TestSyncWithFailedFetchesIsNotReportedAsSuccess(t *testing.T) {
	fetchTimeout := &domain.InboundError{Stage: domain.StageFetch, Command: "RETR", Class: "timeout",
		Elapsed: 60 * time.Second, Timeout: 60 * time.Second, Err: fmt.Errorf("i/o timeout")}
	t.Run("every message", func(t *testing.T) {
		d := &scriptedInbound{messages: map[string]string{"u1": testMail("m1", "하나", "본문"), "u2": testMail("m2", "둘", "본문")},
			fetchFail: fetchTimeout, failEvery: 1}
		app, acc := scriptedApp(t, d)
		job := syncAndWait(t, app, acc.ID)
		if job.Status != domain.JobFailed || job.Error != syncStoppedTimeout || job.Stats["failed"] != 2 {
			t.Fatalf("job = %s %q stats=%v", job.Status, job.Error, job.Stats)
		}
		if job.Diagnostic == nil || job.Diagnostic.Stage != domain.StageFetch || job.Diagnostic.Command != "RETR" {
			t.Fatalf("diagnostic = %+v", job.Diagnostic)
		}
		if !strings.Contains(job.Diagnostic.Summary, "메일 본문 수신") || !strings.Contains(job.Diagnostic.Summary, "첨부") {
			t.Fatalf("summary = %q", job.Diagnostic.Summary)
		}
	})
	t.Run("some messages", func(t *testing.T) {
		d := &scriptedInbound{messages: map[string]string{"u1": testMail("m1", "하나", "본문"), "u2": testMail("m2", "둘", "본문")},
			fetchFail: fetchTimeout, failEvery: 2}
		app, acc := scriptedApp(t, d)
		job := syncAndWait(t, app, acc.ID)
		if job.Status != domain.JobPartial || job.Stats["new"] != 1 || job.Stats["failed"] != 1 {
			t.Fatalf("job = %s stats=%v", job.Status, job.Stats)
		}
	})
}

// The Sent-folder pass is best-effort: received mail must still sync, and the
// pass's own outcome must be visible rather than lost in a log line.
func TestSentFolderOutcomeIsRecordedWithoutFailingTheSync(t *testing.T) {
	for name, tc := range map[string]struct {
		folders *fakeFolders
		want    string
	}{
		"synced": {&fakeFolders{inbox: map[string]string{"1.1": testMail("in", "받은", "본문")},
			sent: map[string]string{"1.2": outgoingMail("out", "bob@example.com", "보낸", "본문")}, sentName: "Sent"}, sentSynced},
		"no sent folder": {&fakeFolders{inbox: map[string]string{"1.1": testMail("in", "받은", "본문")}}, sentNone},
		"refused": {&fakeFolders{inbox: map[string]string{"1.1": testMail("in", "받은", "본문")}, sentName: "Sent",
			selectErr: &domain.InboundError{Stage: domain.StageSelect, Command: "SELECT", Class: "rejected", Code: "NONEXISTENT"}}, sentFailed},
	} {
		t.Run(name, func(t *testing.T) {
			app, acc := sentTestApp(t, tc.folders)
			job := syncAndWait(t, app, acc.ID)
			if job.Status != domain.JobSucceeded || job.Stats["new"] != 1 {
				t.Fatalf("received mail did not sync: %s %q stats=%v", job.Status, job.Error, job.Stats)
			}
			if job.Diagnostic == nil || job.Diagnostic.Sent != tc.want {
				t.Fatalf("sent outcome = %+v, want %q", job.Diagnostic, tc.want)
			}
			if tc.want == sentFailed {
				if job.Diagnostic.SentStage != domain.StageSelect || job.Diagnostic.SentClass != "rejected" {
					t.Fatalf("sent failure not described: %+v", job.Diagnostic)
				}
				if !strings.Contains(job.Diagnostic.Summary, "보낸 메일함 확인에 실패") || !strings.Contains(job.Diagnostic.Summary, "메일함 선택") {
					t.Fatalf("summary = %q", job.Diagnostic.Summary)
				}
			}
		})
	}
}

// An IDLE wake-up skips the Sent folder, and says so: the pass is not lost,
// it is deferred to the next scheduled sync.
func TestIdleSyncRecordsTheSkippedSentPass(t *testing.T) {
	folders := &fakeFolders{inbox: map[string]string{"1.1": testMail("in", "받은", "본문")},
		sent: map[string]string{"1.2": outgoingMail("out", "bob@example.com", "보낸", "본문")}, sentName: "Sent"}
	app, acc := sentTestApp(t, folders)
	if job := syncAndWait(t, app, acc.ID); job.Diagnostic.Sent != sentSynced {
		t.Fatalf("first sync = %+v", job.Diagnostic)
	}
	ctx := WithActor(context.Background(), "idle-worker")
	started, err := app.StartSync(ctx, acc.ID, SyncOptions{fromIdle: true})
	if err != nil {
		t.Fatal(err)
	}
	job := waitForJob(t, app, started.ID)

	if job.Status != domain.JobSucceeded || job.Diagnostic == nil || job.Diagnostic.Sent != sentThrottled {
		t.Fatalf("idle sync = %s %+v", job.Status, job.Diagnostic)
	}
	if !strings.Contains(job.Diagnostic.Summary, "실시간 동기화에서 생략") {
		t.Fatalf("summary = %q", job.Diagnostic.Summary)
	}
}

// Configuration that stops a sync before any packet is sent must not read as a
// server problem.
func TestSyncStoppedByPolicyNamesTheConfigurationStep(t *testing.T) {
	app, acc := scriptedApp(t, &scriptedInbound{})
	if _, err := app.AdminPatchSettings(settingsAdmin(), SettingsPatch{Values: map[string]string{"mail.tls_required": "true"}}); err != nil {
		t.Fatal(err)
	}
	job := syncAndWait(t, app, acc.ID)
	if job.Status != domain.JobFailed || job.Error != syncStoppedConfig {
		t.Fatalf("job = %s %q", job.Status, job.Error)
	}
	if job.Diagnostic == nil || job.Diagnostic.Stage != domain.StagePolicy || job.Diagnostic.Class != classConfig {
		t.Fatalf("diagnostic = %+v", job.Diagnostic)
	}
	if !strings.Contains(job.Diagnostic.Summary, "관리자 정책 확인") {
		t.Fatalf("summary = %q", job.Diagnostic.Summary)
	}
}

// Timing tells a step that used up its own deadline from one cut short by a
// budget set elsewhere — the distinction the single word "timeout" hides.
func TestSyncDiagnosticTellsAnOutsideDeadlineFromTheStepsOwn(t *testing.T) {
	own := safeSyncDiagnostic(&domain.SyncDiagnostic{Stage: domain.StageFetch, Class: "timeout", ElapsedMS: 60000, TimeoutMS: 60000})
	if !strings.Contains(own.Summary, "제한 60.0초 중 60.0초 경과") {
		t.Fatalf("summary = %q", own.Summary)
	}
	outside := safeSyncDiagnostic(&domain.SyncDiagnostic{Stage: domain.StageFetch, Class: "timeout", ElapsedMS: 900, TimeoutMS: 60000})
	if !strings.Contains(outside.Summary, "이 단계의 제한 시간에는 이르지 않았습니다") {
		t.Fatalf("summary = %q", outside.Summary)
	}
}

// The diagnostic is persisted and shown, and a mail server's text can echo a
// password or message content, so the read boundary keeps only known tokens
// and rewrites the summary from them.
func TestSyncDiagnosticReadBoundaryDropsAnythingUnknown(t *testing.T) {
	hostile := &domain.SyncDiagnostic{
		Stage: "PASS hunter2", Command: "RETR; DROP TABLE", Class: "made-up", Code: "SECRET",
		Protocol: "smtp", Host: "evil host <script>", Port: -5, Security: "plaintext",
		ElapsedMS: -1, TimeoutMS: 1000, Attempts: -2, HostSessions: -3, AccountSessions: -4, SlotWaitMS: -5,
		Sent: "deleted-everything", SentStage: "rm -rf", SentClass: "boom", Recovered: "invented",
		Summary: "비밀번호는 hunter2 입니다",
	}
	got := safeSyncDiagnostic(hostile)
	if got != nil {
		t.Fatalf("a diagnostic with nothing known survived: %+v", got)
	}
	partial := safeSyncDiagnostic(&domain.SyncDiagnostic{Stage: domain.StageLogin, Class: "rejected", Code: "UNAVAILABLE",
		Command: "LOGIN", Host: "mail.corp.local", Port: 993, Protocol: "imap", Security: "tls",
		Summary: "server said: password hunter2"})
	if partial == nil || strings.Contains(partial.Summary, "hunter2") {
		t.Fatalf("stored summary was trusted: %+v", partial)
	}
	for _, want := range []string{"IMAP mail.corp.local:993 (tls)", "로그인 단계(LOGIN)", "[UNAVAILABLE]", "일시적인 사유로 거부"} {
		if !strings.Contains(partial.Summary, want) {
			t.Fatalf("summary %q lacks %q", partial.Summary, want)
		}
	}
}

func TestRetryableInboundOnlyCoversFailuresAnotherAttemptCouldPass(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"rejected password":     {&domain.AuthError{Err: &domain.InboundRejected{Code: "AUTHENTICATIONFAILED"}}, false},
		"permanent refusal":     {&domain.InboundRejected{Code: "NOPERM"}, false},
		"temporary refusal":     {&domain.InboundRejected{Code: "LIMIT"}, true},
		"timeout":               {&domain.InboundError{Class: "timeout", Err: fmt.Errorf("i/o timeout")}, true},
		"reset":                 {fmt.Errorf("read: connection reset by peer"), true},
		"untrusted certificate": {&domain.InboundError{Class: "tls_certificate", Err: fmt.Errorf("x509: unknown authority")}, false},
		"unknown host":          {&domain.InboundError{Class: "dns_not_found", Err: fmt.Errorf("no such host")}, false},
		"policy":                {stageError(domain.StagePolicy, userErrf("정책")), false},
	} {
		if got := retryableInbound(tc.err); got != tc.want {
			t.Fatalf("%s: retryable = %v", name, got)
		}
	}
}

func TestInboundCensusCountsSessionsPerHostAndAccount(t *testing.T) {
	census := &inboundCensus{}
	first := &domain.MailAccount{ID: "acc-1", POP3Host: "mail.corp.local"}
	second := &domain.MailAccount{ID: "acc-2", POP3Host: "mail.corp.local"}
	releaseFirst, releaseSecond := census.hold(first), census.hold(second)
	if hosts, accounts := census.count(first); hosts != 2 || accounts != 1 {
		t.Fatalf("hosts=%d accounts=%d", hosts, accounts)
	}
	releaseSecond()
	if hosts, accounts := census.count(first); hosts != 1 || accounts != 1 {
		t.Fatalf("after release: hosts=%d accounts=%d", hosts, accounts)
	}
	releaseFirst()
	if hosts, accounts := census.count(first); hosts != 0 || accounts != 0 {
		t.Fatalf("after both released: hosts=%d accounts=%d", hosts, accounts)
	}
}

// Accept every retry and keep each connection open until the client closes it.
// The longer server deadline is only a safety net, never the timeout under test.
func syncGreetingServer(t *testing.T, greeting string) (int, <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 8)
	done := make(chan struct{})
	t.Cleanup(func() {
		ln.Close()
		select {
		case <-done:
		case <-time.After(6 * time.Second):
			t.Error("greeting server did not stop")
		}
	})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			if _, err := io.WriteString(conn, greeting); err != nil {
				conn.Close()
				closed <- err
				continue
			}
			var b [1]byte
			n, err := conn.Read(b[:])
			conn.Close()
			if n != 0 || err != io.EOF {
				closed <- fmt.Errorf("after greeting read = (%d bytes, %v), want EOF", n, err)
			} else {
				closed <- nil
			}
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, closed
}

func TestSyncGreetingTimeoutUsesConnectBudget(t *testing.T) {
	fastRetries(t) // Changes package backoffs; these cases must not run in parallel.
	const marker = "private-greeting-marker"
	for _, protocol := range []string{"pop3", "imap"} {
		for _, class := range []string{"timeout", "rejected"} {
			t.Run(protocol+"/"+class, func(t *testing.T) {
				greeting := ""
				if class == "rejected" {
					greeting = "-ERR " + marker + "\r\n"
					if protocol == "imap" {
						greeting = "* BYE " + marker + "\r\n"
					}
				}
				port, closed := syncGreetingServer(t, greeting)
				app, _, _, _ := newTestApp(t)
				app.POP3 = pop3.Dialer{}
				app.IMAP = imap.Dialer{}
				view, err := app.AdminSettingsCatalog(settingsAdmin())
				if err != nil {
					t.Fatal(err)
				}
				_, err = app.AdminPatchSettings(settingsAdmin(), SettingsPatch{Revision: view.Revision, Values: map[string]string{
					"sync.connect_timeout_sec": "1", "sync.command_timeout_sec": "4",
				}})
				if err != nil {
					t.Fatal(err)
				}
				ctx := WithActor(context.Background(), "test")
				acc, err := app.CreateAccount(ctx, CreateAccountInput{
					Name: "greeting budget", Email: "me@corp.local", InboundProtocol: protocol,
					POP3Host: "127.0.0.1", POP3Port: port, POP3Security: "none",
					SMTPHost: "127.0.0.1", SMTPSecurity: "none", SMTPAuth: "none",
				})
				if err != nil {
					t.Fatal(err)
				}
				job := syncAndWait(t, app, acc.ID) // StartSync -> persisted GetJob.
				d := job.Diagnostic
				if job.Status != domain.JobFailed || d == nil {
					t.Fatalf("job = %+v, want failed with diagnostic", job)
				}
				if d.Stage != domain.StageGreeting || d.Class != class || d.Command != "" || d.Protocol != protocol {
					t.Fatalf("diagnostic = %+v", d)
				}
				if d.TimeoutMS != 1000 {
					t.Errorf("TimeoutMS = %d, want 1000", d.TimeoutMS)
				}
				if !strings.Contains(d.Summary, "제한 1.0초") {
					t.Errorf("summary lacks connect budget: %q", d.Summary)
				}
				if class == "timeout" {
					if d.ElapsedMS <= 0 || d.Attempts != 2 || job.Error != syncStoppedTimeout {
						t.Errorf("timeout job = %+v", job)
					}
					if strings.Contains(d.Summary, "이 단계의 제한 시간에는 이르지 않았습니다") {
						t.Errorf("own deadline reported as early interruption: %q", d.Summary)
					}
				}
				stored, err := json.Marshal(job)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(stored), marker) {
					t.Fatal("server marker leaked into stored job")
				}
				for attempt := 0; attempt < d.Attempts; attempt++ {
					select {
					case err := <-closed:
						if err != nil {
							t.Fatal(err)
						}
					case <-time.After(6 * time.Second):
						t.Fatal("server did not observe peer EOF before cleanup")
					}
				}
				deadline := time.Now().Add(5 * time.Second)
				for {
					incidents, err := app.AdminListIncidents(settingsAdmin(), domain.IncidentFilter{})
					if err != nil {
						t.Fatal(err)
					}
					for _, inc := range incidents {
						if inc.Component == "sync" && inc.Severity == domain.SeverityError && inc.Detail == d.Summary {
							return
						}
					}
					if time.Now().After(deadline) {
						t.Fatal("no sync incident carried the diagnostic")
					}
					time.Sleep(20 * time.Millisecond)
				}
			})
		}
	}
}

package application

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"postra/internal/domain"
)

const diagnosticSecret = "arbitrary-unrecognizable-shared-mail-password-937"

type echoDiagnosticSMTP struct {
	err     error
	receipt domain.SendReceipt
	calls   int
}

func (*echoDiagnosticSMTP) TestConnection(context.Context, domain.SMTPSendOptions) (*domain.ConnDiagnostics, error) {
	return &domain.ConnDiagnostics{Target: diagnosticSecret, Steps: []domain.ConnStep{{Step: diagnosticSecret, Detail: diagnosticSecret}, {Step: "smtp_ehlo_auth", OK: true, Detail: diagnosticSecret}}}, nil
}
func (s *echoDiagnosticSMTP) Send(_ context.Context, opts domain.SMTPSendOptions, _ domain.Envelope, body io.Reader) (domain.SendReceipt, error) {
	if opts.Password != nil {
		defer opts.Password.Zero()
	}
	if _, err := io.Copy(io.Discard, body); err != nil {
		return domain.SendReceipt{}, err
	}
	s.calls++
	return s.receipt, s.err
}

func assertNoProviderEcho(t *testing.T, value any) {
	t.Helper()
	wire, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), diagnosticSecret) {
		t.Fatal("provider credential echo leaked into a response or persisted diagnostic")
	}
}

func TestProviderDiagnosticsOutboundSuccessDTOsAndHistory(t *testing.T) {
	for _, test := range []struct {
		name    string
		err     error
		receipt domain.SendReceipt
		retries int
		want    domain.OutboundStatus
	}{
		{"permanent", errors.New(diagnosticSecret), domain.SendReceipt{}, 3, domain.OutboundFailed},
		{"retry", classifiedErr{diagnosticSecret, true}, domain.SendReceipt{}, 3, domain.OutboundRetryWait},
		{"exhausted", classifiedErr{diagnosticSecret, true}, domain.SendReceipt{}, 1, domain.OutboundFailed},
		{"uncertain", nil, domain.SendReceipt{Uncertain: true, ServerResponse: diagnosticSecret}, 3, domain.OutboundUncertain},
		{"accepted", nil, domain.SendReceipt{ServerResponse: diagnosticSecret}, 3, domain.OutboundSent},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, _, _, _ := newTestApp(t)
			app.Cfg.Send.MaxRetries = test.retries
			account := mustAccount(t, app)
			ctx := context.Background()
			echo := &echoDiagnosticSMTP{err: test.err, receipt: test.receipt}
			app.SMTP = echo
			draft, err := app.CreateDraft(ctx, CreateDraftInput{AccountID: account.ID, To: []string{"colleague@corp.local"}, Subject: "진단 계약", Body: "확인 부탁드립니다."})
			if err != nil {
				t.Fatal(err)
			}
			_, approval, err := app.RequestSendApproval(ctx, draft.Draft.ID, "user", 120)
			if err != nil {
				t.Fatal(err)
			}
			input := SendInput{DraftID: draft.Draft.ID, ApprovalToken: approval.Token, IdempotencyKey: "echo-once"}
			out, err := app.Send(ctx, input)
			if err != nil || out.Status != test.want || out.Attempts != 1 {
				t.Fatalf("send classification changed: %+v %v", out, err)
			}
			if (out.NextAttemptAt > 0) != (test.want == domain.OutboundRetryWait) {
				t.Fatal("uncertainty/retry scheduling changed")
			}
			assertNoProviderEcho(t, out)
			stored, err := app.Store.GetOutbound(ctx, DefaultUserID, out.ID)
			if err != nil {
				t.Fatal(err)
			}
			assertNoProviderEcho(t, stored)
			for i := 0; i < 2; i++ {
				read, err := app.GetOutbound(ctx, out.ID)
				if err != nil {
					t.Fatal(err)
				}
				assertNoProviderEcho(t, read)
				list, err := app.ListOutbound(ctx, 20)
				if err != nil {
					t.Fatal(err)
				}
				assertNoProviderEcho(t, list)
				replay, err := app.Send(ctx, input)
				if err != nil || replay.ID != out.ID || echo.calls != 1 {
					t.Fatal("diagnostic sanitization changed idempotency")
				}
				assertNoProviderEcho(t, replay)
				// Simulate a historical version that persisted raw provider text.
				// Reads/replays must sanitize it without destructively rewriting it.
				if err := app.Store.UpdateOutbound(ctx, out.ID, test.want, diagnosticSecret, 1); err != nil {
					t.Fatal(err)
				}
			}
			audit, err := app.Store.SearchAudit(ctx, DefaultUserID, 100)
			if err != nil {
				t.Fatal(err)
			}
			assertNoProviderEcho(t, audit)
		})
	}
}

type echoDiagnosticDialer struct{ mode string }

func (d echoDiagnosticDialer) Dial(context.Context, domain.POP3DialOptions) (domain.POP3Session, error) {
	switch d.mode {
	case "panic":
		panic(diagnosticSecret)
	case "auth":
		return nil, &domain.AuthError{Err: errors.New(diagnosticSecret)}
	case "list":
		return echoDiagnosticSession{}, nil
	default:
		return nil, errors.New(diagnosticSecret)
	}
}

type echoDiagnosticSession struct{ domain.POP3Session }

func (echoDiagnosticSession) UIDL(context.Context) ([]domain.RemoteMessage, error) {
	return nil, errors.New(diagnosticSecret)
}
func (echoDiagnosticSession) List(context.Context) ([]domain.RemoteMessage, error) {
	return nil, errors.New(diagnosticSecret)
}
func (echoDiagnosticSession) Close() error               { return nil }
func (echoDiagnosticSession) Quit(context.Context) error { return nil }

func TestProviderDiagnosticsSyncAndConnectionDTOs(t *testing.T) {
	for _, mode := range []string{"connect", "auth", "list", "panic"} {
		t.Run(mode, func(t *testing.T) {
			app, _, _, _ := newTestApp(t)
			account := mustAccount(t, app)
			app.POP3 = echoDiagnosticDialer{mode: mode}
			ctx := context.Background()
			job := &domain.Job{ID: "echo-sync", UserID: DefaultUserID, AccountID: account.ID, Type: "sync", Status: domain.JobQueued}
			if err := app.Store.CreateJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			app.runSync(ctx, job, account, SyncOptions{})
			if job.Status != domain.JobFailed || job.Error == "" {
				t.Fatalf("sync state changed: %+v", job)
			}
			assertNoProviderEcho(t, job)
			stored, err := app.Store.GetJob(ctx, DefaultUserID, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			assertNoProviderEcho(t, stored)
			if mode == "auth" {
				account, err = app.GetAccount(ctx, account.ID)
				if err != nil || account.Status != domain.AccountCredentialError {
					t.Fatal("bad credentials did not disable retries")
				}
			}
			incidents, err := app.Store.ListIncidents(ctx, domain.IncidentFilter{})
			if err != nil {
				t.Fatal(err)
			}
			assertNoProviderEcho(t, incidents)
			// Connection diagnostics are success DTOs too, including arbitrary
			// provider-supplied step labels and success-message details.
			if mode != "panic" {
				app.SMTP = &echoDiagnosticSMTP{}
				diags, err := app.TestAccount(ctx, account.ID)
				if err != nil {
					t.Fatal(err)
				}
				assertNoProviderEcho(t, diags)
			}
			stored.Error = diagnosticSecret
			if err := app.Store.UpdateJob(ctx, stored); err != nil {
				t.Fatal(err)
			}
			read, err := app.GetJob(ctx, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			assertNoProviderEcho(t, read)
			list, err := app.ListJobs(ctx, 20)
			if err != nil {
				t.Fatal(err)
			}
			assertNoProviderEcho(t, list)
		})
	}
}

type echoDiagnosticAI struct{ fakeAI }

func (echoDiagnosticAI) Embed(context.Context, domain.EmbeddingRequest) (domain.EmbeddingResult, error) {
	return domain.EmbeddingResult{}, errors.New(diagnosticSecret)
}
func TestProviderDiagnosticsEmbeddingAndEvaluationDTOs(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	ctx := context.Background()
	account := mustAccount(t, app)
	message := &domain.Message{ID: "echo-ai", UserID: DefaultUserID, AccountID: account.ID, UIDL: "echo-ai", RawHash: "echo-ai", Subject: "색인 검사"}
	if err := app.Store.InsertMessage(ctx, message, &domain.MessageBody{MessageID: message.ID, TextBody: "색인 대상 본문"}, nil); err != nil {
		t.Fatal(err)
	}
	app.AI = &echoDiagnosticAI{fakeAI: fakeAI{genErr: errors.New(diagnosticSecret)}}
	job := &domain.Job{ID: "echo-embed", UserID: DefaultUserID, AccountID: account.ID, Type: "embed", Status: domain.JobQueued}
	if err := app.Store.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	app.runBuildEmbeddings(ctx, job, account.ID, 5)
	if job.Status != domain.JobFailed || job.Error == "" {
		t.Fatalf("missing AI failure: %+v", job)
	}
	stored, err := app.Store.GetJob(ctx, DefaultUserID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertNoProviderEcho(t, stored)
	audit, err := app.Store.SearchAudit(ctx, DefaultUserID, 100)
	if err != nil {
		t.Fatal(err)
	}
	assertNoProviderEcho(t, audit)
	eval, err := app.EvaluatePrompt(ctx, "summarize", []EvalCase{{MessageID: message.ID, Expected: "검사"}})
	if err != nil || len(eval.Cases) != 1 || eval.Cases[0].Error == "" {
		t.Fatalf("evaluation failure lost: %+v %v", eval, err)
	}
	assertNoProviderEcho(t, eval)
	audit, err = app.Store.SearchAudit(ctx, DefaultUserID, 100)
	if err != nil {
		t.Fatal(err)
	}
	assertNoProviderEcho(t, audit)
	probe, err := app.AdminTestAI(settingsAdmin())
	if err != nil || probe.OK || probe.Message == "" {
		t.Fatalf("AI probe failed to report safe failure: %+v %v", probe, err)
	}
	assertNoProviderEcho(t, probe)
	embedProbe, err := app.AdminTestEmbeddingStore(settingsAdmin())
	if err != nil || embedProbe.OK || embedProbe.Message == "" {
		t.Fatalf("embedding probe failed to report safe failure: %+v %v", embedProbe, err)
	}
	assertNoProviderEcho(t, embedProbe)
}

func TestProviderDiagnosticsClassificationsAreFixed(t *testing.T) {
	for _, err := range []error{errors.New(diagnosticSecret), context.DeadlineExceeded, context.Canceled, &domain.AuthError{Err: errors.New(diagnosticSecret)}} {
		assertNoProviderEcho(t, providerDiagnostic(err))
	}
	for code, expected := range map[string]string{
		"context_limit": providerAIContext, "output_limit": providerAIOutput, "empty_response": providerAIEmpty,
		"model_disabled": providerAIDisabled, "invalid_embedding": providerAIEmbedding, "invalid_response": providerAIResponse,
		"ai_auth_failed": providerAIAuth, "ai_forbidden": providerAIForbidden, "ai_rate_limited": providerAIRate,
		"ai_quota_exceeded": providerAIQuota, "ai_timeout": providerAITimeout, "ai_unreachable": providerAINetwork,
		"ai_request_rejected": providerAIRejected, "upstream_untrusted": providerFailed,
	} {
		err := &domain.PublicError{Code: code, Message: diagnosticSecret, Status: 400}
		if got := providerDiagnostic(err); got != expected {
			t.Fatalf("unsafe or unhelpful diagnostic for %s", code)
		}
		assertNoProviderEcho(t, providerDiagnostic(err))
		if got := jobDiagnostic("embed", domain.JobFailed, expected); got != expected {
			t.Fatalf("fixed diagnostic lost in job history: %s", code)
		}
	}
	// Fixed legacy recovery messages must not be accepted by prefix.
	job := &domain.Job{Type: "embed", Status: domain.JobFailed, Error: providerEmbedFailed + diagnosticSecret, UpdatedAt: time.Now().Unix()}
	assertNoProviderEcho(t, safeJob(job))
	if job.Error == safeJob(job).Error {
		t.Fatal("read redaction mutated the stored row")
	}
}

type echoDiagnosticStorage struct{ Storage }

func (echoDiagnosticStorage) GetMessage(context.Context, string, string) (*domain.Message, error) {
	return nil, errors.New(diagnosticSecret)
}

type echoDiagnosticVector struct {
	VectorStore
	panicMissing bool
}

func (v echoDiagnosticVector) MessagesMissingEmbeddings(context.Context, string, string, int) ([]string, error) {
	if v.panicMissing {
		panic(diagnosticSecret)
	}
	return nil, errors.New(diagnosticSecret)
}
func (echoDiagnosticVector) Ping(context.Context) error { return errors.New(diagnosticSecret) }

func TestProviderDiagnosticsStorageAndVectorDTOs(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	ctx := context.Background()
	base := app.Store
	app.Store = echoDiagnosticStorage{Storage: base}
	batch, err := app.BatchUpdateMessages(ctx, BatchUpdateOptions{Action: BatchActionArchive, MessageIDs: []string{"safe-id"}})
	if err != nil || batch.Failed != 1 || batch.Results[0].Error == "" {
		t.Fatalf("batch error state lost: %+v %v", batch, err)
	}
	assertNoProviderEcho(t, batch)
	app.Store = base
	vector := app.VectorStore()
	for _, crash := range []bool{false, true} {
		app.vectorMu.Lock()
		app.vectorStore = echoDiagnosticVector{VectorStore: vector, panicMissing: crash}
		app.vectorMu.Unlock()
		id := "vector-error"
		if crash {
			id = "vector-panic"
		}
		job := &domain.Job{ID: id, UserID: DefaultUserID, Type: "embed", Status: domain.JobQueued}
		if err := app.Store.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		app.runBuildEmbeddings(ctx, job, "", 5)
		stored, err := app.Store.GetJob(ctx, DefaultUserID, id)
		if err != nil || stored.Status != domain.JobFailed {
			t.Fatalf("vector job failure lost: %+v %v", stored, err)
		}
		assertNoProviderEcho(t, stored)
	}
	probe, err := app.AdminTestEmbeddingStore(settingsAdmin())
	if err != nil || probe.OK || !probe.AIEmbedOK || probe.VectorStoreOK {
		t.Fatalf("vector probe state lost: %+v %v", probe, err)
	}
	assertNoProviderEcho(t, probe)
	incidents, err := app.Store.ListIncidents(ctx, domain.IncidentFilter{})
	if err != nil {
		t.Fatal(err)
	}
	assertNoProviderEcho(t, incidents)
}

func TestProviderDiagnosticsHistoricalAuditAndIncidentReads(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	ctx := settingsAdmin()
	const businessDetail = "sync.auto_sync_minutes changed from 5 to 7"
	for _, action := range []string{"mail_send", "ai_analysis", "embed_batch_failed", "secret_rotate"} {
		if err := app.Store.AppendAudit(ctx, domain.AuditEvent{UserID: DefaultUserID, Actor: "fixture", Action: action, Result: "error", Detail: diagnosticSecret}); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.Store.AppendAudit(ctx, domain.AuditEvent{UserID: DefaultUserID, Actor: "fixture", Action: "settings_change", Result: "ok", Detail: businessDetail}); err != nil {
		t.Fatal(err)
	}
	audit, err := app.SearchAudit(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	assertNoProviderEcho(t, audit)
	found := false
	for _, event := range audit {
		if event.Action == "settings_change" && event.Detail == businessDetail {
			found = true
		}
	}
	if !found {
		t.Fatal("historical normalization erased business audit metadata")
	}
	for _, component := range []string{"sync", "sync-worker", "embeddings", "oidc", "oidc-mail"} {
		app.recordIncident(domain.SeverityError, component, diagnosticSecret, diagnosticSecret, withIncidentJob("known-job"))
	}
	app.recordIncident(domain.SeverityWarning, "maintenance", "planned service maintenance", businessDetail)
	incidents, err := app.AdminListIncidents(ctx, domain.IncidentFilter{})
	if err != nil {
		t.Fatal(err)
	}
	assertNoProviderEcho(t, incidents)
	found = false
	for _, incident := range incidents {
		public, err := app.AdminGetIncident(ctx, incident.ID)
		if err != nil {
			t.Fatal(err)
		}
		assertNoProviderEcho(t, public)
		if incident.Component == "maintenance" {
			if incident.Detail == businessDetail {
				found = true
			}
			continue
		}
		stored, err := app.Store.GetIncident(ctx, incident.ID)
		if err != nil || stored.Message != diagnosticSecret || stored.Detail != diagnosticSecret || public.JobID != "known-job" {
			t.Fatal("read normalization mutated historical data or erased correlation")
		}
	}
	if !found {
		t.Fatal("unrelated incident detail was changed")
	}
}

package pgstore

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"postra/internal/domain"
)

// openTestStore connects to the integration database, or skips.
func openTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("POSTRA_TEST_PG")
	if dsn == "" {
		t.Skip("set POSTRA_TEST_PG to a pgvector-enabled Postgres DSN to run")
	}
	ctx := context.Background()
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, ctx
}

// The secret store moved into the database so credentials survive a restart
// and are visible to every replica; none of this SQL had ever run on Postgres.
func TestPGSecretEnvelopes(t *testing.T) {
	s, ctx := openTestStore(t)
	ref := NewID("sec")
	sec := domain.StoredSecret{
		Ref: ref, Owner: "u1", Type: domain.SecretMailPassword,
		Label: "메일 비밀번호", Envelope: `{"alg":"aes-256-gcm"}`, Version: 1,
	}
	if err := s.PutSecretEnvelope(ctx, sec); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSecretEnvelope(ctx, ref)
	if err != nil || got.Envelope != sec.Envelope || got.Type != domain.SecretMailPassword {
		t.Fatalf("round-trip failed: %+v (err=%v)", got, err)
	}
	// Rotation upserts in place, keeping the same reference.
	sec.Envelope, sec.Version = `{"alg":"rotated"}`, 2
	if err := s.PutSecretEnvelope(ctx, sec); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.GetSecretEnvelope(ctx, ref); got.Version != 2 || got.Envelope != `{"alg":"rotated"}` {
		t.Fatalf("rotation did not replace the envelope: %+v", got)
	}
	list, err := s.ListSecretEnvelopes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range list {
		if e.Ref == ref {
			found = true
		}
	}
	if !found {
		t.Fatal("rewrap listing does not include the stored secret")
	}
	// Revoking drops the ciphertext and hides it from the listing.
	if err := s.MarkSecretEnvelopeRevoked(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.GetSecretEnvelope(ctx, ref); !got.Revoked || got.Envelope != "" {
		t.Fatalf("revoke left the secret usable: %+v", got)
	}
	if _, err := s.GetSecretEnvelope(ctx, "sec_missing"); err != domain.ErrNotFound {
		t.Fatalf("missing secret should report not-found, got %v", err)
	}
}

// Raw MIME and attachments moved into the database for the same reason; BYTEA
// round-tripping is the part that cannot be verified on SQLite.
func TestPGObjectBlobs(t *testing.T) {
	s, ctx := openTestStore(t)
	name := NewID("obj")
	blob := []byte("\x00\x01binary\xff본문")
	if err := s.PutObject(ctx, "raw", name, blob); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetObject(ctx, "raw", name)
	if err != nil || string(got) != string(blob) {
		t.Fatalf("blob round-trip failed: %q (err=%v)", got, err)
	}
	// Storing the same content again must not error (dedup by content name).
	if err := s.PutObject(ctx, "raw", name, blob); err != nil {
		t.Fatalf("re-storing identical content failed: %v", err)
	}
	if err := s.OverwriteObject(ctx, "raw", name, []byte("rewrapped")); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.GetObject(ctx, "raw", name); string(got) != "rewrapped" {
		t.Fatalf("overwrite did not replace the blob: %q", got)
	}
	var seen bool
	if err := s.WalkObjects(ctx, func(kind, n string, b []byte) error {
		if n == name {
			seen = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("key rotation walk does not reach the stored object")
	}
	if err := s.DeleteObject(ctx, "raw", name); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetObject(ctx, "raw", name); err != domain.ErrNotFound {
		t.Fatalf("deleted object should report not-found, got %v", err)
	}
}

// Incidents drive the admin dashboard; the aggregate query in particular uses
// Postgres-specific SUM/CASE typing.
func TestPGIncidents(t *testing.T) {
	s, ctx := openTestStore(t)
	fp := NewID("fp")
	inc := &domain.Incident{
		ID: NewID("inc"), Fingerprint: fp, Severity: domain.SeverityCritical,
		Component: "pgtest", Message: "재현용 오류", Detail: "stack",
		Count: 1, FirstSeen: time.Now().Unix(), LastSeen: time.Now().Unix(),
	}
	if err := s.RecordIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	// A recurrence collapses into the same row rather than flooding the view.
	inc2 := *inc
	inc2.ID, inc2.LastSeen = NewID("inc"), time.Now().Unix()+1
	if err := s.RecordIncident(ctx, &inc2); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListIncidents(ctx, domain.IncidentFilter{Component: "pgtest"})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Count != 2 {
		t.Fatalf("recurrence did not dedupe: %+v", list)
	}
	id := list[0].ID
	if _, err := s.GetIncident(ctx, id); err != nil {
		t.Fatal(err)
	}
	stats, err := s.IncidentStats(ctx)
	if err != nil {
		t.Fatalf("stats aggregate failed on Postgres: %v", err)
	}
	if stats.OpenCritical < 1 || stats.OpenTotal < 1 {
		t.Fatalf("stats did not count the open incident: %+v", stats)
	}
	if err := s.ResolveIncident(ctx, id, "admin"); err != nil {
		t.Fatal(err)
	}
	open, _ := s.ListIncidents(ctx, domain.IncidentFilter{Component: "pgtest"})
	if len(open) != 0 {
		t.Fatalf("resolved incident still listed as open: %+v", open)
	}
	if err := s.ResolveIncident(ctx, id, "admin"); err != ErrNotFound {
		t.Fatalf("resolving twice should report not-found, got %v", err)
	}
}

// seedMessage inserts a message owned by a fresh user, returning both IDs.
func seedMessage(t *testing.T, s *Store, ctx context.Context, subject, bodyText string, date, created int64) (userID, msgID, accID string) {
	t.Helper()
	userID = NewID("usr")
	if err := s.EnsureUser(ctx, userID, userID); err != nil {
		t.Fatal(err)
	}
	acc := &domain.MailAccount{ID: NewID("acc"), UserID: userID, Name: "t", Email: "me@x.com", Status: domain.AccountActive}
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	m := &domain.Message{
		ID: NewID("msg"), UserID: userID, AccountID: acc.ID, UIDL: NewID("u"),
		Subject: subject, From: domain.Address{Email: "a@x.com"},
		Date: date, CreatedAt: created, RawHash: NewID("h"), RawURI: "local://raw/x",
	}
	if err := s.InsertMessage(ctx, m, &domain.MessageBody{MessageID: m.ID, TextBody: bodyText}, nil); err != nil {
		t.Fatal(err)
	}
	return userID, m.ID, acc.ID
}

// The triage sweep filters labels with a concatenated LIKE pattern — syntax
// that differs from the SQLite adapter and had never executed.
func TestPGMessagesNeedingTriage(t *testing.T) {
	s, ctx := openTestStore(t)
	now := time.Now().Unix()
	userID, msgID, _ := seedMessage(t, s, ctx, "분류 대상", "본문", now, now)

	ids, err := s.MessagesNeedingTriage(ctx, userID, now-3600, 10)
	if err != nil {
		t.Fatalf("triage sweep query failed on Postgres: %v", err)
	}
	if len(ids) != 1 || ids[0] != msgID {
		t.Fatalf("untriaged message not selected: %v", ids)
	}

	// Once labelled it leaves the queue, which is what stops the worker from
	// paying for the same message every tick.
	m, err := s.GetMessage(ctx, userID, msgID)
	if err != nil {
		t.Fatal(err)
	}
	m.Labels = []string{"ai/urgent"}
	if err := s.UpdateMessage(ctx, m); err != nil {
		t.Fatal(err)
	}
	if ids, _ = s.MessagesNeedingTriage(ctx, userID, now-3600, 10); len(ids) != 0 {
		t.Fatalf("labelled message still queued for triage: %v", ids)
	}

	// The sweep is scoped by arrival: mail older than the window is left alone
	// so enabling triage does not fan out over the whole archive.
	_, oldMsg, _ := seedMessage(t, s, ctx, "오래된 메일", "본문", now, now)
	if ids, _ = s.MessagesNeedingTriage(ctx, userID, now+3600, 10); len(ids) != 0 {
		t.Fatalf("a future window should match nothing, got %v (%s)", ids, oldMsg)
	}
}

// The evidence/preview fetch passes a Go slice to ANY($2) and caps by length.
func TestPGBodyTextBatch(t *testing.T) {
	s, ctx := openTestStore(t)
	now := time.Now().Unix()
	userID, msgID, _ := seedMessage(t, s, ctx, "본문 조회", "미리보기 본문입니다", now, now)

	got, err := s.BodyTextBatch(ctx, userID, []string{msgID, "msg_missing"})
	if err != nil {
		t.Fatalf("batch body fetch failed on Postgres: %v", err)
	}
	if got[msgID] != "미리보기 본문입니다" {
		t.Fatalf("body not returned: %q", got[msgID])
	}
	if _, ok := got["msg_missing"]; ok {
		t.Fatal("unknown id should be absent, not empty-valued")
	}
	// An empty request must not build a broken query.
	if got, err = s.BodyTextBatch(ctx, userID, nil); err != nil || len(got) != 0 {
		t.Fatalf("empty batch: %v (err=%v)", got, err)
	}
}

// Job recovery builds its placeholder list dynamically, and the grace window
// is what keeps a live sync from being reaped.
func TestPGJobRecovery(t *testing.T) {
	s, ctx := openTestStore(t)
	userID := NewID("usr")
	if err := s.EnsureUser(ctx, userID, userID); err != nil {
		t.Fatal(err)
	}
	job := &domain.Job{ID: NewID("job"), UserID: userID, Type: "sync", AccountID: "acc_x", Status: domain.JobRunning}
	if err := s.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	// Fresh heartbeat: a generous grace window must spare it.
	n, err := s.RecoverStaleJobsExcept(ctx, nil, 3600)
	if err != nil {
		t.Fatalf("recovery query failed on Postgres: %v", err)
	}
	if got, _ := s.GetJob(ctx, userID, job.ID); got.Status != domain.JobRunning {
		t.Fatalf("live job reaped (n=%d, status=%s)", n, got.Status)
	}
	// Excluding an active ID uses the dynamic placeholder path.
	if _, err := s.RecoverStaleJobsExcept(ctx, []string{job.ID, "job_other"}, 0); err != nil {
		t.Fatalf("recovery with an exclusion list failed: %v", err)
	}
	if got, _ := s.GetJob(ctx, userID, job.ID); got.Status != domain.JobRunning {
		t.Fatal("an explicitly active job was reaped")
	}
	if err := s.TouchJob(ctx, job.ID); err != nil {
		t.Fatalf("heartbeat failed: %v", err)
	}
	// No grace and not excluded: now it is abandoned.
	if _, err := s.RecoverStaleJobsExcept(ctx, nil, 0); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetJob(ctx, userID, job.ID); got.Status != domain.JobFailed {
		t.Fatalf("abandoned job not recovered, status=%s", got.Status)
	}
	if _, err := s.FailStaleAccountJobs(ctx, "acc_x", 0); err != nil {
		t.Fatalf("per-account cleanup failed: %v", err)
	}
}

// Arrival-time filtering and in-place body repair (which rebuilds the search
// vector) are both Postgres-specific SQL added this cycle.
func TestPGArrivalFilterAndBodyRepair(t *testing.T) {
	s, ctx := openTestStore(t)
	now := time.Now().Unix()
	// Sender claims an old date; the store stamps arrival itself.
	userID, msgID, _ := seedMessage(t, s, ctx, "지연 도착", "", now-90*24*3600, now)

	byDate, err := s.Search(ctx, domain.SearchQuery{UserID: userID, Since: now - 3600, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(byDate.Messages) != 0 {
		t.Fatal("premise: the stale Date header should fall outside a Since window")
	}
	byArrival, err := s.Search(ctx, domain.SearchQuery{UserID: userID, ReceivedSince: now - 3600, Limit: 10})
	if err != nil {
		t.Fatalf("arrival-time filter failed on Postgres: %v", err)
	}
	if len(byArrival.Messages) != 1 {
		t.Fatalf("ReceivedSince matched %d messages, want 1", len(byArrival.Messages))
	}

	// An empty body marks the message for repair.
	need, err := s.UIDLsNeedingBodyRepair(ctx, byArrival.Messages[0].AccountID)
	if err != nil {
		t.Fatalf("repair scan failed: %v", err)
	}
	if len(need) != 1 {
		t.Fatalf("empty-bodied message not flagged for repair: %v", need)
	}
	if err := s.UpdateMessageBody(ctx, msgID, &domain.MessageBody{MessageID: msgID, TextBody: "복구된 finance 본문"}); err != nil {
		t.Fatalf("in-place body repair failed: %v", err)
	}
	// The rewrite must also refresh the search vector, or repaired mail stays
	// unfindable by its restored content.
	found, err := s.Search(ctx, domain.SearchQuery{UserID: userID, Text: "finance", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(found.Messages) != 1 {
		t.Fatalf("repaired body is not searchable: %d hits", len(found.Messages))
	}
	if need, _ = s.UIDLsNeedingBodyRepair(ctx, byArrival.Messages[0].AccountID); len(need) != 0 {
		t.Fatalf("message still flagged after repair: %v", need)
	}
}

// OIDC account linking looks users up by a unique active email.
func TestPGGetUserByEmail(t *testing.T) {
	s, ctx := openTestStore(t)
	// The integration database persists between runs, so a fixed address would
	// pass once and then read as ambiguous forever.
	email := NewID("who") + "@corp.local"
	u := &domain.User{
		ID: NewID("usr"), LoginID: NewID("login"), Email: email,
		Role: domain.RoleUser, Status: domain.UserActive, AuthProvider: "local",
	}
	if err := s.CreateUser(ctx, u, ""); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetUserByEmail(ctx, strings.ToUpper(email)) // case-insensitive
	if err != nil || got.ID != u.ID {
		t.Fatalf("email lookup failed: %+v (err=%v)", got, err)
	}
	if _, err := s.GetUserByEmail(ctx, ""); err != ErrNotFound {
		t.Fatalf("empty email must not match anyone, got %v", err)
	}
	// Ambiguity must not be resolved by guessing.
	dup := &domain.User{
		ID: NewID("usr"), LoginID: NewID("login"), Email: email,
		Role: domain.RoleUser, Status: domain.UserActive, AuthProvider: "local",
	}
	if err := s.CreateUser(ctx, dup, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetUserByEmail(ctx, email); err != ErrNotFound {
		t.Fatalf("an ambiguous email must not resolve, got %v", err)
	}
}

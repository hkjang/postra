package application

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"postra/internal/adapters/mailparse"
	"postra/internal/adapters/persistence"
	"postra/internal/domain"
	"postra/internal/platform/metrics"
	"postra/internal/platform/telemetry"
)

type SyncOptions struct {
	MaxMessages int  `json:"max_messages,omitempty"`
	FullSync    bool `json:"full_sync,omitempty"`
	// RepairBodies re-fetches and rewrites the body of already-stored messages
	// whose body is missing/empty/undecryptable (e.g. sealed under a key lost
	// on restart), preserving message identity. No new messages are ingested.
	RepairBodies bool `json:"repair_bodies,omitempty"`
	// DeleteAfterFetch is intentionally absent from the MVP sync path:
	// server-side deletion is a separate, approval-gated flow (§5.2).

	// fromIdle marks a sync an IDLE wake-up started. IDLE watches the inbox,
	// and every new message wakes it, so these syncs read the Sent folder at
	// most every sentFolderIdleInterval instead of on every arrival.
	fromIdle bool
	// done, when set, is closed once the worker has finished. The IDLE watch
	// waits on it before reconnecting, so one account never holds two server
	// connections at once.
	done chan struct{}
}

// sentFolderIdleInterval bounds how often IDLE-started syncs enumerate the
// Sent folder. Scheduled and manual syncs always read it.
var sentFolderIdleInterval = 10 * time.Minute

// StartSync launches an asynchronous POP3 sync job and returns its job ID.
func (a *App) StartSync(ctx context.Context, accountID string, opts SyncOptions) (*domain.Job, error) {
	userID := userIDFrom(ctx)
	acc, err := a.GetAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if acc.Status != domain.AccountActive {
		return nil, userErrf("account %s is %s", accountID, acc.Status)
	}
	if acc.POP3Host == "" {
		return nil, userErrf("account %s has no POP3 server configured", accountID)
	}
	if _, loaded := a.syncLocks.LoadOrStore(accountID, struct{}{}); loaded {
		return nil, userErrf("a sync for account %s is already running", accountID)
	}

	// Unstick a previous sync for this account that a crashed/restarted worker
	// left "진행 중": holding the (fresh) in-memory lock means no live local
	// sync exists, so any still-"running" DB job that stopped heart-beating is
	// dead and safe to fail now instead of waiting for the periodic reaper.
	if n, err := a.Store.FailStaleAccountJobs(ctx, accountID, staleJobGraceSeconds); err == nil && n > 0 {
		slog.Info("cleared stale sync job on new sync start", "account", accountID, "count", n)
	}

	job := &domain.Job{
		ID: persistence.NewID("job"), UserID: userID,
		Type: "sync", AccountID: accountID, Status: domain.JobQueued,
	}
	if err := a.Store.CreateJob(ctx, job); err != nil {
		a.syncLocks.Delete(accountID)
		return nil, err
	}
	initial := *job // response must not race with the mutable worker-owned job
	a.audit(ctx, "sync_start", "account:"+accountID, "ok", "job:"+job.ID)

	jobCtx, cancel := context.WithCancel(a.background)
	if p, ok := PrincipalFrom(ctx); ok {
		jobCtx = WithPrincipal(jobCtx, p)
	}
	a.jobCancels.Store(job.ID, cancel)
	a.workerGroup.Add(1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("sync worker panic", "account", accountID, "job", job.ID)
				a.recordIncident(domain.SeverityCritical, "sync-worker",
					providerUnexpected, string(debug.Stack()),
					withIncidentAccount(accountID), withIncidentJob(job.ID))
			}
		}()
		defer a.workerGroup.Done()
		if opts.done != nil {
			defer close(opts.done)
		}
		defer a.syncLocks.Delete(accountID)
		defer a.jobCancels.Delete(job.ID)
		a.runSync(jobCtx, job, acc, opts)
	}()
	return &initial, nil
}

func (a *App) CancelJob(ctx context.Context, jobID string) error {
	if _, err := a.Store.GetJob(ctx, userIDFrom(ctx), jobID); err != nil {
		return err
	}
	if c, ok := a.jobCancels.Load(jobID); ok {
		c.(context.CancelFunc)()
		a.audit(ctx, "job_cancel", "job:"+jobID, "ok", "")
		return nil
	}
	return userErrf("job %s is not running", jobID)
}

func (a *App) GetJob(ctx context.Context, jobID string) (*domain.Job, error) {
	job, err := a.Store.GetJob(ctx, userIDFrom(ctx), jobID)
	return safeJob(job), err
}

func (a *App) ListJobs(ctx context.Context, limit int) ([]domain.Job, error) {
	jobs, err := a.Store.ListJobs(ctx, userIDFrom(ctx), limit)
	for i := range jobs {
		jobs[i] = *safeJob(&jobs[i])
	}
	return listResult(jobs, err)
}

func (a *App) runSync(ctx context.Context, job *domain.Job, acc *domain.MailAccount, opts SyncOptions) {
	ctx, span := telemetry.Start(ctx, "sync.run",
		telemetry.Attr("account.id", acc.ID), telemetry.Attr("inbound.protocol", acc.InboundProtocol))
	defer span.End()
	stats := domain.SyncStats{}
	// Sent mail is counted apart, so "new" still means new mail received.
	sentStats := domain.SyncStats{}
	// diag accumulates where this sync went, so its outcome can be explained
	// step by step instead of as one word. It is filled in as the sync
	// proceeds and attached to the job by finish.
	protocol := "pop3"
	if acc.InboundProtocol == domain.InboundIMAP {
		protocol = "imap"
	}
	diag := domain.SyncDiagnostic{Protocol: protocol, Host: acc.POP3Host, Port: acc.POP3Port, Security: string(acc.POP3Security)}
	finish := func(status domain.JobStatus, errMsg string) {
		job.Status = status
		job.Error = jobDiagnostic(job.Type, status, errMsg)
		job.Diagnostic = safeSyncDiagnostic(&diag)
		job.Stats = map[string]int64{
			"seen": stats.Seen, "new": stats.New, "duplicate": stats.Duplicate,
			"failed": stats.Failed, "oversize": stats.Oversize, "parse_error": stats.ParseError,
		}
		if sentStats.Seen > 0 {
			job.Stats["sent_seen"], job.Stats["sent_new"] = sentStats.Seen, sentStats.New
			job.Stats["sent_duplicate"], job.Stats["sent_failed"] = sentStats.Duplicate, sentStats.Failed
		}
		_ = a.Store.UpdateJob(context.Background(), job)
		metrics.SyncTotal.WithLabelValues(string(status)).Inc()
		metrics.MessagesFetched.Add(float64(stats.New))
		a.audit(context.Background(), "sync_finish", "account:"+acc.ID, string(status),
			fmt.Sprintf("job:%s new=%d dup=%d failed=%d", job.ID, stats.New, stats.Duplicate, stats.Failed))
		if status == domain.JobFailed || status == domain.JobPartial {
			severity := domain.SeverityError
			if status == domain.JobPartial {
				severity = domain.SeverityWarning
			}
			// The detail is Postra's own rendering of the diagnostic: the
			// failing step, its timings, the connections held — what an
			// operator needs before opening the job.
			a.recordIncident(severity, "sync", job.Error, diagnosticDetail(job.Diagnostic),
				withIncidentAccount(acc.ID), withIncidentJob(job.ID))
		}
	}

	defer func() {
		if r := recover(); r != nil {
			slog.Error("runSync caught panic", "account", acc.ID, "job", job.ID)
			finish(domain.JobFailed, providerUnexpected)
		}
	}()

	// Heartbeat: keep updated_at fresh for the whole run — including the time
	// spent queued on the concurrency semaphore and the long dial + mailbox-
	// enumeration phase (no per-message updates there) — so a leader-election
	// flap or the periodic reaper never mistakes this live sync for an
	// abandoned one. Stops when the sync returns. TouchJob updates queued jobs
	// too, so a job waiting for a slot stays fresh.
	hbCtx, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				a.guard("sync-heartbeat", func() { _ = a.Store.TouchJob(context.Background(), job.ID) })
			}
		}
	}()

	// Bound concurrent syncs so a scheduler fan-out over many accounts (plus
	// IMAP IDLE triggers) doesn't buffer many whole messages at once and OOM
	// the container (which K8s then restarts, orphaning this very job).
	slotStart := time.Now()
	if err := a.acquireSyncSlot(ctx); err != nil {
		diag.SlotWaitMS = durationMS(time.Since(slotStart))
		finish(domain.JobCancelled, "cancelled")
		return
	}
	defer a.releaseSyncSlot()
	diag.SlotWaitMS = durationMS(time.Since(slotStart))

	job.Status = domain.JobRunning
	_ = a.Store.UpdateJob(ctx, job)

	// Count this session while it is open: the next connect failure can then
	// say whether Postra itself was already using the server's per-account
	// connection allowance (IDLE holds one of its own).
	releaseSession := a.inbound.hold(acc)
	defer releaseSession()

	sess, err := a.dialInboundForSync(ctx, acc, &diag)
	if err != nil {
		var authErr *domain.AuthError
		if errors.As(err, &authErr) {
			// POP-011: no endless retries on bad credentials. Only a refusal
			// of these credentials reaches here; a busy or throttled server is
			// a transient failure and leaves the account active.
			_ = a.Store.SetAccountStatus(context.Background(), acc.UserID, acc.ID, domain.AccountCredentialError)
			diag.Stage = domain.StageLogin
			finish(domain.JobFailed, providerAuthFailed)
			return
		}
		finish(domain.JobFailed, syncFailure(&diag, err, providerDiagnostic(err)))
		return
	}
	defer sess.Close()

	// Prefer UIDL as the dedup checkpoint (POP-004); fall back to LIST +
	// content-derived IDs when the server lacks UIDL (POP-005/008).
	remote, err := sess.UIDL(ctx)
	uidlSupported := err == nil
	if !uidlSupported {
		remote, err = sess.List(ctx)
		if err != nil {
			syncDiagnosticStage(&diag, domain.StageEnumerate, err)
			finish(domain.JobFailed, syncFailure(&diag, err, providerListFailed))
			return
		}
	} else {
		// merge sizes for the oversize check
		if listed, lerr := sess.List(ctx); lerr == nil {
			sizes := map[int]int64{}
			for _, m := range listed {
				sizes[m.Number] = m.Size
			}
			for i := range remote {
				remote[i].Size = sizes[remote[i].Number]
			}
		}
	}

	// Reverse remote so newest messages (higher sequence numbers) are ingested first.
	for i, j := 0, len(remote)-1; i < j; i, j = i+1, j-1 {
		remote[i], remote[j] = remote[j], remote[i]
	}

	// Body-repair mode re-fetches only messages whose stored body is missing or
	// undecryptable and rewrites them in place — no new ingestion.
	if opts.RepairBodies {
		a.runBodyRepair(ctx, sess, acc, remote, job, &stats)
		_ = sess.Quit(ctx)
		finish(domain.JobSucceeded, "")
		return
	}

	maxN := a.EffectiveConfig().Sync.MaxPerSync
	if opts.FullSync || opts.MaxMessages < 0 {
		maxN = 0
	} else if opts.MaxMessages > 0 {
		maxN = opts.MaxMessages
	}

	fetched := 0
	attempted := int64(0)
	var ingestErr error
	for _, rm := range remote {
		if ctx.Err() != nil {
			finish(domain.JobCancelled, "cancelled")
			return
		}
		if maxN > 0 && fetched >= maxN {
			break
		}

		stats.Seen++
		if uidlSupported && rm.UIDL != "" {
			dup, err := a.Store.HasCheckpoint(ctx, acc.ID, rm.UIDL)
			if err == nil && dup {
				stats.Duplicate++
				continue
			}
		}
		if maxBytes := a.EffectiveConfig().Sync.MaxMessageBytes; maxBytes > 0 && rm.Size > maxBytes {
			stats.Oversize++
			continue
		}
		attempted++
		if err := a.ingestOne(ctx, sess, acc, rm, uidlSupported, &stats, domain.MailboxInbox); err != nil {
			stats.Failed++
			ingestErr = err
			continue
		}
		fetched++
		if fetched%20 == 0 {
			runtime.GC()
		}
		job.Progress = fmt.Sprintf("%d/%d", fetched, len(remote))
		_ = a.Store.UpdateJob(ctx, job)
	}
	sentOutcome, sentErr := a.sentFolderPass(ctx, sess, acc, job, &sentStats, maxN, opts)
	if sentOutcome == sentCancelled {
		finish(domain.JobCancelled, "cancelled")
		return
	}
	diag.Sent = sentOutcome
	if sentErr != nil {
		sentDiag := domain.SyncDiagnostic{}
		syncDiagnosticStage(&sentDiag, domain.StageSelect, sentErr)
		diag.SentStage, diag.SentClass = sentDiag.Stage, sentDiag.Class
	}
	_ = sess.Quit(ctx)
	// Every message this sync tried to fetch failed: reporting that as a
	// success hides the outage the operator has to act on. A sync that got
	// some of them through is partial, not clean.
	if stats.Failed > 0 {
		syncDiagnosticStage(&diag, domain.StageFetch, ingestErr)
		if stats.Failed == attempted && sentStats.New == 0 {
			finish(domain.JobFailed, syncFailure(&diag, ingestErr, providerSyncFailed))
			return
		}
		finish(domain.JobPartial, syncFailure(&diag, ingestErr, providerSyncFailed))
		return
	}
	finish(domain.JobSucceeded, "")
}

// sentFolderPass runs the Sent-folder pass when it is due and names how it
// ended. It never fails the sync: received mail is synced either way, and an
// account whose server refuses the Sent folder must still get its inbox.
func (a *App) sentFolderPass(ctx context.Context, sess domain.POP3Session, acc *domain.MailAccount,
	job *domain.Job, stats *domain.SyncStats, maxN int, opts SyncOptions) (outcome string, err error) {
	sent, ok := sess.(domain.SentFolderCapable)
	if !ok {
		return "", nil // POP3: there is no Sent folder to read
	}
	if !a.sentFolderDue(acc.ID, opts) {
		return sentThrottled, nil
	}
	return a.syncSentFolder(ctx, sess, sent, acc, job, stats, maxN)
}

// sentFolderDue reports whether this sync reads the Sent folder, and claims
// the pass. Enumerating it costs one FETCH per 2,000 messages, which on every
// IDLE wake-up — each new inbox message — would double the cost of real-time
// sync for no gain: IDLE cannot see Sent. Other syncs always read it.
func (a *App) sentFolderDue(accountID string, opts SyncOptions) bool {
	now := time.Now()
	if opts.fromIdle {
		if last, ok := a.sentSyncedAt.Load(accountID); ok && now.Sub(last.(time.Time)) < sentFolderIdleInterval {
			return false
		}
	}
	a.sentSyncedAt.Store(accountID, now)
	return true
}

// sentCheckpointPrefix namespaces Sent-folder UIDs in the dedup checkpoints.
// Each IMAP folder has its own UID space, and two folders may even share a
// UIDVALIDITY, so an unprefixed Sent UID could collide with an INBOX one and
// be skipped as already seen.
const sentCheckpointPrefix = "sent:"

// How the Sent-folder pass ended. sentCancelled is internal to the sync: the
// job becomes cancelled, so it is never stored as an outcome.
const (
	sentSynced    = "synced"
	sentThrottled = "throttled"
	sentNone      = "none"
	sentFailed    = "failed"
	sentCancelled = "cancelled"
)

// syncSentFolder ingests the account's Sent folder after the inbox, on the
// same session. It is best-effort: a server without a Sent folder, or one
// that refuses it, never fails the inbox sync. It names how the pass ended and
// returns the failure that ended it, for the sync's diagnostic.
func (a *App) syncSentFolder(ctx context.Context, sess domain.POP3Session, sent domain.SentFolderCapable,
	acc *domain.MailAccount, job *domain.Job, stats *domain.SyncStats, maxN int) (string, error) {
	name, err := sent.SentMailbox(ctx)
	if err != nil {
		slog.Warn("sync: sent folder lookup failed; received mail was synced", "account", acc.ID, "err", providerDiagnostic(err))
		return sentFailed, err
	}
	if name == "" {
		return sentNone, nil
	}
	if err := sent.SelectMailbox(name); err != nil {
		slog.Warn("sync: sent folder could not be opened; received mail was synced", "account", acc.ID, "err", providerDiagnostic(err))
		return sentFailed, err
	}
	remote, err := sess.UIDL(ctx)
	if err != nil {
		slog.Warn("sync: sent folder could not be listed; received mail was synced", "account", acc.ID, "err", providerDiagnostic(err))
		return sentFailed, err
	}
	// Newest first, as for the inbox.
	for i, j := 0, len(remote)-1; i < j; i, j = i+1, j-1 {
		remote[i], remote[j] = remote[j], remote[i]
	}
	fetched := 0
	for _, rm := range remote {
		if ctx.Err() != nil {
			return sentCancelled, nil
		}
		if maxN > 0 && fetched >= maxN {
			break
		}
		if rm.UIDL == "" {
			continue // IMAP always has UIDs; nothing stable to dedup on otherwise
		}
		stats.Seen++
		rm.UIDL = sentCheckpointPrefix + rm.UIDL
		if dup, err := a.Store.HasCheckpoint(ctx, acc.ID, rm.UIDL); err == nil && dup {
			stats.Duplicate++
			continue
		}
		if maxBytes := a.EffectiveConfig().Sync.MaxMessageBytes; maxBytes > 0 && rm.Size > maxBytes {
			stats.Oversize++
			continue
		}
		if err := a.ingestOne(ctx, sess, acc, rm, true, stats, domain.MailboxSent); err != nil {
			stats.Failed++
			continue
		}
		fetched++
		if fetched%20 == 0 {
			runtime.GC()
		}
		job.Progress = fmt.Sprintf("sent %d/%d", fetched, len(remote))
		_ = a.Store.UpdateJob(ctx, job)
	}
	return sentSynced, nil
}

// runBodyRepair re-fetches messages whose stored body is missing/undecryptable
// and rewrites the body in place. It reuses the freshly enumerated mailbox
// (seq→uidl) so no UID-FETCH is needed, and preserves message identity so
// labels, collaboration, and analyses survive the repair.
func (a *App) runBodyRepair(ctx context.Context, sess domain.POP3Session, acc *domain.MailAccount,
	remote []domain.RemoteMessage, job *domain.Job, stats *domain.SyncStats) {
	repairSet, err := a.Store.UIDLsNeedingBodyRepair(ctx, acc.ID)
	if err != nil {
		slog.Error("body repair: list failed", "account", acc.ID, "err", err)
		return
	}
	if len(repairSet) == 0 {
		slog.Info("body repair: nothing to repair", "account", acc.ID)
		return
	}
	slog.Info("body repair: candidates", "account", acc.ID, "count", len(repairSet))
	done := 0
	for _, rm := range remote {
		if ctx.Err() != nil {
			return
		}
		msgID, ok := repairSet[rm.UIDL]
		if !ok {
			continue
		}
		stats.Seen++
		raw, ferr := a.fetchRaw(ctx, sess, rm.Number)
		if ferr != nil {
			stats.Failed++
			continue
		}
		parsed := mailparse.Parse(raw)
		body := &domain.MessageBody{
			MessageID: msgID, TextBody: parsed.TextBody,
			HTMLSanitized: parsed.HTMLSafe, Charset: parsed.Charset,
		}
		if uerr := a.Store.UpdateMessageBody(ctx, msgID, body); uerr != nil {
			slog.Warn("body repair: update failed", "message", msgID, "err", uerr)
			stats.Failed++
			continue
		}
		done++
		job.Progress = fmt.Sprintf("repair %d/%d", done, len(repairSet))
		_ = a.Store.UpdateJob(ctx, job)
	}
	stats.Duplicate = int64(len(repairSet) - done) // untouched candidates (not found on server)
	slog.Info("body repair: done", "account", acc.ID, "repaired", done, "candidates", len(repairSet))
}

// rawReadLimit turns MaxMessageBytes into the io.LimitReader count one message
// body is read under. The extra byte is what lets the caller tell a message
// exactly at the limit from one over it.
//
// A limit of zero or less turns the size cap off — the convention this setting
// carries everywhere else (the oversize checks below and in runSync only
// compare when maxBytes > 0, and the IMAP adapter treats maxLiteral <= 0 as no
// literal cap). io.LimitReader has no value meaning "unbounded", and the naive
// maxBytes+1 becomes 1 at zero and non-positive below it, which silently
// truncates every message on such an account to a single byte or to nothing at
// all; the oversize check cannot catch it because the cap is off, so the stub
// is stored as if it were the mail. MaxInt64 stands in for unbounded, and it
// also absorbs the overflow of +1 at the top of the range.
func rawReadLimit(maxBytes int64) int64 {
	if maxBytes <= 0 || maxBytes == math.MaxInt64 {
		return math.MaxInt64
	}
	return maxBytes + 1
}

// fetchRaw downloads one message's raw bytes, bounded by MaxMessageBytes.
func (a *App) fetchRaw(ctx context.Context, sess domain.POP3Session, number int) ([]byte, error) {
	rc, err := sess.Retrieve(ctx, number)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, rawReadLimit(a.EffectiveConfig().Sync.MaxMessageBytes)))
}

// ingestOne downloads, stores, and indexes a single message. Each message is
// committed independently so an interruption never loses or duplicates
// already-stored mail (POP-012, POP-015).
func (a *App) ingestOne(ctx context.Context, sess domain.POP3Session, acc *domain.MailAccount,
	rm domain.RemoteMessage, uidlSupported bool, stats *domain.SyncStats, mailbox string) (err error) {

	var raw []byte
	var parsed *mailparse.Parsed
	defer func() {
		raw = nil
		parsed = nil
		if r := recover(); r != nil {
			slog.Error("ingestOne caught panic", "account", acc.ID)
			err = errors.New(providerUnexpected)
		}
	}()

	rc, err := sess.Retrieve(ctx, rm.Number)
	if err != nil {
		return err
	}
	maxBytes := a.EffectiveConfig().Sync.MaxMessageBytes
	raw, err = io.ReadAll(io.LimitReader(rc, rawReadLimit(maxBytes)))
	rc.Close()
	if err != nil {
		return err
	}
	if maxBytes > 0 && int64(len(raw)) > maxBytes {
		stats.Oversize++
		return nil
	}
	sum := sha256.Sum256(raw)
	rawHash := hex.EncodeToString(sum[:])

	// Content-hash dedup catches UIDL-less servers and UIDL churn.
	if dup, _ := a.Store.IsDuplicateHash(ctx, acc.ID, rawHash); dup {
		stats.Duplicate++
		if uidlSupported && rm.UIDL != "" {
			_ = a.Store.AddCheckpoint(ctx, acc.ID, rm.UIDL, "")
		}
		return nil
	}

	parsed = mailparse.Parse(raw)
	if parsed.ParseError != "" {
		stats.ParseError++ // partial result still stored (MIME-004)
	}
	// A message sent through Postra was recorded when it was sent; the
	// server's Sent copy of it carries the same Message-ID, not the same bytes.
	if mailbox == domain.MailboxSent {
		if dup, _ := a.Store.HasSentCopy(ctx, acc.ID, parsed.MessageID); dup {
			stats.Duplicate++
			_ = a.Store.AddCheckpoint(ctx, acc.ID, rm.UIDL, "")
			return nil
		}
	}

	uidl := rm.UIDL
	if uidl == "" {
		// POP-005 fallback identity: stable content-derived key.
		fb := sha256.Sum256([]byte(parsed.MessageID + "|" + parsed.From.Email + "|" +
			parsed.Date.String() + "|" + fmt.Sprint(len(raw)) + "|" + rawHash))
		uidl = "fb_" + hex.EncodeToString(fb[:16])
		if dup, _ := a.Store.HasCheckpoint(ctx, acc.ID, uidl); dup {
			stats.Duplicate++
			return nil
		}
	}

	msg, body, duplicate, err := a.storeMessage(ctx, acc, raw, rawHash, parsed, uidl, mailbox)
	if err != nil {
		return err
	}
	if duplicate {
		stats.Duplicate++
		return nil
	}
	stats.New++
	// Apply the user's automation rules to the freshly ingested message
	// (§자동화 메일 규칙 엔진). Best-effort: never fails the sync. Rules are
	// about mail received; sent mail never triggers them.
	if mailbox != domain.MailboxSent {
		a.evaluateRulesOnIngest(ctx, msg, body)
	}
	return nil
}

// storeMessage keeps one parsed message — the raw bytes, its thread, its
// attachments after policy scanning, and the dedup checkpoint under uidl.
// Sync and the sent-copy record share it. duplicate reports a message the
// store already held.
func (a *App) storeMessage(ctx context.Context, acc *domain.MailAccount, raw []byte, rawHash string,
	parsed *mailparse.Parsed, uidl, mailbox string) (msg *domain.Message, body *domain.MessageBody, duplicate bool, err error) {
	rawURI, _, _, err := a.Objects.Put("raw", bytes.NewReader(raw))
	if err != nil {
		return nil, nil, false, err
	}

	subjectKey := mailparse.SubjectKey(parsed.Subject)
	refs := mailparse.ReferenceIDs(parsed.References, parsed.InReplyTo)
	threadID, err := a.Store.ResolveThread(ctx, acc.UserID, acc.ID, refs, subjectKey, parsed.Date.Unix())
	if err != nil {
		threadID = ""
	}

	msg = &domain.Message{
		ID: persistence.NewID("msg"), UserID: acc.UserID, AccountID: acc.ID,
		UIDL: uidl, MessageID: parsed.MessageID, Subject: parsed.Subject,
		From: parsed.From, To: parsed.To, Cc: parsed.Cc, ReplyTo: parsed.ReplyTo, Bcc: parsed.Bcc,
		Date: parsed.Date.Unix(), Size: int64(len(raw)),
		RawHash: rawHash, RawURI: rawURI, ThreadID: threadID,
		HasAttachments: len(parsed.Attachments) > 0,
		InReplyTo:      parsed.InReplyTo, References: parsed.References,
		AuthResults: parsed.AuthResults, ParseError: parsed.ParseError,
		Mailbox: mailbox,
		// The owner wrote it: sent mail is never unread.
		IsRead: mailbox == domain.MailboxSent,
	}
	body = &domain.MessageBody{
		MessageID: msg.ID, TextBody: parsed.TextBody,
		HTMLSanitized: parsed.HTMLSafe, Charset: parsed.Charset,
	}
	var atts []domain.Attachment
	for i := range parsed.Attachments {
		ap := &parsed.Attachments[i]
		// Policy + archive scan before retention (MIME-011/012/015).
		verdict := a.ScanAttachment(ctx, domain.ScanInput{
			Name: ap.Name, MIMEType: ap.MIMEType, Data: ap.Data,
		})
		at := domain.Attachment{
			ID: persistence.NewID("att"), MessageID: msg.ID,
			Name: ap.Name, MIMEType: ap.MIMEType, Size: int64(len(ap.Data)),
			Inline: ap.Inline, ScanStatus: verdict.Status, ScanDetail: verdict.Detail,
		}
		if verdict.StoreContent {
			uri, hash, _, err := a.Objects.Put("att", bytes.NewReader(ap.Data))
			if err == nil {
				at.StorageURI, at.Hash = uri, hash
			}
		} else {
			// Dangerous content (blocked extension / zip bomb) is recorded
			// but never retained (§13 악성 첨부).
			a.audit(ctx, "attachment_blocked", "message:"+msg.ID, "ok",
				fmt.Sprintf("%s: %s", ap.Name, verdict.Detail))
		}
		ap.Data = nil // release attachment memory immediately
		atts = append(atts, at)
	}
	if err := a.Store.InsertMessage(ctx, msg, body, atts); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			_ = a.Store.AddCheckpoint(ctx, acc.ID, uidl, msg.ID)
			return nil, nil, true, nil
		}
		return nil, nil, false, err
	}
	_ = a.Store.AddCheckpoint(ctx, acc.ID, uidl, msg.ID)
	return msg, body, false, nil
}

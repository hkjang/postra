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
	"runtime"
	"runtime/debug"
	"strings"

	"postra/internal/adapters/mailparse"
	"postra/internal/adapters/persistence"
	"postra/internal/domain"
	"postra/internal/platform/metrics"
	"postra/internal/platform/telemetry"
)

type SyncOptions struct {
	MaxMessages int  `json:"max_messages,omitempty"`
	FullSync    bool `json:"full_sync,omitempty"`
	// DeleteAfterFetch is intentionally absent from the MVP sync path:
	// server-side deletion is a separate, approval-gated flow (§5.2).
}

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

	job := &domain.Job{
		ID: persistence.NewID("job"), UserID: userID,
		Type: "sync", AccountID: accountID, Status: domain.JobQueued,
	}
	if err := a.Store.CreateJob(ctx, job); err != nil {
		a.syncLocks.Delete(accountID)
		return nil, err
	}
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
				slog.Error("sync worker panic", "account", accountID, "job", job.ID, "panic", r)
			}
		}()
		defer a.workerGroup.Done()
		defer a.syncLocks.Delete(accountID)
		defer a.jobCancels.Delete(job.ID)
		a.runSync(jobCtx, job, acc, opts)
	}()
	return job, nil
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
	return a.Store.GetJob(ctx, userIDFrom(ctx), jobID)
}

func (a *App) ListJobs(ctx context.Context, limit int) ([]domain.Job, error) {
	return a.Store.ListJobs(ctx, userIDFrom(ctx), limit)
}

func (a *App) runSync(ctx context.Context, job *domain.Job, acc *domain.MailAccount, opts SyncOptions) {
	ctx, span := telemetry.Start(ctx, "sync.run",
		telemetry.Attr("account.id", acc.ID), telemetry.Attr("inbound.protocol", acc.InboundProtocol))
	defer span.End()
	stats := domain.SyncStats{}
	finish := func(status domain.JobStatus, errMsg string) {
		job.Status = status
		job.Error = errMsg
		job.Stats = map[string]int64{
			"seen": stats.Seen, "new": stats.New, "duplicate": stats.Duplicate,
			"failed": stats.Failed, "oversize": stats.Oversize, "parse_error": stats.ParseError,
		}
		_ = a.Store.UpdateJob(context.Background(), job)
		metrics.SyncTotal.WithLabelValues(string(status)).Inc()
		metrics.MessagesFetched.Add(float64(stats.New))
		a.audit(context.Background(), "sync_finish", "account:"+acc.ID, string(status),
			fmt.Sprintf("job:%s new=%d dup=%d failed=%d", job.ID, stats.New, stats.Duplicate, stats.Failed))
	}

	defer func() {
		if r := recover(); r != nil {
			errStr := fmt.Sprintf("panic during sync: %v", r)
			slog.Error("runSync caught panic", "account", acc.ID, "job", job.ID, "panic", r)
			finish(domain.JobFailed, errStr)
		}
	}()

	job.Status = domain.JobRunning
	_ = a.Store.UpdateJob(ctx, job)

	sess, err := a.dialInbound(ctx, acc, domain.PurposePOP3Auth)
	if err != nil {
		var authErr *domain.AuthError
		if errors.As(err, &authErr) {
			// POP-011: no endless retries on bad credentials.
			_ = a.Store.SetAccountStatus(context.Background(), acc.UserID, acc.ID, domain.AccountCredentialError)
			finish(domain.JobFailed, "authentication failed; account moved to credential_error")
			return
		}
		finish(domain.JobFailed, "connect failed: "+err.Error())
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
			finish(domain.JobFailed, "LIST failed: "+err.Error())
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

	maxN := a.Cfg.Sync.MaxPerSync
	if opts.FullSync || opts.MaxMessages < 0 {
		maxN = 0
	} else if opts.MaxMessages > 0 {
		maxN = opts.MaxMessages
	}

	fetched := 0
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
		if a.Cfg.Sync.MaxMessageBytes > 0 && rm.Size > a.Cfg.Sync.MaxMessageBytes {
			stats.Oversize++
			continue
		}
		if err := a.ingestOne(ctx, sess, acc, rm, uidlSupported, &stats); err != nil {
			stats.Failed++
			continue
		}
		fetched++
		if rm.Size > 1<<20 || fetched%2 == 0 {
			runtime.GC()
			debug.FreeOSMemory()
		}
		job.Progress = fmt.Sprintf("%d/%d", fetched, len(remote))
		_ = a.Store.UpdateJob(ctx, job)
	}
	runtime.GC()
	debug.FreeOSMemory()
	_ = sess.Quit(ctx)
	finish(domain.JobSucceeded, "")
}

// ingestOne downloads, stores, and indexes a single message. Each message is
// committed independently so an interruption never loses or duplicates
// already-stored mail (POP-012, POP-015).
func (a *App) ingestOne(ctx context.Context, sess domain.POP3Session, acc *domain.MailAccount,
	rm domain.RemoteMessage, uidlSupported bool, stats *domain.SyncStats) (err error) {

	var raw []byte
	var parsed *mailparse.Parsed
	defer func() {
		raw = nil
		parsed = nil
		if r := recover(); r != nil {
			slog.Error("ingestOne caught panic", "account", acc.ID, "uidl", rm.UIDL, "panic", r)
			err = fmt.Errorf("ingest panic: %v", r)
		}
	}()

	rc, err := sess.Retrieve(ctx, rm.Number)
	if err != nil {
		return err
	}
	raw, err = io.ReadAll(io.LimitReader(rc, a.Cfg.Sync.MaxMessageBytes+1))
	rc.Close()
	if err != nil {
		return err
	}
	if a.Cfg.Sync.MaxMessageBytes > 0 && int64(len(raw)) > a.Cfg.Sync.MaxMessageBytes {
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

	rawURI, _, _, err := a.Objects.Put("raw", bytes.NewReader(raw))
	if err != nil {
		return err
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

	subjectKey := mailparse.SubjectKey(parsed.Subject)
	refs := mailparse.ReferenceIDs(parsed.References, parsed.InReplyTo)
	threadID, err := a.Store.ResolveThread(ctx, acc.UserID, acc.ID, refs, subjectKey, parsed.Date.Unix())
	if err != nil {
		threadID = ""
	}

	msg := &domain.Message{
		ID: persistence.NewID("msg"), UserID: acc.UserID, AccountID: acc.ID,
		UIDL: uidl, MessageID: parsed.MessageID, Subject: parsed.Subject,
		From: parsed.From, To: parsed.To, Cc: parsed.Cc, ReplyTo: parsed.ReplyTo,
		Date: parsed.Date.Unix(), Size: int64(len(raw)),
		RawHash: rawHash, RawURI: rawURI, ThreadID: threadID,
		HasAttachments: len(parsed.Attachments) > 0,
		InReplyTo:      parsed.InReplyTo, References: parsed.References,
		AuthResults: parsed.AuthResults, ParseError: parsed.ParseError,
	}
	body := &domain.MessageBody{
		MessageID: msg.ID, TextBody: parsed.TextBody,
		HTMLSanitized: parsed.HTMLSafe, Charset: parsed.Charset,
	}
	var atts []domain.Attachment
	for i := range parsed.Attachments {
		ap := &parsed.Attachments[i]
		// Policy + archive scan before retention (MIME-011/012/015).
		verdict := a.Scanner.Scan(ctx, domain.ScanInput{
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
			stats.Duplicate++
			_ = a.Store.AddCheckpoint(ctx, acc.ID, uidl, msg.ID)
			return nil
		}
		return err
	}
	_ = a.Store.AddCheckpoint(ctx, acc.ID, uidl, msg.ID)
	stats.New++
	// Apply the user's automation rules to the freshly ingested message
	// (§자동화 메일 규칙 엔진). Best-effort: never fails the sync.
	a.evaluateRulesOnIngest(ctx, msg, body)
	return nil
}

package application

import (
	"context"

	"postra/internal/domain"
)

// Storage is the persistence port. The application layer depends only on this
// interface, so the SQLite adapter (personal/embedded mode) and the
// PostgreSQL adapter (server/multi-user mode) are interchangeable (§2 공급자
// 독립성, §24 확장). Concrete implementations live in internal/adapters.
type Storage interface {
	// Ping verifies the backend is reachable (readiness probe).
	Ping(ctx context.Context) error

	// System incident tracking (major errors captured for admin reporting).
	domain.IncidentStore

	EnsureUser(ctx context.Context, id, loginID string) error
	CreateUser(ctx context.Context, user *domain.User, passwordHash string) error
	GetUser(ctx context.Context, id string) (*domain.User, error)
	GetUserByLogin(ctx context.Context, loginID string) (*domain.User, string, error)
	GetUserByOIDC(ctx context.Context, issuer, subject string) (*domain.User, error)
	// GetUserByEmail returns the single active user with this email, or
	// ErrNotFound when there is no match or the match is ambiguous (used for
	// safe OIDC account linking).
	GetUserByEmail(ctx context.Context, email string) (*domain.User, error)
	ListUsers(ctx context.Context) ([]domain.User, error)
	UpdateUser(ctx context.Context, user *domain.User) error
	SetUserPassword(ctx context.Context, userID, passwordHash string) error
	CountAdmins(ctx context.Context) (int, error)
	CreateSession(ctx context.Context, session *domain.Session) error
	GetSessionByTokenHash(ctx context.Context, tokenHash string) (*domain.Session, *domain.User, error)
	TouchSession(ctx context.Context, id string, lastSeen int64) error
	DeleteSession(ctx context.Context, id string) error
	DeleteUserSessions(ctx context.Context, userID string) error
	GetSettings(ctx context.Context) (map[string]string, error)
	UpsertSettings(ctx context.Context, values map[string]string) error
	GetOrCreateSetting(ctx context.Context, key, candidate string) (string, error)

	CreateMCPKey(ctx context.Context, key *domain.MCPKey) error
	GetMCPKeyByHash(ctx context.Context, keyHash string) (*domain.MCPKey, *domain.User, error)
	ListMCPKeys(ctx context.Context, userID string) ([]domain.MCPKey, error)
	ListAllMCPKeys(ctx context.Context) ([]domain.MCPKey, error)
	RevokeMCPKey(ctx context.Context, userID, keyID string) error
	TouchMCPKey(ctx context.Context, keyID string, lastUsedAt int64) error

	CreateAccount(ctx context.Context, a *domain.MailAccount) error
	GetAccount(ctx context.Context, userID, id string) (*domain.MailAccount, error)
	ListAccounts(ctx context.Context, userID string) ([]domain.MailAccount, error)
	UpdateAccount(ctx context.Context, a *domain.MailAccount) error
	SetAccountStatus(ctx context.Context, userID, id string, st domain.AccountStatus) error

	PutCredentialRef(ctx context.Context, c domain.CredentialRef) error
	TouchCredential(ctx context.Context, ref domain.SecretRef)
	SetCredentialStatus(ctx context.Context, ref domain.SecretRef, status string) error

	// Secret envelopes: the shared, DB-backed secret value store (survives pod
	// restarts and is visible to every replica, unlike a per-node local file).
	PutSecretEnvelope(ctx context.Context, sec domain.StoredSecret) error
	GetSecretEnvelope(ctx context.Context, ref string) (domain.StoredSecret, error)
	MarkSecretEnvelopeRevoked(ctx context.Context, ref string) error
	ListSecretEnvelopes(ctx context.Context) ([]domain.StoredSecret, error)

	// Object blobs: the shared, DB-backed object store (raw MIME + attachment
	// bodies), so they survive restarts and are readable from every replica.
	PutObject(ctx context.Context, kind, name string, blob []byte) error
	GetObject(ctx context.Context, kind, name string) ([]byte, error)
	DeleteObject(ctx context.Context, kind, name string) error
	WalkObjects(ctx context.Context, fn func(kind, name string, blob []byte) error) error
	OverwriteObject(ctx context.Context, kind, name string, blob []byte) error

	HasCheckpoint(ctx context.Context, accountID, uidl string) (bool, error)
	AddCheckpoint(ctx context.Context, accountID, uidl, messageID string) error
	StoredUIDLs(ctx context.Context, accountID string) (map[string]bool, error)

	InsertMessage(ctx context.Context, m *domain.Message, body *domain.MessageBody, atts []domain.Attachment) error
	IsDuplicateHash(ctx context.Context, accountID, rawHash string) (bool, error)
	DeleteMessage(ctx context.Context, userID, id string) ([]string, error)
	GetMessage(ctx context.Context, userID, id string) (*domain.Message, error)
	GetBody(ctx context.Context, userID, messageID string) (*domain.MessageBody, error)
	// UIDLsNeedingBodyRepair lists uidl→messageID for messages whose body is
	// missing/empty/undecryptable, and UpdateMessageBody rewrites a body (and
	// search index) in place — together they power body-repair re-sync.
	UIDLsNeedingBodyRepair(ctx context.Context, accountID string) (map[string]string, error)
	UpdateMessageBody(ctx context.Context, messageID string, body *domain.MessageBody) error
	ListAttachments(ctx context.Context, userID, messageID string) ([]domain.Attachment, error)
	Search(ctx context.Context, q domain.SearchQuery) (*domain.SearchResult, error)
	UpdateMessage(ctx context.Context, m *domain.Message) error

	ResolveThread(ctx context.Context, userID, accountID string, refs []string, subjectKey string, date int64) (string, error)
	GetThreadMessages(ctx context.Context, userID, threadID string) ([]domain.Message, error)

	CreateRule(ctx context.Context, r *domain.MailRule) error
	UpdateRule(ctx context.Context, r *domain.MailRule) error
	DeleteRule(ctx context.Context, userID, id string) error
	GetRule(ctx context.Context, userID, id string) (*domain.MailRule, error)
	ListRules(ctx context.Context, userID string) ([]domain.MailRule, error)

	CreateActionCard(ctx context.Context, c *domain.ActionCard) error
	UpdateActionCard(ctx context.Context, c *domain.ActionCard) error
	GetActionCard(ctx context.Context, userID, id string) (*domain.ActionCard, error)
	ListActionCards(ctx context.Context, userID, status string, limit int) ([]domain.ActionCard, error)

	UpsertMessageCollab(ctx context.Context, mc *domain.MessageCollab) error
	GetMessageCollab(ctx context.Context, userID, messageID string) (*domain.MessageCollab, error)
	ListMessageCollab(ctx context.Context, userID, status, assignee string, limit int) ([]domain.MessageCollab, error)
	AddMessageNote(ctx context.Context, n *domain.MessageNote) error
	ListMessageNotes(ctx context.Context, userID, messageID string) ([]domain.MessageNote, error)

	SaveAnalysis(ctx context.Context, a *domain.Analysis) error
	FindCachedAnalysis(ctx context.Context, userID, analysisType, inputHash, model string) (*domain.Analysis, error)

	CreateDraft(ctx context.Context, d *domain.Draft, v *domain.DraftVersion) error
	AddDraftVersion(ctx context.Context, userID, draftID string, v *domain.DraftVersion) (int, error)
	GetDraft(ctx context.Context, userID, id string) (*domain.Draft, *domain.DraftVersion, error)
	GetDraftVersion(ctx context.Context, userID, draftID string, version int) (*domain.DraftVersion, error)
	SetDraftStatus(ctx context.Context, userID, id string, st domain.DraftStatus) error

	InsertApproval(ctx context.Context, id, userID, actionType, draftID string, draftVersion int, payloadHash, tokenHash, approver string, expiresAt int64) error
	ConsumeApproval(ctx context.Context, tokenHash, payloadHash string) (string, int, error)

	CreateOutbound(ctx context.Context, o *domain.OutboundMessage) error
	GetOutboundByIdemKey(ctx context.Context, userID, key string) (*domain.OutboundMessage, error)
	GetOutbound(ctx context.Context, userID, id string) (*domain.OutboundMessage, error)
	CountSentSince(ctx context.Context, userID, accountID string, since int64) (int, error)
	UpdateOutbound(ctx context.Context, id string, status domain.OutboundStatus, smtpResponse string, attempts int) error
	MarkOutboundRetry(ctx context.Context, id, smtpResponse string, attempts int, nextAttemptAt int64) error
	ListDueRetries(ctx context.Context, nowTS int64, limit int) ([]domain.OutboundMessage, error)

	CreateJob(ctx context.Context, j *domain.Job) error
	UpdateJob(ctx context.Context, j *domain.Job) error
	// TouchJob bumps a running job's updated_at as a liveness heartbeat, so
	// stale-job recovery can distinguish an actively-progressing job from one
	// abandoned by a crashed worker.
	TouchJob(ctx context.Context, id string) error
	// FailStaleAccountJobs clears queued/running jobs for one account that have
	// stopped heart-beating past the grace window (used to unstick a prior
	// crashed sync when a new one starts).
	FailStaleAccountJobs(ctx context.Context, accountID string, graceSeconds int) (int64, error)
	RecoverStaleJobs(ctx context.Context) (int64, error)
	RecoverStaleJobsExcept(ctx context.Context, activeJobIDs []string, graceSeconds int) (int64, error)
	GetJob(ctx context.Context, userID, id string) (*domain.Job, error)
	ListJobs(ctx context.Context, userID string, limit int) ([]domain.Job, error)

	AppendAudit(ctx context.Context, ev domain.AuditEvent) error
	SearchAudit(ctx context.Context, userID string, limit int) ([]domain.AuditEvent, error)

	// Embeddings / semantic search.
	SaveEmbedding(ctx context.Context, userID, accountID, messageID string, chunkID int, vec []float32, model string) error
	SaveEmbeddingsBatch(ctx context.Context, userID, accountID string, items []domain.EmbeddingItem) error
	MessagesMissingEmbeddings(ctx context.Context, userID, accountID string, limit int) ([]string, error)
	SemanticSearch(ctx context.Context, userID, accountID string, queryVec []float32, limit int) ([]domain.SemanticHit, error)
	TryAcquireLease(ctx context.Context, key, nodeID string, durationSec int) (bool, error)
}

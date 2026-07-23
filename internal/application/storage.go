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

	EnsureUser(ctx context.Context, id, loginID string) error
	CreateUser(ctx context.Context, user *domain.User, passwordHash string) error
	GetUser(ctx context.Context, id string) (*domain.User, error)
	GetUserByLogin(ctx context.Context, loginID string) (*domain.User, string, error)
	GetUserByOIDC(ctx context.Context, issuer, subject string) (*domain.User, error)
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

	CreateAccount(ctx context.Context, a *domain.MailAccount) error
	GetAccount(ctx context.Context, userID, id string) (*domain.MailAccount, error)
	ListAccounts(ctx context.Context, userID string) ([]domain.MailAccount, error)
	UpdateAccount(ctx context.Context, a *domain.MailAccount) error
	SetAccountStatus(ctx context.Context, userID, id string, st domain.AccountStatus) error

	PutCredentialRef(ctx context.Context, c domain.CredentialRef) error
	TouchCredential(ctx context.Context, ref domain.SecretRef)
	SetCredentialStatus(ctx context.Context, ref domain.SecretRef, status string) error

	HasCheckpoint(ctx context.Context, accountID, uidl string) (bool, error)
	AddCheckpoint(ctx context.Context, accountID, uidl, messageID string) error
	StoredUIDLs(ctx context.Context, accountID string) (map[string]bool, error)

	InsertMessage(ctx context.Context, m *domain.Message, body *domain.MessageBody, atts []domain.Attachment) error
	IsDuplicateHash(ctx context.Context, accountID, rawHash string) (bool, error)
	DeleteMessage(ctx context.Context, userID, id string) ([]string, error)
	GetMessage(ctx context.Context, userID, id string) (*domain.Message, error)
	GetBody(ctx context.Context, userID, messageID string) (*domain.MessageBody, error)
	ListAttachments(ctx context.Context, userID, messageID string) ([]domain.Attachment, error)
	Search(ctx context.Context, q domain.SearchQuery) (*domain.SearchResult, error)

	ResolveThread(ctx context.Context, userID, accountID string, refs []string, subjectKey string, date int64) (string, error)
	GetThreadMessages(ctx context.Context, userID, threadID string) ([]domain.Message, error)

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
	RecoverStaleJobs(ctx context.Context) (int64, error)
	GetJob(ctx context.Context, userID, id string) (*domain.Job, error)

	AppendAudit(ctx context.Context, ev domain.AuditEvent) error
	SearchAudit(ctx context.Context, userID string, limit int) ([]domain.AuditEvent, error)

	// Embeddings / semantic search.
	SaveEmbedding(ctx context.Context, userID, accountID, messageID string, chunkID int, vec []float32, model string) error
	MessagesMissingEmbeddings(ctx context.Context, userID, accountID string, limit int) ([]string, error)
	SemanticSearch(ctx context.Context, userID, accountID string, queryVec []float32, limit int) ([]domain.SemanticHit, error)
}

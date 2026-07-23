// Package pgstore implements the application.Storage port on PostgreSQL
// (pgx, pure Go) for server / multi-user deployments. Full-text search uses
// tsvector + GIN; semantic search uses the pgvector extension.
//
// This adapter mirrors the SQLite adapter's semantics, including at-rest
// body-column encryption when a KEK is configured (EnableEncryption): the
// parsed body columns (text + sanitized HTML) are sealed with the same
// envelope format the SQLite adapter uses. Search/sort metadata (subject,
// addresses, dates) stays plaintext by design; the FTS tsvector still holds
// body plaintext (search over ciphertext is out of scope). Integration tests
// run only when POSTRA_TEST_PG is set to a DSN.
package pgstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"postra/internal/application"
	"postra/internal/domain"
	"postra/internal/platform/crypto"
)

type Store struct {
	pool        *pgxpool.Pool
	hasPgVector bool
	// kek, when set, encrypts message body columns at rest (same envelope
	// format as the SQLite adapter). Nil = plaintext bodies.
	kek *crypto.KEK
}

// EnableEncryption turns on at-rest encryption of the message body columns.
// Metadata columns used for search/sort stay queryable in plaintext, and the
// FTS tsvector holds body plaintext (see package doc).
func (s *Store) EnableEncryption(kek *crypto.KEK) { s.kek = kek }

const bodyEncPrefix = "enc:v1:"

// sealBody encrypts a body field when a KEK is configured, tagging the output
// so openBody can detect and reverse it. AAD binds the ciphertext to its
// message and field.
func (s *Store) sealBody(messageID, field, plain string) (string, error) {
	if s.kek == nil || plain == "" {
		return plain, nil
	}
	env, err := s.kek.Encrypt([]byte(plain), []byte("body:"+messageID+":"+field))
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(env)
	if err != nil {
		return "", err
	}
	return bodyEncPrefix + string(b), nil
}

func (s *Store) openBody(messageID, field, stored string) (string, error) {
	rest, ok := strings.CutPrefix(stored, bodyEncPrefix)
	if !ok {
		return stored, nil // plaintext (encryption off, or pre-encryption row)
	}
	if s.kek == nil {
		return "", errors.New("body is encrypted but no key is configured")
	}
	var env crypto.Envelope
	if err := json.Unmarshal([]byte(rest), &env); err != nil {
		return "", err
	}
	pt, err := s.kek.Decrypt(&env, []byte("body:"+messageID+":"+field))
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// RewrapBodies re-encrypts sealed body columns under the KEK's current version
// (§11.3 회전). No-op when encryption is disabled. Returns rows rewrapped.
func (s *Store) RewrapBodies(ctx context.Context) (int, error) {
	if s.kek == nil {
		return 0, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT message_id, text_body, html_sanitized FROM message_bodies`)
	if err != nil {
		return 0, err
	}
	type row struct{ id, text, html string }
	var todo []row
	for rows.Next() {
		var r row
		var text, html *string
		if err := rows.Scan(&r.id, &text, &html); err != nil {
			rows.Close()
			return 0, err
		}
		r.text, r.html = deref(text), deref(html)
		if strings.HasPrefix(r.text, bodyEncPrefix) || strings.HasPrefix(r.html, bodyEncPrefix) {
			todo = append(todo, r)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, r := range todo {
		text, err := s.openBody(r.id, "text", r.text)
		if err != nil {
			return n, err
		}
		html, err := s.openBody(r.id, "html", r.html)
		if err != nil {
			return n, err
		}
		sealedText, err := s.sealBody(r.id, "text", text)
		if err != nil {
			return n, err
		}
		sealedHTML, err := s.sealBody(r.id, "html", html)
		if err != nil {
			return n, err
		}
		if _, err := s.pool.Exec(ctx,
			`UPDATE message_bodies SET text_body=$1, html_sanitized=$2 WHERE message_id=$3`,
			sealedText, sealedHTML, r.id); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func (s *Store) HasPgVector() bool {
	return s.hasPgVector
}

// Ping verifies the PostgreSQL pool is usable (readiness probe).
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

func maskDSN(dsn string) string {
	if u, err := url.Parse(dsn); err == nil && u.User != nil {
		if _, hasPass := u.User.Password(); hasPass {
			u.User = url.UserPassword(u.User.Username(), "*****")
			return u.String()
		}
	}
	return dsn
}

// Open connects to PostgreSQL and applies the schema. The pgvector extension
// is required for semantic search.
func Open(ctx context.Context, dsn string) (*Store, error) {
	slog.Info("pgstore: connecting to postgres", "dsn_masked", maskDSN(dsn))
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		slog.Error("pgstore: postgres pool creation failed", "error", err, "reason", err.Error(), "dsn_masked", maskDSN(dsn))
		return nil, fmt.Errorf("postgres pool creation failed: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		slog.Error("pgstore: postgres ping failed", "error", err, "reason", err.Error(), "dsn_masked", maskDSN(dsn))
		return nil, fmt.Errorf("postgres ping failed: %w", err)
	}
	s := &Store{pool: pool}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		slog.Error("pgstore: postgres migration failed", "error", err, "reason", err.Error(), "dsn_masked", maskDSN(dsn))
		return nil, fmt.Errorf("postgres migration failed: %w", err)
	}
	slog.Info("pgstore: postgres connection and migration successful")
	return s, nil
}

func (s *Store) Close() error { s.pool.Close(); return nil }

func now() int64 { return time.Now().Unix() }

func NewID(prefix string) string {
	b := make([]byte, 10)
	rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

var ErrNotFound = domain.ErrNotFound

func (s *Store) migrate(ctx context.Context) error {
	var hasPgVector bool
	if _, err := s.pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		slog.Warn("pgstore: pgvector extension is not available. Postgres-native semantic search will be disabled.", "error", err)
		hasPgVector = false
	} else {
		hasPgVector = true
	}
	s.hasPgVector = hasPgVector

	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id TEXT PRIMARY KEY, login_id TEXT UNIQUE NOT NULL,
			status TEXT NOT NULL DEFAULT 'active', timezone TEXT DEFAULT 'UTC',
			display_name TEXT NOT NULL DEFAULT '', email TEXT NOT NULL DEFAULT '',
			role TEXT NOT NULL DEFAULT 'user', auth_provider TEXT NOT NULL DEFAULT 'local',
			password_hash TEXT NOT NULL DEFAULT '', oidc_issuer TEXT NOT NULL DEFAULT '',
			oidc_subject TEXT NOT NULL DEFAULT '', updated_at BIGINT NOT NULL DEFAULT 0,
			last_login_at BIGINT NOT NULL DEFAULT 0, created_at BIGINT NOT NULL)`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS display_name TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS email TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS role TEXT NOT NULL DEFAULT 'user'`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS auth_provider TEXT NOT NULL DEFAULT 'local'`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS password_hash TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS oidc_issuer TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS oidc_subject TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS updated_at BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS last_login_at BIGINT NOT NULL DEFAULT 0`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_oidc ON users(oidc_issuer,oidc_subject) WHERE oidc_subject != ''`,
		`CREATE TABLE IF NOT EXISTS auth_sessions (
			id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			token_hash TEXT UNIQUE NOT NULL, csrf_hash TEXT NOT NULL, expires_at BIGINT NOT NULL,
			created_at BIGINT NOT NULL, last_seen BIGINT NOT NULL,
			user_agent TEXT NOT NULL DEFAULT '', ip_address TEXT NOT NULL DEFAULT '')`,
		`CREATE INDEX IF NOT EXISTS idx_auth_sessions_expiry ON auth_sessions(expires_at)`,
		`CREATE TABLE IF NOT EXISTS system_settings (
			key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at BIGINT NOT NULL,
			updated_by TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE IF NOT EXISTS mcp_keys (
			id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			name TEXT NOT NULL, key_hash TEXT UNIQUE NOT NULL, key_prefix TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'active', created_at BIGINT NOT NULL, last_used_at BIGINT DEFAULT 0)`,
		`CREATE INDEX IF NOT EXISTS idx_mcp_keys_user ON mcp_keys(user_id)`,
		`CREATE TABLE IF NOT EXISTS mail_accounts (
			id TEXT PRIMARY KEY, user_id TEXT NOT NULL, name TEXT NOT NULL, email TEXT NOT NULL, status TEXT NOT NULL,
			inbound_protocol TEXT NOT NULL DEFAULT 'pop3',
			pop3_host TEXT, pop3_port INT, pop3_security TEXT, pop3_username TEXT, pop3_secret_ref TEXT,
			smtp_host TEXT, smtp_port INT, smtp_security TEXT, smtp_username TEXT, smtp_auth TEXT, smtp_secret_ref TEXT,
			insecure_skip_verify BOOL NOT NULL DEFAULT false, created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS credential_refs (
			ref TEXT PRIMARY KEY, owner_id TEXT NOT NULL, secret_type TEXT NOT NULL, provider TEXT NOT NULL,
			label TEXT, status TEXT NOT NULL DEFAULT 'active', version INT NOT NULL DEFAULT 1,
			created_at BIGINT NOT NULL, last_used_at BIGINT)`,
		`CREATE TABLE IF NOT EXISTS sync_checkpoints (
			account_id TEXT NOT NULL, uidl TEXT NOT NULL, message_id TEXT, synced_at BIGINT NOT NULL,
			PRIMARY KEY (account_id, uidl))`,
		`CREATE TABLE IF NOT EXISTS messages (
			id TEXT PRIMARY KEY, user_id TEXT NOT NULL, account_id TEXT NOT NULL, uidl TEXT,
			message_id_hdr TEXT, subject TEXT, from_name TEXT, from_email TEXT,
			to_json TEXT, cc_json TEXT, reply_to_json TEXT,
			date BIGINT, size BIGINT, raw_hash TEXT NOT NULL, raw_uri TEXT NOT NULL, thread_id TEXT,
			has_attachments BOOL NOT NULL DEFAULT false, in_reply_to TEXT, refs TEXT, auth_results TEXT, parse_error TEXT,
			created_at BIGINT NOT NULL, search_tsv tsvector,
			is_archived BOOL NOT NULL DEFAULT false, is_important BOOL NOT NULL DEFAULT false,
			snoozed_until BIGINT NOT NULL DEFAULT 0, labels_json TEXT NOT NULL DEFAULT '',
			legal_hold BOOL NOT NULL DEFAULT false)`,
		`ALTER TABLE messages ADD COLUMN IF NOT EXISTS is_archived BOOL NOT NULL DEFAULT false`,
		`ALTER TABLE messages ADD COLUMN IF NOT EXISTS is_important BOOL NOT NULL DEFAULT false`,
		`ALTER TABLE messages ADD COLUMN IF NOT EXISTS snoozed_until BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE messages ADD COLUMN IF NOT EXISTS labels_json TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE messages ADD COLUMN IF NOT EXISTS legal_hold BOOL NOT NULL DEFAULT false`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_hash ON messages(account_id, raw_hash)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_account_date ON messages(account_id, date DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_tsv ON messages USING GIN(search_tsv)`,
		`CREATE TABLE IF NOT EXISTS message_bodies (
			message_id TEXT PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
			text_body TEXT, html_sanitized TEXT, charset TEXT)`,
		`CREATE TABLE IF NOT EXISTS attachments (
			id TEXT PRIMARY KEY, message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
			name TEXT, mime_type TEXT, size BIGINT, hash TEXT, storage_uri TEXT, inline_flag BOOL DEFAULT false,
			scan_status TEXT DEFAULT 'clean', scan_detail TEXT)`,
		`ALTER TABLE attachments ADD COLUMN IF NOT EXISTS scan_status TEXT DEFAULT 'clean'`,
		`ALTER TABLE attachments ADD COLUMN IF NOT EXISTS scan_detail TEXT`,
		`CREATE TABLE IF NOT EXISTS threads (
			id TEXT PRIMARY KEY, user_id TEXT NOT NULL, account_id TEXT NOT NULL,
			subject_key TEXT, last_message_at BIGINT, message_count INT DEFAULT 0)`,
		`CREATE INDEX IF NOT EXISTS idx_threads_key ON threads(account_id, subject_key)`,
		`CREATE TABLE IF NOT EXISTS analyses (
			id TEXT PRIMARY KEY, user_id TEXT NOT NULL, target_type TEXT NOT NULL, target_id TEXT NOT NULL,
			analysis_type TEXT NOT NULL, result_json TEXT NOT NULL, model TEXT, prompt_version TEXT,
			input_hash TEXT, created_at BIGINT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_analyses_cache ON analyses(analysis_type, input_hash, model)`,
		`CREATE TABLE IF NOT EXISTS drafts (
			id TEXT PRIMARY KEY, user_id TEXT NOT NULL, account_id TEXT NOT NULL, kind TEXT NOT NULL,
			reply_to_message_id TEXT, status TEXT NOT NULL, current_version INT NOT NULL DEFAULT 0,
			created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS draft_versions (
			draft_id TEXT NOT NULL REFERENCES drafts(id) ON DELETE CASCADE, version INT NOT NULL,
			subject TEXT, body_text TEXT, body_html TEXT, to_json TEXT, cc_json TEXT, bcc_json TEXT,
			author TEXT NOT NULL, created_at BIGINT NOT NULL, PRIMARY KEY (draft_id, version))`,
		`CREATE TABLE IF NOT EXISTS approvals (
			id TEXT PRIMARY KEY, user_id TEXT NOT NULL, action_type TEXT NOT NULL, draft_id TEXT, draft_version INT,
			payload_hash TEXT NOT NULL, token_hash TEXT NOT NULL, approver TEXT, expires_at BIGINT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending', created_at BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS outbound_messages (
			id TEXT PRIMARY KEY, user_id TEXT NOT NULL, draft_id TEXT NOT NULL, draft_version INT NOT NULL,
			idempotency_key TEXT UNIQUE, message_id_hdr TEXT, status TEXT NOT NULL, smtp_response TEXT,
			attempts INT NOT NULL DEFAULT 0, next_attempt_at BIGINT NOT NULL DEFAULT 0,
			created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL)`,
		`ALTER TABLE outbound_messages ADD COLUMN IF NOT EXISTS next_attempt_at BIGINT NOT NULL DEFAULT 0`,
		`CREATE INDEX IF NOT EXISTS idx_outbound_retry ON outbound_messages(status, next_attempt_at)`,
		`CREATE TABLE IF NOT EXISTS jobs (
			id TEXT PRIMARY KEY, user_id TEXT NOT NULL, type TEXT NOT NULL, account_id TEXT, status TEXT NOT NULL,
			progress TEXT, stats_json TEXT, error TEXT, meta_json TEXT, created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS audit_events (
			id BIGSERIAL PRIMARY KEY, at BIGINT NOT NULL, user_id TEXT, actor TEXT, action TEXT NOT NULL,
			resource TEXT, result TEXT NOT NULL, detail TEXT)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_at ON audit_events(at DESC)`,
		`CREATE TABLE IF NOT EXISTS mail_rules (
			id TEXT PRIMARY KEY, user_id TEXT NOT NULL, name TEXT NOT NULL,
			enabled BOOL NOT NULL DEFAULT true, priority INT NOT NULL DEFAULT 100,
			match_mode TEXT NOT NULL DEFAULT 'all', conditions_json TEXT NOT NULL DEFAULT '[]',
			actions_json TEXT NOT NULL DEFAULT '[]', stop_on_match BOOL NOT NULL DEFAULT false,
			created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_mail_rules_user ON mail_rules(user_id, priority)`,
		`CREATE TABLE IF NOT EXISTS action_cards (
			id TEXT PRIMARY KEY, user_id TEXT NOT NULL, message_id TEXT NOT NULL,
			type TEXT NOT NULL, title TEXT NOT NULL, detail TEXT, due TEXT, assignee TEXT,
			status TEXT NOT NULL DEFAULT 'pending', export_target TEXT, external_ref TEXT,
			confidence DOUBLE PRECISION DEFAULT 0, created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_action_cards_user ON action_cards(user_id, status)`,
		`CREATE TABLE IF NOT EXISTS message_collab (
			message_id TEXT PRIMARY KEY, user_id TEXT NOT NULL, assignee TEXT,
			status TEXT NOT NULL DEFAULT 'open', sla_due BIGINT NOT NULL DEFAULT 0,
			updated_by TEXT, updated_at BIGINT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_message_collab_user ON message_collab(user_id, status)`,
		`CREATE TABLE IF NOT EXISTS message_notes (
			id TEXT PRIMARY KEY, message_id TEXT NOT NULL, user_id TEXT NOT NULL,
			author TEXT NOT NULL, body TEXT NOT NULL, created_at BIGINT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_message_notes_msg ON message_notes(message_id)`,
		`CREATE TABLE IF NOT EXISTS embedding_meta (
			message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE, chunk_id INT NOT NULL,
			user_id TEXT NOT NULL, account_id TEXT NOT NULL, model TEXT NOT NULL, dim INT NOT NULL,
			PRIMARY KEY (message_id, chunk_id))`,
		`CREATE INDEX IF NOT EXISTS idx_embedding_meta_scope ON embedding_meta(user_id, account_id)`,
	}
	for _, st := range stmts {
		if _, err := s.pool.Exec(ctx, st); err != nil {
			return fmt.Errorf("migrate: %w (stmt: %.60s)", err, st)
		}
	}

	if s.hasPgVector {
		vectorStmts := []string{
			`CREATE TABLE IF NOT EXISTS embeddings (
				message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE, chunk_id INT NOT NULL,
				user_id TEXT NOT NULL, account_id TEXT NOT NULL, model TEXT NOT NULL, dim INT NOT NULL,
				vec vector, PRIMARY KEY (message_id, chunk_id))`,
			`CREATE INDEX IF NOT EXISTS idx_embeddings_scope ON embeddings(user_id, account_id)`,
		}
		for _, st := range vectorStmts {
			if _, err := s.pool.Exec(ctx, st); err != nil {
				return fmt.Errorf("migrate (vector): %w (stmt: %.60s)", err, st)
			}
		}
	}

	return nil
}

func addrJSON(a []domain.Address) string {
	if len(a) == 0 {
		return ""
	}
	b, _ := json.Marshal(a)
	return string(b)
}

func addrFromJSON(s string) []domain.Address {
	if s == "" {
		return nil
	}
	var out []domain.Address
	json.Unmarshal([]byte(s), &out)
	return out
}

// vectorLiteral renders a []float32 as a pgvector text literal '[1,2,3]'.
func vectorLiteral(v []float32) string {
	parts := make([]string, len(v))
	for i, f := range v {
		parts[i] = strconv.FormatFloat(float64(f), 'g', -1, 32)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// ---------- users ----------

func (s *Store) EnsureUser(ctx context.Context, id, loginID string) error {
	var exists bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1 OR lower(login_id)=lower($2))`, id, loginID).Scan(&exists)
	if err == nil && exists {
		return nil
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO users (id,login_id,display_name,role,status,auth_provider,created_at,updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (id) DO NOTHING`,
		id, loginID, loginID, domain.RoleUser, domain.UserActive, "local", now(), now())
	if err != nil && (strings.Contains(err.Error(), "23505") || strings.Contains(err.Error(), "unique constraint") || strings.Contains(err.Error(), "users_pkey")) {
		return nil
	}
	return err
}

const userCols = `id,login_id,display_name,email,role,status,auth_provider,
 oidc_issuer,oidc_subject,created_at,updated_at,last_login_at`

func scanUser(row pgx.Row) (*domain.User, error) {
	var u domain.User
	err := row.Scan(&u.ID, &u.LoginID, &u.DisplayName, &u.Email, &u.Role, &u.Status,
		&u.AuthProvider, &u.OIDCIssuer, &u.OIDCSubject, &u.CreatedAt, &u.UpdatedAt, &u.LastLoginAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &u, err
}

func (s *Store) CreateUser(ctx context.Context, u *domain.User, passwordHash string) error {
	u.CreatedAt, u.UpdatedAt = now(), now()
	_, err := s.pool.Exec(ctx, `INSERT INTO users
	 (id,login_id,display_name,email,role,status,auth_provider,password_hash,oidc_issuer,oidc_subject,created_at,updated_at,last_login_at)
	 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		u.ID, u.LoginID, u.DisplayName, u.Email, u.Role, u.Status, u.AuthProvider,
		passwordHash, u.OIDCIssuer, u.OIDCSubject, u.CreatedAt, u.UpdatedAt, u.LastLoginAt)
	return err
}

func (s *Store) GetUser(ctx context.Context, id string) (*domain.User, error) {
	return scanUser(s.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id=$1`, id))
}

func (s *Store) GetUserByLogin(ctx context.Context, loginID string) (*domain.User, string, error) {
	var u domain.User
	var passwordHash string
	err := s.pool.QueryRow(ctx, `SELECT `+userCols+`,password_hash FROM users WHERE lower(login_id)=lower($1)`, loginID).
		Scan(&u.ID, &u.LoginID, &u.DisplayName, &u.Email, &u.Role, &u.Status, &u.AuthProvider,
			&u.OIDCIssuer, &u.OIDCSubject, &u.CreatedAt, &u.UpdatedAt, &u.LastLoginAt, &passwordHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	return &u, passwordHash, err
}

func (s *Store) GetUserByOIDC(ctx context.Context, issuer, subject string) (*domain.User, error) {
	return scanUser(s.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE oidc_issuer=$1 AND oidc_subject=$2`, issuer, subject))
}

func (s *Store) ListUsers(ctx context.Context) ([]domain.User, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+userCols+` FROM users ORDER BY login_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.User
	for rows.Next() {
		var u domain.User
		if err := rows.Scan(&u.ID, &u.LoginID, &u.DisplayName, &u.Email, &u.Role, &u.Status,
			&u.AuthProvider, &u.OIDCIssuer, &u.OIDCSubject, &u.CreatedAt, &u.UpdatedAt, &u.LastLoginAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) UpdateUser(ctx context.Context, u *domain.User) error {
	u.UpdatedAt = now()
	ct, err := s.pool.Exec(ctx, `UPDATE users SET login_id=$1,display_name=$2,email=$3,role=$4,status=$5,
	 auth_provider=$6,oidc_issuer=$7,oidc_subject=$8,updated_at=$9,last_login_at=$10 WHERE id=$11`,
		u.LoginID, u.DisplayName, u.Email, u.Role, u.Status, u.AuthProvider,
		u.OIDCIssuer, u.OIDCSubject, u.UpdatedAt, u.LastLoginAt, u.ID)
	if err == nil && ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Store) SetUserPassword(ctx context.Context, userID, passwordHash string) error {
	ct, err := s.pool.Exec(ctx, `UPDATE users SET password_hash=$1,auth_provider='local',updated_at=$2 WHERE id=$3`,
		passwordHash, now(), userID)
	if err == nil && ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Store) CountAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE role='admin' AND status='active'`).Scan(&n)
	return n, err
}

func (s *Store) CreateSession(ctx context.Context, ss *domain.Session) error {
	ss.CreatedAt, ss.LastSeen = now(), now()
	_, err := s.pool.Exec(ctx, `INSERT INTO auth_sessions
	 (id,user_id,token_hash,csrf_hash,expires_at,created_at,last_seen,user_agent,ip_address)
	 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, ss.ID, ss.UserID, ss.TokenHash, ss.CSRFHash,
		ss.ExpiresAt, ss.CreatedAt, ss.LastSeen, ss.UserAgent, ss.IPAddress)
	return err
}

func (s *Store) GetSessionByTokenHash(ctx context.Context, tokenHash string) (*domain.Session, *domain.User, error) {
	var ss domain.Session
	var u domain.User
	err := s.pool.QueryRow(ctx, `SELECT s.id,s.user_id,s.token_hash,s.csrf_hash,s.expires_at,s.created_at,s.last_seen,
	 s.user_agent,s.ip_address,`+prefixCols(userCols, "u.")+`
	 FROM auth_sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=$1 AND s.expires_at>$2`,
		tokenHash, now()).Scan(&ss.ID, &ss.UserID, &ss.TokenHash, &ss.CSRFHash, &ss.ExpiresAt,
		&ss.CreatedAt, &ss.LastSeen, &ss.UserAgent, &ss.IPAddress,
		&u.ID, &u.LoginID, &u.DisplayName, &u.Email, &u.Role, &u.Status, &u.AuthProvider,
		&u.OIDCIssuer, &u.OIDCSubject, &u.CreatedAt, &u.UpdatedAt, &u.LastLoginAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	return &ss, &u, err
}

func (s *Store) TouchSession(ctx context.Context, id string, lastSeen int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE auth_sessions SET last_seen=$1 WHERE id=$2`, lastSeen, id)
	return err
}
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM auth_sessions WHERE id=$1`, id)
	return err
}
func (s *Store) DeleteUserSessions(ctx context.Context, userID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM auth_sessions WHERE user_id=$1`, userID)
	return err
}
func (s *Store) GetSettings(ctx context.Context) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT key,value FROM system_settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		out[key] = value
	}
	return out, rows.Err()
}
func (s *Store) UpsertSettings(ctx context.Context, values map[string]string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for key, value := range values {
		if _, err := tx.Exec(ctx, `INSERT INTO system_settings(key,value,updated_at) VALUES($1,$2,$3)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, key, value, now()); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) GetOrCreateSetting(ctx context.Context, key, candidate string) (string, error) {
	if _, err := s.pool.Exec(ctx, `INSERT INTO system_settings(key,value,updated_at) VALUES($1,$2,$3)
	 ON CONFLICT(key) DO NOTHING`, key, candidate, now()); err != nil {
		return "", err
	}
	var value string
	err := s.pool.QueryRow(ctx, `SELECT value FROM system_settings WHERE key=$1`, key).Scan(&value)
	return value, err
}

// ---------- accounts ----------

const accountCols = `id,user_id,name,email,status,inbound_protocol,pop3_host,pop3_port,pop3_security,pop3_username,pop3_secret_ref,
 smtp_host,smtp_port,smtp_security,smtp_username,smtp_auth,smtp_secret_ref,insecure_skip_verify,created_at,updated_at`

func (s *Store) CreateAccount(ctx context.Context, a *domain.MailAccount) error {
	a.CreatedAt, a.UpdatedAt = now(), now()
	_, err := s.pool.Exec(ctx, `INSERT INTO mail_accounts (`+accountCols+`)
	 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`,
		a.ID, a.UserID, a.Name, a.Email, a.Status, a.InboundProtocol,
		a.POP3Host, a.POP3Port, a.POP3Security, a.POP3Username, string(a.POP3Secret),
		a.SMTPHost, a.SMTPPort, a.SMTPSecurity, a.SMTPUsername, a.SMTPAuth, string(a.SMTPSecret),
		a.InsecureSkipVerify, a.CreatedAt, a.UpdatedAt)
	return err
}

func scanAccount(row pgx.Row) (*domain.MailAccount, error) {
	var a domain.MailAccount
	var pop3Ref, smtpRef string
	err := row.Scan(&a.ID, &a.UserID, &a.Name, &a.Email, &a.Status, &a.InboundProtocol,
		&a.POP3Host, &a.POP3Port, &a.POP3Security, &a.POP3Username, &pop3Ref,
		&a.SMTPHost, &a.SMTPPort, &a.SMTPSecurity, &a.SMTPUsername, &a.SMTPAuth, &smtpRef,
		&a.InsecureSkipVerify, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.POP3Secret, a.SMTPSecret = domain.SecretRef(pop3Ref), domain.SecretRef(smtpRef)
	return &a, nil
}

func (s *Store) GetAccount(ctx context.Context, userID, id string) (*domain.MailAccount, error) {
	return scanAccount(s.pool.QueryRow(ctx, `SELECT `+accountCols+` FROM mail_accounts WHERE id=$1 AND user_id=$2`, id, userID))
}

func (s *Store) ListAccounts(ctx context.Context, userID string) ([]domain.MailAccount, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+accountCols+` FROM mail_accounts WHERE user_id=$1 AND status<>$2 ORDER BY created_at`,
		userID, domain.AccountDeleted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.MailAccount
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

func (s *Store) UpdateAccount(ctx context.Context, a *domain.MailAccount) error {
	a.UpdatedAt = now()
	ct, err := s.pool.Exec(ctx, `UPDATE mail_accounts SET
	 name=$1,email=$2,status=$3,inbound_protocol=$4,pop3_host=$5,pop3_port=$6,pop3_security=$7,pop3_username=$8,pop3_secret_ref=$9,
	 smtp_host=$10,smtp_port=$11,smtp_security=$12,smtp_username=$13,smtp_auth=$14,smtp_secret_ref=$15,
	 insecure_skip_verify=$16,updated_at=$17 WHERE id=$18 AND user_id=$19`,
		a.Name, a.Email, a.Status, a.InboundProtocol, a.POP3Host, a.POP3Port, a.POP3Security, a.POP3Username, string(a.POP3Secret),
		a.SMTPHost, a.SMTPPort, a.SMTPSecurity, a.SMTPUsername, a.SMTPAuth, string(a.SMTPSecret),
		a.InsecureSkipVerify, a.UpdatedAt, a.ID, a.UserID)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SetAccountStatus(ctx context.Context, userID, id string, st domain.AccountStatus) error {
	_, err := s.pool.Exec(ctx, `UPDATE mail_accounts SET status=$1, updated_at=$2 WHERE id=$3 AND user_id=$4`, st, now(), id, userID)
	return err
}

// ---------- credentials ----------

func (s *Store) PutCredentialRef(ctx context.Context, c domain.CredentialRef) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO credential_refs (ref,owner_id,secret_type,provider,label,status,version,created_at)
	 VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (ref) DO UPDATE SET status=excluded.status, version=excluded.version`,
		string(c.Ref), c.OwnerID, c.Type, c.Provider, c.Label, c.Status, c.Version, now())
	return err
}

func (s *Store) TouchCredential(ctx context.Context, ref domain.SecretRef) {
	s.pool.Exec(ctx, `UPDATE credential_refs SET last_used_at=$1 WHERE ref=$2`, now(), string(ref))
}

func (s *Store) SetCredentialStatus(ctx context.Context, ref domain.SecretRef, status string) error {
	_, err := s.pool.Exec(ctx, `UPDATE credential_refs SET status=$1 WHERE ref=$2`, status, string(ref))
	return err
}

// ---------- checkpoints ----------

func (s *Store) HasCheckpoint(ctx context.Context, accountID, uidl string) (bool, error) {
	var one int
	err := s.pool.QueryRow(ctx, `SELECT 1 FROM sync_checkpoints WHERE account_id=$1 AND uidl=$2`, accountID, uidl).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) AddCheckpoint(ctx context.Context, accountID, uidl, messageID string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO sync_checkpoints (account_id,uidl,message_id,synced_at)
	 VALUES ($1,$2,$3,$4) ON CONFLICT DO NOTHING`, accountID, uidl, messageID, now())
	return err
}

func (s *Store) StoredUIDLs(ctx context.Context, accountID string) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT uidl FROM sync_checkpoints WHERE account_id=$1`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out[u] = true
	}
	return out, rows.Err()
}

// ---------- messages ----------

const msgCols = `id,user_id,account_id,uidl,message_id_hdr,subject,from_name,from_email,to_json,cc_json,reply_to_json,
 date,size,raw_hash,raw_uri,thread_id,has_attachments,in_reply_to,refs,auth_results,parse_error,created_at`

// msgSelectCols extends the immutable ingest set (msgCols, used by
// InsertMessage's fixed placeholder list) with the mutable UX-state columns
// used by every SELECT + scanMessage.
const msgSelectCols = msgCols + `,is_archived,is_important,snoozed_until,labels_json,legal_hold`

func labelsFromJSON(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

func (s *Store) InsertMessage(ctx context.Context, m *domain.Message, body *domain.MessageBody, atts []domain.Attachment) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	m.CreatedAt = now()
	text := ""
	if body != nil {
		text = body.TextBody
	}
	_, err = tx.Exec(ctx, `INSERT INTO messages (`+msgCols+`,search_tsv)
	 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,
	  to_tsvector('simple', coalesce($6,'')||' '||coalesce($8,'')||' '||$23))`,
		m.ID, m.UserID, m.AccountID, m.UIDL, m.MessageID, m.Subject, m.From.Name, m.From.Email,
		addrJSON(m.To), addrJSON(m.Cc), addrJSON(m.ReplyTo), m.Date, m.Size, m.RawHash, m.RawURI, m.ThreadID,
		m.HasAttachments, m.InReplyTo, m.References, m.AuthResults, m.ParseError, m.CreatedAt, text)
	if err != nil {
		return err
	}
	if body != nil {
		sealedText, err := s.sealBody(m.ID, "text", body.TextBody)
		if err != nil {
			return err
		}
		sealedHTML, err := s.sealBody(m.ID, "html", body.HTMLSanitized)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO message_bodies (message_id,text_body,html_sanitized,charset) VALUES ($1,$2,$3,$4)`,
			m.ID, sealedText, sealedHTML, body.Charset); err != nil {
			return err
		}
	}
	for _, at := range atts {
		status := at.ScanStatus
		if status == "" {
			status = domain.ScanClean
		}
		if _, err := tx.Exec(ctx, `INSERT INTO attachments (id,message_id,name,mime_type,size,hash,storage_uri,inline_flag,scan_status,scan_detail)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, at.ID, m.ID, at.Name, at.MIMEType, at.Size, at.Hash, at.StorageURI, at.Inline,
			string(status), at.ScanDetail); err != nil {
			return err
		}
	}
	if m.ThreadID != "" {
		if _, err := tx.Exec(ctx, `UPDATE threads SET last_message_at=GREATEST(last_message_at,$1), message_count=message_count+1 WHERE id=$2`,
			m.Date, m.ThreadID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) IsDuplicateHash(ctx context.Context, accountID, rawHash string) (bool, error) {
	var one int
	err := s.pool.QueryRow(ctx, `SELECT 1 FROM messages WHERE account_id=$1 AND raw_hash=$2`, accountID, rawHash).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func scanMessage(row pgx.Row) (*domain.Message, error) {
	var m domain.Message
	var toJ, ccJ, rtJ, threadID, parseErr, authRes, inReplyTo, refs, labelsJSON *string
	err := row.Scan(&m.ID, &m.UserID, &m.AccountID, &m.UIDL, &m.MessageID, &m.Subject, &m.From.Name, &m.From.Email,
		&toJ, &ccJ, &rtJ, &m.Date, &m.Size, &m.RawHash, &m.RawURI, &threadID,
		&m.HasAttachments, &inReplyTo, &refs, &authRes, &parseErr, &m.CreatedAt,
		&m.IsArchived, &m.IsImportant, &m.SnoozedUntil, &labelsJSON, &m.LegalHold)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	m.To, m.Cc, m.ReplyTo = addrFromJSON(deref(toJ)), addrFromJSON(deref(ccJ)), addrFromJSON(deref(rtJ))
	m.ThreadID, m.ParseError, m.AuthResults = deref(threadID), deref(parseErr), deref(authRes)
	m.InReplyTo, m.References = deref(inReplyTo), deref(refs)
	m.Labels = labelsFromJSON(deref(labelsJSON))
	return &m, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (s *Store) GetMessage(ctx context.Context, userID, id string) (*domain.Message, error) {
	return scanMessage(s.pool.QueryRow(ctx, `SELECT `+msgSelectCols+` FROM messages WHERE id=$1 AND user_id=$2`, id, userID))
}

func (s *Store) GetBody(ctx context.Context, userID, messageID string) (*domain.MessageBody, error) {
	var b domain.MessageBody
	var html, charset *string
	err := s.pool.QueryRow(ctx, `SELECT b.message_id, b.text_body, b.html_sanitized, b.charset
	 FROM message_bodies b JOIN messages m ON m.id=b.message_id WHERE b.message_id=$1 AND m.user_id=$2`, messageID, userID).
		Scan(&b.MessageID, &b.TextBody, &html, &charset)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	b.HTMLSanitized, b.Charset = deref(html), deref(charset)
	if b.TextBody, err = s.openBody(messageID, "text", b.TextBody); err != nil {
		return nil, err
	}
	if b.HTMLSanitized, err = s.openBody(messageID, "html", b.HTMLSanitized); err != nil {
		return nil, err
	}
	return &b, nil
}

func (s *Store) ListAttachments(ctx context.Context, userID, messageID string) ([]domain.Attachment, error) {
	rows, err := s.pool.Query(ctx, `SELECT a.id,a.message_id,a.name,a.mime_type,a.size,a.hash,a.storage_uri,a.inline_flag,
	 COALESCE(a.scan_status,'clean'),COALESCE(a.scan_detail,'')
	 FROM attachments a JOIN messages m ON m.id=a.message_id WHERE a.message_id=$1 AND m.user_id=$2`, messageID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Attachment
	for rows.Next() {
		var a domain.Attachment
		var status string
		if err := rows.Scan(&a.ID, &a.MessageID, &a.Name, &a.MIMEType, &a.Size, &a.Hash, &a.StorageURI, &a.Inline, &status, &a.ScanDetail); err != nil {
			return nil, err
		}
		a.ScanStatus = domain.ScanStatus(status)
		out = append(out, a)
	}
	return out, rows.Err()
}

// UpdateMessage persists the mutable UX-state columns. A failure is surfaced
// directly — there is no subject-only fallback that would report a false
// success when the state write did not land.
func (s *Store) UpdateMessage(ctx context.Context, m *domain.Message) error {
	labels := m.Labels
	if labels == nil {
		labels = []string{}
	}
	labelsJSON, err := json.Marshal(labels)
	if err != nil {
		return err
	}
	ct, err := s.pool.Exec(ctx, `UPDATE messages SET is_archived=$1, is_important=$2, snoozed_until=$3, labels_json=$4, legal_hold=$5 WHERE id=$6 AND user_id=$7`,
		m.IsArchived, m.IsImportant, m.SnoozedUntil, string(labelsJSON), m.LegalHold, m.ID, m.UserID)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) Search(ctx context.Context, q domain.SearchQuery) (*domain.SearchResult, error) {
	limit := q.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	conds := []string{"m.user_id=$1"}
	args := []any{q.UserID}
	add := func(cond string, val any) {
		args = append(args, val)
		conds = append(conds, fmt.Sprintf(cond, len(args)))
	}
	if q.AccountID != "" {
		add("m.account_id=$%d", q.AccountID)
	}
	if q.From != "" {
		add("m.from_email ILIKE $%d", "%"+q.From+"%")
	}
	if q.To != "" {
		add("m.to_json ILIKE $%d", "%"+q.To+"%")
	}
	if q.Subject != "" {
		add("m.subject ILIKE $%d", "%"+q.Subject+"%")
	}
	if q.Since > 0 {
		add("m.date >= $%d", q.Since)
	}
	if q.Until > 0 {
		add("m.date <= $%d", q.Until)
	}
	if q.HasAttachment != nil {
		add("m.has_attachments = $%d", *q.HasAttachment)
	}
	if q.IsImportant != nil {
		add("m.is_important = $%d", *q.IsImportant)
	}
	if q.IsArchived != nil {
		add("m.is_archived = $%d", *q.IsArchived)
	}
	if q.Label != "" {
		add("m.labels_json LIKE $%d", `%"`+q.Label+`"%`)
	}
	switch q.Folder {
	case "inbox":
		add("(m.is_archived = false AND (m.snoozed_until = 0 OR m.snoozed_until <= $%d))", now())
	case "archive":
		conds = append(conds, "m.is_archived = true")
	case "important":
		conds = append(conds, "m.is_important = true")
	case "snoozed":
		add("m.snoozed_until > $%d", now())
	}
	if q.Text != "" {
		add("m.search_tsv @@ websearch_to_tsquery('simple', $%d)", q.Text)
	}
	if q.Cursor != "" {
		var cDate int64
		var cID string
		if _, err := fmt.Sscanf(q.Cursor, "%d:%s", &cDate, &cID); err == nil {
			args = append(args, cDate)
			di := len(args)
			args = append(args, cID)
			ii := len(args)
			conds = append(conds, fmt.Sprintf("(m.date < $%d OR (m.date = $%d AND m.id < $%d))", di, di, ii))
		}
	}
	args = append(args, limit+1)
	query := `SELECT ` + prefixCols(msgSelectCols, "m.") + ` FROM messages m WHERE ` + strings.Join(conds, " AND ") +
		fmt.Sprintf(` ORDER BY m.date DESC, m.id DESC LIMIT $%d`, len(args))
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var msgs []domain.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, *m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	res := &domain.SearchResult{}
	if len(msgs) > limit {
		msgs = msgs[:limit]
		last := msgs[len(msgs)-1]
		res.NextCursor = fmt.Sprintf("%d:%s", last.Date, last.ID)
	}
	res.Messages = msgs
	return res, nil
}

func prefixCols(cols, prefix string) string {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		parts[i] = prefix + strings.TrimSpace(p)
	}
	return strings.Join(parts, ",")
}

// ---------- threads ----------

func (s *Store) ResolveThread(ctx context.Context, userID, accountID string, refs []string, subjectKey string, date int64) (string, error) {
	for _, r := range refs {
		if r == "" {
			continue
		}
		var tid *string
		err := s.pool.QueryRow(ctx, `SELECT thread_id FROM messages
		 WHERE user_id=$1 AND account_id=$2 AND message_id_hdr=$3 AND thread_id <> '' LIMIT 1`, userID, accountID, r).Scan(&tid)
		if err == nil && tid != nil && *tid != "" {
			return *tid, nil
		}
	}
	if subjectKey != "" && len(refs) > 0 {
		var tid string
		err := s.pool.QueryRow(ctx, `SELECT id FROM threads WHERE user_id=$1 AND account_id=$2 AND subject_key=$3 LIMIT 1`,
			userID, accountID, subjectKey).Scan(&tid)
		if err == nil {
			return tid, nil
		}
	}
	tid := NewID("thr")
	_, err := s.pool.Exec(ctx, `INSERT INTO threads (id,user_id,account_id,subject_key,last_message_at,message_count)
	 VALUES ($1,$2,$3,$4,$5,0)`, tid, userID, accountID, subjectKey, date)
	return tid, err
}

func (s *Store) GetThreadMessages(ctx context.Context, userID, threadID string) ([]domain.Message, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+msgSelectCols+` FROM messages WHERE user_id=$1 AND thread_id=$2 ORDER BY date ASC`, userID, threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// ---------- mail rules ----------

const ruleCols = `id,user_id,name,enabled,priority,match_mode,conditions_json,actions_json,stop_on_match,created_at,updated_at`

func (s *Store) CreateRule(ctx context.Context, r *domain.MailRule) error {
	r.CreatedAt, r.UpdatedAt = now(), now()
	conds, _ := json.Marshal(r.Conditions)
	acts, _ := json.Marshal(r.Actions)
	_, err := s.pool.Exec(ctx, `INSERT INTO mail_rules
	 (id,user_id,name,enabled,priority,match_mode,conditions_json,actions_json,stop_on_match,created_at,updated_at)
	 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		r.ID, r.UserID, r.Name, r.Enabled, r.Priority, r.Match,
		string(conds), string(acts), r.StopOnMatch, r.CreatedAt, r.UpdatedAt)
	return err
}

func (s *Store) UpdateRule(ctx context.Context, r *domain.MailRule) error {
	r.UpdatedAt = now()
	conds, _ := json.Marshal(r.Conditions)
	acts, _ := json.Marshal(r.Actions)
	ct, err := s.pool.Exec(ctx, `UPDATE mail_rules SET name=$1,enabled=$2,priority=$3,match_mode=$4,
	 conditions_json=$5,actions_json=$6,stop_on_match=$7,updated_at=$8 WHERE id=$9 AND user_id=$10`,
		r.Name, r.Enabled, r.Priority, r.Match, string(conds), string(acts),
		r.StopOnMatch, r.UpdatedAt, r.ID, r.UserID)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteRule(ctx context.Context, userID, id string) error {
	ct, err := s.pool.Exec(ctx, `DELETE FROM mail_rules WHERE id=$1 AND user_id=$2`, id, userID)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scanRule(row pgx.Row) (*domain.MailRule, error) {
	var r domain.MailRule
	var conds, acts string
	if err := row.Scan(&r.ID, &r.UserID, &r.Name, &r.Enabled, &r.Priority, &r.Match,
		&conds, &acts, &r.StopOnMatch, &r.CreatedAt, &r.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	_ = json.Unmarshal([]byte(conds), &r.Conditions)
	_ = json.Unmarshal([]byte(acts), &r.Actions)
	return &r, nil
}

func (s *Store) GetRule(ctx context.Context, userID, id string) (*domain.MailRule, error) {
	return scanRule(s.pool.QueryRow(ctx, `SELECT `+ruleCols+` FROM mail_rules WHERE id=$1 AND user_id=$2`, id, userID))
}

func (s *Store) ListRules(ctx context.Context, userID string) ([]domain.MailRule, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+ruleCols+` FROM mail_rules WHERE user_id=$1 ORDER BY priority, created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.MailRule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// ---------- action cards ----------

const actionCardCols = `id,user_id,message_id,type,title,COALESCE(detail,''),COALESCE(due,''),COALESCE(assignee,''),status,COALESCE(export_target,''),COALESCE(external_ref,''),COALESCE(confidence,0),created_at,updated_at`

func (s *Store) CreateActionCard(ctx context.Context, c *domain.ActionCard) error {
	c.CreatedAt, c.UpdatedAt = now(), now()
	if c.Status == "" {
		c.Status = domain.ActionCardPending
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO action_cards
	 (id,user_id,message_id,type,title,detail,due,assignee,status,export_target,external_ref,confidence,created_at,updated_at)
	 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		c.ID, c.UserID, c.MessageID, c.Type, c.Title, c.Detail, c.Due, c.Assignee,
		c.Status, c.ExportTarget, c.ExternalRef, c.Confidence, c.CreatedAt, c.UpdatedAt)
	return err
}

func (s *Store) UpdateActionCard(ctx context.Context, c *domain.ActionCard) error {
	c.UpdatedAt = now()
	ct, err := s.pool.Exec(ctx, `UPDATE action_cards SET type=$1,title=$2,detail=$3,due=$4,assignee=$5,
	 status=$6,export_target=$7,external_ref=$8,confidence=$9,updated_at=$10 WHERE id=$11 AND user_id=$12`,
		c.Type, c.Title, c.Detail, c.Due, c.Assignee, c.Status, c.ExportTarget, c.ExternalRef,
		c.Confidence, c.UpdatedAt, c.ID, c.UserID)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scanActionCard(row pgx.Row) (*domain.ActionCard, error) {
	var c domain.ActionCard
	if err := row.Scan(&c.ID, &c.UserID, &c.MessageID, &c.Type, &c.Title, &c.Detail, &c.Due,
		&c.Assignee, &c.Status, &c.ExportTarget, &c.ExternalRef, &c.Confidence, &c.CreatedAt, &c.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &c, nil
}

func (s *Store) GetActionCard(ctx context.Context, userID, id string) (*domain.ActionCard, error) {
	return scanActionCard(s.pool.QueryRow(ctx, `SELECT `+actionCardCols+` FROM action_cards WHERE id=$1 AND user_id=$2`, id, userID))
}

func (s *Store) ListActionCards(ctx context.Context, userID, status string, limit int) ([]domain.ActionCard, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT ` + actionCardCols + ` FROM action_cards WHERE user_id=$1`
	args := []any{userID}
	if status != "" {
		query += ` AND status=$2`
		args = append(args, status)
	}
	query += ` ORDER BY created_at DESC LIMIT $` + strconv.Itoa(len(args)+1)
	args = append(args, limit)
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ActionCard
	for rows.Next() {
		c, err := scanActionCard(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// ---------- collaboration (shared mailbox) ----------

func (s *Store) UpsertMessageCollab(ctx context.Context, mc *domain.MessageCollab) error {
	mc.UpdatedAt = now()
	_, err := s.pool.Exec(ctx, `INSERT INTO message_collab
	 (message_id,user_id,assignee,status,sla_due,updated_by,updated_at) VALUES ($1,$2,$3,$4,$5,$6,$7)
	 ON CONFLICT(message_id) DO UPDATE SET assignee=excluded.assignee, status=excluded.status,
	   sla_due=excluded.sla_due, updated_by=excluded.updated_by, updated_at=excluded.updated_at`,
		mc.MessageID, mc.UserID, mc.Assignee, mc.Status, mc.SLADue, mc.UpdatedBy, mc.UpdatedAt)
	return err
}

func (s *Store) GetMessageCollab(ctx context.Context, userID, messageID string) (*domain.MessageCollab, error) {
	var mc domain.MessageCollab
	var assignee, updatedBy *string
	err := s.pool.QueryRow(ctx, `SELECT message_id,user_id,assignee,status,sla_due,updated_by,updated_at
	 FROM message_collab WHERE message_id=$1 AND user_id=$2`, messageID, userID).
		Scan(&mc.MessageID, &mc.UserID, &assignee, &mc.Status, &mc.SLADue, &updatedBy, &mc.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	mc.Assignee, mc.UpdatedBy = deref(assignee), deref(updatedBy)
	return &mc, nil
}

func (s *Store) ListMessageCollab(ctx context.Context, userID, status, assignee string, limit int) ([]domain.MessageCollab, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT message_id,user_id,COALESCE(assignee,''),status,sla_due,COALESCE(updated_by,''),updated_at
	 FROM message_collab WHERE user_id=$1`
	args := []any{userID}
	if status != "" {
		args = append(args, status)
		q += ` AND status=$` + strconv.Itoa(len(args))
	}
	if assignee != "" {
		args = append(args, assignee)
		q += ` AND assignee=$` + strconv.Itoa(len(args))
	}
	args = append(args, limit)
	q += ` ORDER BY updated_at DESC LIMIT $` + strconv.Itoa(len(args))
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.MessageCollab
	for rows.Next() {
		var mc domain.MessageCollab
		if err := rows.Scan(&mc.MessageID, &mc.UserID, &mc.Assignee, &mc.Status, &mc.SLADue, &mc.UpdatedBy, &mc.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, mc)
	}
	return out, rows.Err()
}

func (s *Store) AddMessageNote(ctx context.Context, n *domain.MessageNote) error {
	n.CreatedAt = now()
	_, err := s.pool.Exec(ctx, `INSERT INTO message_notes (id,message_id,user_id,author,body,created_at)
	 VALUES ($1,$2,$3,$4,$5,$6)`, n.ID, n.MessageID, n.UserID, n.Author, n.Body, n.CreatedAt)
	return err
}

func (s *Store) ListMessageNotes(ctx context.Context, userID, messageID string) ([]domain.MessageNote, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,message_id,user_id,author,body,created_at
	 FROM message_notes WHERE message_id=$1 AND user_id=$2 ORDER BY created_at`, messageID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.MessageNote
	for rows.Next() {
		var n domain.MessageNote
		if err := rows.Scan(&n.ID, &n.MessageID, &n.UserID, &n.Author, &n.Body, &n.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ---------- analyses ----------

func (s *Store) SaveAnalysis(ctx context.Context, a *domain.Analysis) error {
	a.CreatedAt = now()
	_, err := s.pool.Exec(ctx, `INSERT INTO analyses
	 (id,user_id,target_type,target_id,analysis_type,result_json,model,prompt_version,input_hash,created_at)
	 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		a.ID, a.UserID, a.TargetType, a.TargetID, a.AnalysisType, a.ResultJSON, a.Model, a.PromptVersion, a.InputHash, a.CreatedAt)
	return err
}

func (s *Store) FindCachedAnalysis(ctx context.Context, userID, analysisType, inputHash, model string) (*domain.Analysis, error) {
	var a domain.Analysis
	err := s.pool.QueryRow(ctx, `SELECT id,user_id,target_type,target_id,analysis_type,result_json,model,prompt_version,input_hash,created_at
	 FROM analyses WHERE user_id=$1 AND analysis_type=$2 AND input_hash=$3 AND model=$4 ORDER BY created_at DESC LIMIT 1`,
		userID, analysisType, inputHash, model).
		Scan(&a.ID, &a.UserID, &a.TargetType, &a.TargetID, &a.AnalysisType, &a.ResultJSON, &a.Model, &a.PromptVersion, &a.InputHash, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// ---------- drafts ----------

func (s *Store) CreateDraft(ctx context.Context, d *domain.Draft, v *domain.DraftVersion) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	d.CreatedAt, d.UpdatedAt, d.CurrentVersion = now(), now(), 1
	v.Version, v.CreatedAt = 1, now()
	if _, err := tx.Exec(ctx, `INSERT INTO drafts (id,user_id,account_id,kind,reply_to_message_id,status,current_version,created_at,updated_at)
	 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, d.ID, d.UserID, d.AccountID, d.Kind, d.ReplyToMessageID, d.Status, d.CurrentVersion, d.CreatedAt, d.UpdatedAt); err != nil {
		return err
	}
	if err := insertDraftVersionTx(ctx, tx, d.ID, v); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func insertDraftVersionTx(ctx context.Context, tx pgx.Tx, draftID string, v *domain.DraftVersion) error {
	_, err := tx.Exec(ctx, `INSERT INTO draft_versions (draft_id,version,subject,body_text,body_html,to_json,cc_json,bcc_json,author,created_at)
	 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		draftID, v.Version, v.Subject, v.BodyText, v.BodyHTML, addrJSON(v.To), addrJSON(v.Cc), addrJSON(v.Bcc), v.Author, v.CreatedAt)
	return err
}

func (s *Store) AddDraftVersion(ctx context.Context, userID, draftID string, v *domain.DraftVersion) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var cur int
	err = tx.QueryRow(ctx, `SELECT current_version FROM drafts WHERE id=$1 AND user_id=$2 AND status='open'`, draftID, userID).Scan(&cur)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	v.Version, v.CreatedAt = cur+1, now()
	if err := insertDraftVersionTx(ctx, tx, draftID, v); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE drafts SET current_version=$1, updated_at=$2 WHERE id=$3`, v.Version, now(), draftID); err != nil {
		return 0, err
	}
	return v.Version, tx.Commit(ctx)
}

func (s *Store) GetDraft(ctx context.Context, userID, id string) (*domain.Draft, *domain.DraftVersion, error) {
	var d domain.Draft
	var replyTo *string
	err := s.pool.QueryRow(ctx, `SELECT id,user_id,account_id,kind,reply_to_message_id,status,current_version,created_at,updated_at
	 FROM drafts WHERE id=$1 AND user_id=$2`, id, userID).
		Scan(&d.ID, &d.UserID, &d.AccountID, &d.Kind, &replyTo, &d.Status, &d.CurrentVersion, &d.CreatedAt, &d.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	d.ReplyToMessageID = deref(replyTo)
	v, err := s.GetDraftVersion(ctx, userID, id, d.CurrentVersion)
	if err != nil {
		return nil, nil, err
	}
	return &d, v, nil
}

func (s *Store) GetDraftVersion(ctx context.Context, userID, draftID string, version int) (*domain.DraftVersion, error) {
	var v domain.DraftVersion
	var toJ, ccJ, bccJ, html *string
	err := s.pool.QueryRow(ctx, `SELECT v.draft_id,v.version,v.subject,v.body_text,v.body_html,v.to_json,v.cc_json,v.bcc_json,v.author,v.created_at
	 FROM draft_versions v JOIN drafts d ON d.id=v.draft_id WHERE v.draft_id=$1 AND v.version=$2 AND d.user_id=$3`, draftID, version, userID).
		Scan(&v.DraftID, &v.Version, &v.Subject, &v.BodyText, &html, &toJ, &ccJ, &bccJ, &v.Author, &v.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	v.BodyHTML = deref(html)
	v.To, v.Cc, v.Bcc = addrFromJSON(deref(toJ)), addrFromJSON(deref(ccJ)), addrFromJSON(deref(bccJ))
	return &v, nil
}

func (s *Store) SetDraftStatus(ctx context.Context, userID, id string, st domain.DraftStatus) error {
	_, err := s.pool.Exec(ctx, `UPDATE drafts SET status=$1, updated_at=$2 WHERE id=$3 AND user_id=$4`, st, now(), id, userID)
	return err
}

// ---------- approvals ----------

func (s *Store) InsertApproval(ctx context.Context, id, userID, actionType, draftID string, draftVersion int, payloadHash, tokenHash, approver string, expiresAt int64) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO approvals (id,user_id,action_type,draft_id,draft_version,payload_hash,token_hash,approver,expires_at,status,created_at)
	 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'pending',$10)`,
		id, userID, actionType, draftID, draftVersion, payloadHash, tokenHash, approver, expiresAt, now())
	return err
}

func (s *Store) ConsumeApproval(ctx context.Context, tokenHash, payloadHash string) (string, int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", 0, err
	}
	defer tx.Rollback(ctx)
	var id, draftID string
	var version int
	err = tx.QueryRow(ctx, `SELECT id, COALESCE(draft_id,''), COALESCE(draft_version,0) FROM approvals
	 WHERE token_hash=$1 AND payload_hash=$2 AND status='pending' AND expires_at > $3 FOR UPDATE`,
		tokenHash, payloadHash, now()).Scan(&id, &draftID, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, errors.New("approval token invalid, expired, consumed, or payload changed")
	}
	if err != nil {
		return "", 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE approvals SET status='used' WHERE id=$1`, id); err != nil {
		return "", 0, err
	}
	return draftID, version, tx.Commit(ctx)
}

// ---------- outbound ----------

func (s *Store) CreateOutbound(ctx context.Context, o *domain.OutboundMessage) error {
	o.CreatedAt, o.UpdatedAt = now(), now()
	_, err := s.pool.Exec(ctx, `INSERT INTO outbound_messages (id,user_id,draft_id,draft_version,idempotency_key,message_id_hdr,status,attempts,created_at,updated_at)
	 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		o.ID, o.UserID, o.DraftID, o.DraftVersion, o.IdempotencyKey, o.MessageID, o.Status, o.Attempts, o.CreatedAt, o.UpdatedAt)
	return err
}

func scanOutbound(row pgx.Row) (*domain.OutboundMessage, error) {
	var o domain.OutboundMessage
	err := row.Scan(&o.ID, &o.UserID, &o.DraftID, &o.DraftVersion, &o.IdempotencyKey, &o.MessageID, &o.Status, &o.SMTPResponse, &o.Attempts, &o.NextAttemptAt, &o.CreatedAt, &o.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

const outboundCols = `id,user_id,draft_id,draft_version,idempotency_key,message_id_hdr,status,COALESCE(smtp_response,''),attempts,next_attempt_at,created_at,updated_at`

func (s *Store) GetOutboundByIdemKey(ctx context.Context, userID, key string) (*domain.OutboundMessage, error) {
	return scanOutbound(s.pool.QueryRow(ctx, `SELECT `+outboundCols+` FROM outbound_messages WHERE user_id=$1 AND idempotency_key=$2`, userID, key))
}

func (s *Store) GetOutbound(ctx context.Context, userID, id string) (*domain.OutboundMessage, error) {
	return scanOutbound(s.pool.QueryRow(ctx, `SELECT `+outboundCols+` FROM outbound_messages WHERE user_id=$1 AND id=$2`, userID, id))
}

func (s *Store) CountSentSince(ctx context.Context, userID, accountID string, since int64) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM outbound_messages o JOIN drafts d ON d.id=o.draft_id
	 WHERE d.user_id=$1 AND d.account_id=$2 AND o.status='sent' AND o.updated_at >= $3`, userID, accountID, since).Scan(&n)
	return n, err
}

func (s *Store) UpdateOutbound(ctx context.Context, id string, status domain.OutboundStatus, smtpResponse string, attempts int) error {
	_, err := s.pool.Exec(ctx, `UPDATE outbound_messages SET status=$1, smtp_response=$2, attempts=$3, next_attempt_at=0, updated_at=$4 WHERE id=$5`,
		status, smtpResponse, attempts, now(), id)
	return err
}

func (s *Store) MarkOutboundRetry(ctx context.Context, id, smtpResponse string, attempts int, nextAttemptAt int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE outbound_messages SET status=$1, smtp_response=$2, attempts=$3, next_attempt_at=$4, updated_at=$5 WHERE id=$6`,
		domain.OutboundRetryWait, smtpResponse, attempts, nextAttemptAt, now(), id)
	return err
}

func (s *Store) ListDueRetries(ctx context.Context, nowTS int64, limit int) ([]domain.OutboundMessage, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `SELECT `+outboundCols+`
	 FROM outbound_messages WHERE status=$1 AND next_attempt_at <= $2 ORDER BY next_attempt_at LIMIT $3`,
		domain.OutboundRetryWait, nowTS, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.OutboundMessage
	for rows.Next() {
		o, err := scanOutbound(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *o)
	}
	return out, rows.Err()
}

// ---------- jobs ----------

func (s *Store) CreateJob(ctx context.Context, j *domain.Job) error {
	j.CreatedAt, j.UpdatedAt = now(), now()
	stats, _ := json.Marshal(j.Stats)
	meta, _ := json.Marshal(j.Meta)
	_, err := s.pool.Exec(ctx, `INSERT INTO jobs (id,user_id,type,account_id,status,progress,stats_json,error,meta_json,created_at,updated_at)
	 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		j.ID, j.UserID, j.Type, j.AccountID, j.Status, j.Progress, string(stats), j.Error, string(meta), j.CreatedAt, j.UpdatedAt)
	return err
}

func (s *Store) UpdateJob(ctx context.Context, j *domain.Job) error {
	j.UpdatedAt = now()
	stats, _ := json.Marshal(j.Stats)
	_, err := s.pool.Exec(ctx, `UPDATE jobs SET status=$1, progress=$2, stats_json=$3, error=$4, updated_at=$5 WHERE id=$6`,
		j.Status, j.Progress, string(stats), j.Error, j.UpdatedAt, j.ID)
	return err
}

func (s *Store) RecoverStaleJobs(ctx context.Context) (int64, error) {
	ct, err := s.pool.Exec(ctx, `UPDATE jobs SET status='failed', error='interrupted (process restart)', updated_at=$1
	 WHERE status IN ('queued','running')`, now())
	if err != nil {
		return 0, err
	}
	return ct.RowsAffected(), nil
}

func (s *Store) GetJob(ctx context.Context, userID, id string) (*domain.Job, error) {
	var j domain.Job
	var stats, meta, accountID, progress, jerr *string
	err := s.pool.QueryRow(ctx, `SELECT id,user_id,type,account_id,status,progress,stats_json,error,meta_json,created_at,updated_at
	 FROM jobs WHERE id=$1 AND user_id=$2`, id, userID).
		Scan(&j.ID, &j.UserID, &j.Type, &accountID, &j.Status, &progress, &stats, &jerr, &meta, &j.CreatedAt, &j.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	j.AccountID, j.Progress, j.Error = deref(accountID), deref(progress), deref(jerr)
	if deref(stats) != "" {
		json.Unmarshal([]byte(*stats), &j.Stats)
	}
	if deref(meta) != "" {
		json.Unmarshal([]byte(*meta), &j.Meta)
	}
	return &j, nil
}

func (s *Store) ListJobs(ctx context.Context, userID string, limit int) ([]domain.Job, error) {
	if limit <= 0 {
		limit = 10
	}
	query := `SELECT id, user_id, type, account_id, status, progress, stats_json, error, meta_json, created_at, updated_at
		FROM jobs WHERE user_id=$1 ORDER BY updated_at DESC LIMIT $2`
	args := []any{userID, limit}
	if userID == "" {
		query = `SELECT id, user_id, type, account_id, status, progress, stats_json, error, meta_json, created_at, updated_at
			FROM jobs ORDER BY updated_at DESC LIMIT $1`
		args = []any{limit}
	}
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Job
	for rows.Next() {
		var j domain.Job
		var stats, meta, accountID, progress, jerr *string
		if err := rows.Scan(&j.ID, &j.UserID, &j.Type, &accountID, &j.Status, &progress, &stats, &jerr, &meta, &j.CreatedAt, &j.UpdatedAt); err != nil {
			return nil, err
		}
		j.AccountID, j.Progress, j.Error = deref(accountID), deref(progress), deref(jerr)
		if deref(stats) != "" {
			_ = json.Unmarshal([]byte(*stats), &j.Stats)
		}
		if deref(meta) != "" {
			_ = json.Unmarshal([]byte(*meta), &j.Meta)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ---------- audit ----------

func (s *Store) AppendAudit(ctx context.Context, ev domain.AuditEvent) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO audit_events (at,user_id,actor,action,resource,result,detail)
	 VALUES ($1,$2,$3,$4,$5,$6,$7)`, now(), ev.UserID, ev.Actor, ev.Action, ev.Resource, ev.Result, ev.Detail)
	return err
}

func (s *Store) SearchAudit(ctx context.Context, userID string, limit int) ([]domain.AuditEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT id,at,user_id,actor,action,resource,result,COALESCE(detail,'')
	 FROM audit_events WHERE user_id=$1 ORDER BY id DESC LIMIT $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.AuditEvent
	for rows.Next() {
		var e domain.AuditEvent
		if err := rows.Scan(&e.ID, &e.At, &e.UserID, &e.Actor, &e.Action, &e.Resource, &e.Result, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------- delete ----------

func (s *Store) DeleteMessage(ctx context.Context, userID, id string) ([]string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var rawURI string
	err = tx.QueryRow(ctx, `SELECT raw_uri FROM messages WHERE id=$1 AND user_id=$2`, id, userID).Scan(&rawURI)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	candidates := []string{rawURI}
	rows, err := tx.Query(ctx, `SELECT storage_uri FROM attachments WHERE message_id=$1`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			rows.Close()
			return nil, err
		}
		if u != "" {
			candidates = append(candidates, u)
		}
	}
	rows.Close()
	if _, err := tx.Exec(ctx, `DELETE FROM messages WHERE id=$1 AND user_id=$2`, id, userID); err != nil {
		return nil, err
	}
	// Return only blobs no surviving message references (shared-blob safety).
	var orphans []string
	for _, u := range candidates {
		var refs int
		if err := tx.QueryRow(ctx,
			`SELECT (SELECT COUNT(*) FROM messages WHERE raw_uri=$1) +
			        (SELECT COUNT(*) FROM attachments WHERE storage_uri=$1)`, u).Scan(&refs); err != nil {
			return nil, err
		}
		if refs == 0 {
			orphans = append(orphans, u)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return orphans, nil
}

// ---------- embeddings ----------

func (s *Store) SaveEmbedding(ctx context.Context, userID, accountID, messageID string, chunkID int, vec []float32, model string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Always insert into embedding_meta to track progress
	_, err = tx.Exec(ctx, `INSERT INTO embedding_meta (message_id,chunk_id,user_id,account_id,model,dim)
	 VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (message_id,chunk_id) DO UPDATE SET model=excluded.model, dim=excluded.dim`,
		messageID, chunkID, userID, accountID, model, len(vec))
	if err != nil {
		return err
	}

	// Conditionally insert into embeddings table if pgvector is enabled and vector is not empty
	if s.hasPgVector && len(vec) > 0 {
		_, err = tx.Exec(ctx, `INSERT INTO embeddings (message_id,chunk_id,user_id,account_id,model,dim,vec)
		 VALUES ($1,$2,$3,$4,$5,$6,$7::vector) ON CONFLICT (message_id,chunk_id) DO UPDATE SET model=excluded.model, dim=excluded.dim, vec=excluded.vec`,
			messageID, chunkID, userID, accountID, model, len(vec), vectorLiteral(vec))
		if err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

func (s *Store) SaveEmbeddingsBatch(ctx context.Context, userID, accountID string, items []domain.EmbeddingItem) error {
	if len(items) == 0 {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	for _, item := range items {
		_, err = tx.Exec(ctx, `INSERT INTO embedding_meta (message_id,chunk_id,user_id,account_id,model,dim)
		 VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (message_id,chunk_id) DO UPDATE SET model=excluded.model, dim=excluded.dim`,
			item.MessageID, item.ChunkID, userID, accountID, item.Model, len(item.Vector))
		if err != nil {
			return err
		}

		if s.hasPgVector && len(item.Vector) > 0 {
			_, err = tx.Exec(ctx, `INSERT INTO embeddings (message_id,chunk_id,user_id,account_id,model,dim,vec)
			 VALUES ($1,$2,$3,$4,$5,$6,$7::vector) ON CONFLICT (message_id,chunk_id) DO UPDATE SET model=excluded.model, dim=excluded.dim, vec=excluded.vec`,
				item.MessageID, item.ChunkID, userID, accountID, item.Model, len(item.Vector), vectorLiteral(item.Vector))
			if err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) MessagesMissingEmbeddings(ctx context.Context, userID, accountID string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 200
	}
	args := []any{userID}
	q := `SELECT m.id FROM messages m WHERE m.user_id=$1 AND NOT EXISTS (SELECT 1 FROM embedding_meta e WHERE e.message_id=m.id)`
	if accountID != "" {
		args = append(args, accountID)
		q += fmt.Sprintf(" AND m.account_id=$%d", len(args))
	}
	args = append(args, limit)
	q += fmt.Sprintf(" ORDER BY m.date DESC LIMIT $%d", len(args))
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SemanticSearch uses pgvector cosine distance (<=>). Score = 1 - distance.
func (s *Store) SemanticSearch(ctx context.Context, userID, accountID string, queryVec []float32, limit int) ([]domain.SemanticHit, error) {
	if !s.hasPgVector {
		return nil, fmt.Errorf("pgvector semantic search is not available on this database")
	}
	if limit <= 0 {
		limit = 10
	}
	args := []any{userID, vectorLiteral(queryVec)}
	q := `SELECT message_id, 1 - (vec <=> $2::vector) AS score FROM embeddings WHERE user_id=$1`
	if accountID != "" {
		args = append(args, accountID)
		q += fmt.Sprintf(" AND account_id=$%d", len(args))
	}
	args = append(args, limit)
	q += fmt.Sprintf(" ORDER BY vec <=> $2::vector ASC LIMIT $%d", len(args))
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.SemanticHit
	for rows.Next() {
		var h domain.SemanticHit
		if err := rows.Scan(&h.MessageID, &h.Score); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ---------- MCP Keys ----------

func (s *Store) CreateMCPKey(ctx context.Context, key *domain.MCPKey) error {
	key.CreatedAt = now()
	_, err := s.pool.Exec(ctx, `INSERT INTO mcp_keys (id, user_id, name, key_hash, key_prefix, status, created_at, last_used_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		key.ID, key.UserID, key.Name, key.KeyHash, key.KeyPrefix, key.Status, key.CreatedAt, key.LastUsedAt)
	return err
}

func (s *Store) GetMCPKeyByHash(ctx context.Context, keyHash string) (*domain.MCPKey, *domain.User, error) {
	var k domain.MCPKey
	var u domain.User
	err := s.pool.QueryRow(ctx, `SELECT k.id, k.user_id, k.name, k.key_hash, k.key_prefix, k.status, k.created_at, k.last_used_at,
		u.id, u.login_id, u.display_name, u.email, u.role, u.status, u.auth_provider, u.created_at, u.updated_at, u.last_login_at
		FROM mcp_keys k JOIN users u ON u.id = k.user_id WHERE k.key_hash = $1 AND k.status = 'active'`, keyHash).
		Scan(&k.ID, &k.UserID, &k.Name, &k.KeyHash, &k.KeyPrefix, &k.Status, &k.CreatedAt, &k.LastUsedAt,
			&u.ID, &u.LoginID, &u.DisplayName, &u.Email, &u.Role, &u.Status, &u.AuthProvider, &u.CreatedAt, &u.UpdatedAt, &u.LastLoginAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	return &k, &u, nil
}

func (s *Store) ListMCPKeys(ctx context.Context, userID string) ([]domain.MCPKey, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, user_id, name, key_hash, key_prefix, status, created_at, last_used_at
		FROM mcp_keys WHERE user_id = $1 ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.MCPKey
	for rows.Next() {
		var k domain.MCPKey
		if err := rows.Scan(&k.ID, &k.UserID, &k.Name, &k.KeyHash, &k.KeyPrefix, &k.Status, &k.CreatedAt, &k.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Store) ListAllMCPKeys(ctx context.Context) ([]domain.MCPKey, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, user_id, name, key_hash, key_prefix, status, created_at, last_used_at
		FROM mcp_keys ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.MCPKey
	for rows.Next() {
		var k domain.MCPKey
		if err := rows.Scan(&k.ID, &k.UserID, &k.Name, &k.KeyHash, &k.KeyPrefix, &k.Status, &k.CreatedAt, &k.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Store) RevokeMCPKey(ctx context.Context, userID, keyID string) error {
	if userID != "" {
		res, err := s.pool.Exec(ctx, `UPDATE mcp_keys SET status = 'revoked' WHERE id = $1 AND user_id = $2`, keyID, userID)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return ErrNotFound
		}
	} else {
		res, err := s.pool.Exec(ctx, `UPDATE mcp_keys SET status = 'revoked' WHERE id = $1`, keyID)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return ErrNotFound
		}
	}
	return nil
}

func (s *Store) TouchMCPKey(ctx context.Context, keyID string, lastUsedAt int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE mcp_keys SET last_used_at = $1 WHERE id = $2`, lastUsedAt, keyID)
	return err
}

func (s *Store) NotifySettingsChange(ctx context.Context) {
	_, err := s.pool.Exec(ctx, `NOTIFY postra_settings_changed`)
	if err != nil {
		slog.Error("pgstore: failed to send settings change notification", "error", err)
	}
}

func (s *Store) ListenSettingsChange(ctx context.Context, cb func()) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			conn, err := s.pool.Acquire(ctx)
			if err != nil {
				slog.Error("pgstore: listen failed to acquire connection, retrying in 5s", "error", err)
				time.Sleep(5 * time.Second)
				continue
			}

			_, err = conn.Exec(ctx, "LISTEN postra_settings_changed")
			if err != nil {
				conn.Release()
				slog.Error("pgstore: listen query failed, retrying in 5s", "error", err)
				time.Sleep(5 * time.Second)
				continue
			}

			slog.Info("pgstore: listening for postra_settings_changed notifications")

			// We define a loop to handle notifications
			for {
				notification, err := conn.Conn().WaitForNotification(ctx)
				if err != nil {
					conn.Release()
					slog.Warn("pgstore: listen connection lost, reconnecting", "error", err)
					break
				}

				slog.Info("pgstore: received settings changed notification", "payload", notification.Payload)
				cb()
			}

			time.Sleep(2 * time.Second)
		}
	}()
}

func (s *Store) TryAcquireLease(ctx context.Context, key, nodeID string, durationSec int) (bool, error) {
	nowTS := time.Now().Unix()
	expiresTS := nowTS + int64(durationSec)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	var value string
	var updatedAt int64
	err = tx.QueryRow(ctx, `SELECT value, updated_at FROM system_settings WHERE key = $1 FOR UPDATE`, key).Scan(&value, &updatedAt)

	type Lease struct {
		NodeID    string `json:"node_id"`
		ExpiresAt int64  `json:"expires_at"`
	}

	var current Lease
	isFree := false
	hasRow := true
	if errors.Is(err, pgx.ErrNoRows) {
		isFree = true
		hasRow = false
	} else if err != nil {
		return false, err
	} else {
		_ = json.Unmarshal([]byte(value), &current)
		if current.ExpiresAt < nowTS || current.NodeID == nodeID {
			isFree = true
		}
	}

	if !isFree {
		return false, nil
	}

	newLease := Lease{NodeID: nodeID, ExpiresAt: expiresTS}
	newVal, _ := json.Marshal(newLease)

	if hasRow {
		_, err = tx.Exec(ctx, `UPDATE system_settings SET value = $1, updated_at = $2, updated_by = $3 WHERE key = $4`,
			string(newVal), nowTS, nodeID, key)
	} else {
		_, err = tx.Exec(ctx, `INSERT INTO system_settings (key, value, updated_at, updated_by) VALUES ($1, $2, $3, $4)`,
			key, string(newVal), nowTS, nodeID)
	}
	if err != nil {
		return false, err
	}

	err = tx.Commit(ctx)
	if err != nil {
		return false, err
	}
	return true, nil
}

var _ application.Storage = (*Store)(nil)

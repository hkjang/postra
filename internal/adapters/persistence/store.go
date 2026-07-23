// Package persistence implements the storage port on SQLite (pure Go,
// modernc.org/sqlite). PostgreSQL can be added as a second adapter behind
// the same Store surface for server deployments.
package persistence

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"postra/internal/domain"
	"postra/internal/platform/crypto"
)

type Store struct {
	db  *sql.DB
	fts bool
	// kek, when set, encrypts message body columns at rest. The FTS index
	// still holds plaintext (search over ciphertext is out of scope); see
	// EnableEncryption.
	kek *crypto.KEK
}

// Ping verifies the SQLite connection is usable (readiness probe).
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite single-writer; avoids SQLITE_BUSY under workers
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// EnableEncryption turns on at-rest encryption of message body columns
// (text + sanitized HTML). Metadata columns used for search/sort (subject,
// addresses, dates) stay queryable in plaintext by design.
//
// Caveat: with FTS available, the full-text index contains body plaintext.
// For a strict "backup leak → undecryptable" guarantee, disable FTS so
// content search is not indexed at rest.
func (s *Store) EnableEncryption(kek *crypto.KEK) { s.kek = kek }

// RewrapBodies re-encrypts encrypted body columns under the KEK's current
// version (§11.3 회전). No-op when encryption is disabled. Returns the number
// of rows rewrapped.
func (s *Store) RewrapBodies(ctx context.Context) (int, error) {
	if s.kek == nil {
		return 0, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT message_id, text_body, html_sanitized FROM message_bodies`)
	if err != nil {
		return 0, err
	}
	type row struct{ id, text, html string }
	var todo []row
	for rows.Next() {
		var r row
		var html sql.NullString
		if err := rows.Scan(&r.id, &r.text, &html); err != nil {
			rows.Close()
			return 0, err
		}
		r.html = html.String
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
		if _, err := s.db.ExecContext(ctx,
			`UPDATE message_bodies SET text_body=?, html_sanitized=? WHERE message_id=?`,
			sealedText, sealedHTML, r.id); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

const bodyEncPrefix = "enc:v1:"

// sealBody encrypts a body field when a KEK is configured, tagging the
// output so openBody can detect and reverse it. AAD binds the ciphertext to
// its message and field.
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

func (s *Store) migrate() error {
	schema := `
CREATE TABLE IF NOT EXISTS users (
  id TEXT PRIMARY KEY, login_id TEXT UNIQUE NOT NULL,
  status TEXT NOT NULL DEFAULT 'active', timezone TEXT DEFAULT 'UTC',
  display_name TEXT NOT NULL DEFAULT '', email TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL DEFAULT 'user', auth_provider TEXT NOT NULL DEFAULT 'local',
  password_hash TEXT NOT NULL DEFAULT '', oidc_issuer TEXT NOT NULL DEFAULT '',
  oidc_subject TEXT NOT NULL DEFAULT '', updated_at INTEGER NOT NULL DEFAULT 0,
  last_login_at INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_oidc ON users(oidc_issuer, oidc_subject)
  WHERE oidc_subject != '';

CREATE TABLE IF NOT EXISTS auth_sessions (
  id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_hash TEXT UNIQUE NOT NULL, csrf_hash TEXT NOT NULL, expires_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL, last_seen INTEGER NOT NULL,
  user_agent TEXT NOT NULL DEFAULT '', ip_address TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS idx_auth_sessions_expiry ON auth_sessions(expires_at);

CREATE TABLE IF NOT EXISTS system_settings (
  key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at INTEGER NOT NULL,
  updated_by TEXT NOT NULL DEFAULT '');

CREATE TABLE IF NOT EXISTS mcp_keys (
  id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name TEXT NOT NULL, key_hash TEXT UNIQUE NOT NULL, key_prefix TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'active', created_at INTEGER NOT NULL, last_used_at INTEGER DEFAULT 0);
CREATE INDEX IF NOT EXISTS idx_mcp_keys_user ON mcp_keys(user_id);

CREATE TABLE IF NOT EXISTS mail_accounts (
  id TEXT PRIMARY KEY, user_id TEXT NOT NULL, name TEXT NOT NULL,
  email TEXT NOT NULL, status TEXT NOT NULL,
  inbound_protocol TEXT NOT NULL DEFAULT 'pop3',
  pop3_host TEXT, pop3_port INTEGER, pop3_security TEXT, pop3_username TEXT, pop3_secret_ref TEXT,
  smtp_host TEXT, smtp_port INTEGER, smtp_security TEXT, smtp_username TEXT, smtp_auth TEXT, smtp_secret_ref TEXT,
  insecure_skip_verify INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);

CREATE TABLE IF NOT EXISTS credential_refs (
  ref TEXT PRIMARY KEY, owner_id TEXT NOT NULL, secret_type TEXT NOT NULL,
  provider TEXT NOT NULL, label TEXT, status TEXT NOT NULL DEFAULT 'active',
  version INTEGER NOT NULL DEFAULT 1, created_at INTEGER NOT NULL, last_used_at INTEGER);

CREATE TABLE IF NOT EXISTS sync_checkpoints (
  account_id TEXT NOT NULL, uidl TEXT NOT NULL, message_id TEXT,
  synced_at INTEGER NOT NULL, PRIMARY KEY (account_id, uidl));

CREATE TABLE IF NOT EXISTS messages (
  id TEXT PRIMARY KEY, user_id TEXT NOT NULL, account_id TEXT NOT NULL,
  uidl TEXT, message_id_hdr TEXT, subject TEXT, from_name TEXT, from_email TEXT,
  to_json TEXT, cc_json TEXT, reply_to_json TEXT,
  date INTEGER, size INTEGER, raw_hash TEXT NOT NULL, raw_uri TEXT NOT NULL,
  thread_id TEXT, has_attachments INTEGER NOT NULL DEFAULT 0,
  in_reply_to TEXT, refs TEXT, auth_results TEXT, parse_error TEXT,
  created_at INTEGER NOT NULL,
  is_archived INTEGER NOT NULL DEFAULT 0, is_important INTEGER NOT NULL DEFAULT 0,
  snoozed_until INTEGER NOT NULL DEFAULT 0, labels_json TEXT NOT NULL DEFAULT '',
  legal_hold INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS idx_messages_account_date ON messages(account_id, date DESC);
CREATE INDEX IF NOT EXISTS idx_messages_msgid ON messages(message_id_hdr);
CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_hash ON messages(account_id, raw_hash);

CREATE TABLE IF NOT EXISTS message_bodies (
  message_id TEXT PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
  text_body TEXT, html_sanitized TEXT, charset TEXT);

CREATE TABLE IF NOT EXISTS attachments (
  id TEXT PRIMARY KEY, message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  name TEXT, mime_type TEXT, size INTEGER, hash TEXT, storage_uri TEXT, inline_flag INTEGER DEFAULT 0,
  scan_status TEXT DEFAULT 'clean', scan_detail TEXT);

CREATE TABLE IF NOT EXISTS threads (
  id TEXT PRIMARY KEY, user_id TEXT NOT NULL, account_id TEXT NOT NULL,
  subject_key TEXT, last_message_at INTEGER, message_count INTEGER DEFAULT 0);
CREATE INDEX IF NOT EXISTS idx_threads_key ON threads(account_id, subject_key);

CREATE TABLE IF NOT EXISTS analyses (
  id TEXT PRIMARY KEY, user_id TEXT NOT NULL, target_type TEXT NOT NULL,
  target_id TEXT NOT NULL, analysis_type TEXT NOT NULL, result_json TEXT NOT NULL,
  model TEXT, prompt_version TEXT, input_hash TEXT, created_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS idx_analyses_cache ON analyses(analysis_type, input_hash, model);

CREATE TABLE IF NOT EXISTS drafts (
  id TEXT PRIMARY KEY, user_id TEXT NOT NULL, account_id TEXT NOT NULL,
  kind TEXT NOT NULL, reply_to_message_id TEXT, status TEXT NOT NULL,
  current_version INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);

CREATE TABLE IF NOT EXISTS draft_versions (
  draft_id TEXT NOT NULL REFERENCES drafts(id) ON DELETE CASCADE,
  version INTEGER NOT NULL, subject TEXT, body_text TEXT, body_html TEXT,
  to_json TEXT, cc_json TEXT, bcc_json TEXT, author TEXT NOT NULL,
  created_at INTEGER NOT NULL, PRIMARY KEY (draft_id, version));

CREATE TABLE IF NOT EXISTS approvals (
  id TEXT PRIMARY KEY, user_id TEXT NOT NULL, action_type TEXT NOT NULL,
  draft_id TEXT, draft_version INTEGER, payload_hash TEXT NOT NULL,
  token_hash TEXT NOT NULL, approver TEXT, expires_at INTEGER NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending', created_at INTEGER NOT NULL);

CREATE TABLE IF NOT EXISTS outbound_messages (
  id TEXT PRIMARY KEY, user_id TEXT NOT NULL, draft_id TEXT NOT NULL,
  draft_version INTEGER NOT NULL, idempotency_key TEXT UNIQUE,
  message_id_hdr TEXT, status TEXT NOT NULL, smtp_response TEXT,
  attempts INTEGER NOT NULL DEFAULT 0, next_attempt_at INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS idx_outbound_retry ON outbound_messages(status, next_attempt_at);

CREATE TABLE IF NOT EXISTS jobs (
  id TEXT PRIMARY KEY, user_id TEXT NOT NULL, type TEXT NOT NULL,
  account_id TEXT, status TEXT NOT NULL, progress TEXT,
  stats_json TEXT, error TEXT, meta_json TEXT,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);

CREATE TABLE IF NOT EXISTS embeddings (
  message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  chunk_id INTEGER NOT NULL, user_id TEXT NOT NULL, account_id TEXT NOT NULL,
  model TEXT NOT NULL, dim INTEGER NOT NULL, vec BLOB NOT NULL,
  PRIMARY KEY (message_id, chunk_id));
CREATE INDEX IF NOT EXISTS idx_embeddings_scope ON embeddings(user_id, account_id);

CREATE TABLE IF NOT EXISTS audit_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT, at INTEGER NOT NULL,
  user_id TEXT, actor TEXT, action TEXT NOT NULL, resource TEXT,
  result TEXT NOT NULL, detail TEXT);
CREATE INDEX IF NOT EXISTS idx_audit_at ON audit_events(at DESC);

CREATE TABLE IF NOT EXISTS mail_rules (
  id TEXT PRIMARY KEY, user_id TEXT NOT NULL, name TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1, priority INTEGER NOT NULL DEFAULT 100,
  match_mode TEXT NOT NULL DEFAULT 'all', conditions_json TEXT NOT NULL DEFAULT '[]',
  actions_json TEXT NOT NULL DEFAULT '[]', stop_on_match INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS idx_mail_rules_user ON mail_rules(user_id, priority);

CREATE TABLE IF NOT EXISTS action_cards (
  id TEXT PRIMARY KEY, user_id TEXT NOT NULL, message_id TEXT NOT NULL,
  type TEXT NOT NULL, title TEXT NOT NULL, detail TEXT, due TEXT, assignee TEXT,
  status TEXT NOT NULL DEFAULT 'pending', export_target TEXT, external_ref TEXT,
  confidence REAL DEFAULT 0, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS idx_action_cards_user ON action_cards(user_id, status);

CREATE TABLE IF NOT EXISTS message_collab (
  message_id TEXT PRIMARY KEY, user_id TEXT NOT NULL, assignee TEXT,
  status TEXT NOT NULL DEFAULT 'open', sla_due INTEGER NOT NULL DEFAULT 0,
  updated_by TEXT, updated_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS idx_message_collab_user ON message_collab(user_id, status);

CREATE TABLE IF NOT EXISTS message_notes (
  id TEXT PRIMARY KEY, message_id TEXT NOT NULL, user_id TEXT NOT NULL,
  author TEXT NOT NULL, body TEXT NOT NULL, created_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS idx_message_notes_msg ON message_notes(message_id);
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	// Additive column migrations for databases created before these columns
	// existed. Fresh DBs already have them from CREATE, so a "duplicate
	// column" error is expected and ignored.
	for _, alt := range []string{
		`ALTER TABLE attachments ADD COLUMN scan_status TEXT DEFAULT 'clean'`,
		`ALTER TABLE attachments ADD COLUMN scan_detail TEXT`,
		`ALTER TABLE outbound_messages ADD COLUMN next_attempt_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE mail_accounts ADD COLUMN inbound_protocol TEXT NOT NULL DEFAULT 'pop3'`,
		`ALTER TABLE users ADD COLUMN display_name TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE users ADD COLUMN email TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'user'`,
		`ALTER TABLE users ADD COLUMN auth_provider TEXT NOT NULL DEFAULT 'local'`,
		`ALTER TABLE users ADD COLUMN password_hash TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE users ADD COLUMN oidc_issuer TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE users ADD COLUMN oidc_subject TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE users ADD COLUMN updated_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE users ADD COLUMN last_login_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE messages ADD COLUMN is_archived INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE messages ADD COLUMN is_important INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE messages ADD COLUMN snoozed_until INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE messages ADD COLUMN labels_json TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE messages ADD COLUMN legal_hold INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := s.db.Exec(alt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("migrate attachments: %w", err)
		}
	}
	if _, err := s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_oidc ON users(oidc_issuer, oidc_subject) WHERE oidc_subject != ''`); err != nil {
		return fmt.Errorf("migrate users oidc index: %w", err)
	}

	// Full-text index; fall back to LIKE search when FTS5 is unavailable.
	_, err := s.db.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
      message_pk UNINDEXED, subject, from_email, text_body)`)
	s.fts = err == nil
	return nil
}

func NewID(prefix string) string {
	b := make([]byte, 10)
	rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

func now() int64 { return time.Now().Unix() }

// ErrNotFound aliases the canonical port error so existing callers keep
// working while transports match on domain.ErrNotFound.
var ErrNotFound = domain.ErrNotFound

// ---------- users ----------

func (s *Store) EnsureUser(ctx context.Context, id, loginID string) error {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=? OR lower(login_id)=lower(?))`, id, loginID).Scan(&exists)
	if err == nil && exists {
		return nil
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO users (id, login_id, display_name, role, status, auth_provider, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO NOTHING`,
		id, loginID, loginID, domain.RoleUser, domain.UserActive, "local", now(), now())
	if err != nil && (strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "constraint")) {
		return nil
	}
	return err
}

const userCols = `id,login_id,display_name,email,role,status,auth_provider,
 oidc_issuer,oidc_subject,created_at,updated_at,last_login_at`

func scanUser(row interface{ Scan(...any) error }) (*domain.User, error) {
	var u domain.User
	if err := row.Scan(&u.ID, &u.LoginID, &u.DisplayName, &u.Email, &u.Role, &u.Status,
		&u.AuthProvider, &u.OIDCIssuer, &u.OIDCSubject, &u.CreatedAt, &u.UpdatedAt, &u.LastLoginAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &u, nil
}

func (s *Store) CreateUser(ctx context.Context, u *domain.User, passwordHash string) error {
	u.CreatedAt, u.UpdatedAt = now(), now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO users
	 (id,login_id,display_name,email,role,status,auth_provider,password_hash,oidc_issuer,oidc_subject,created_at,updated_at,last_login_at)
	 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		u.ID, u.LoginID, u.DisplayName, u.Email, u.Role, u.Status, u.AuthProvider,
		passwordHash, u.OIDCIssuer, u.OIDCSubject, u.CreatedAt, u.UpdatedAt, u.LastLoginAt)
	return err
}

func (s *Store) GetUser(ctx context.Context, id string) (*domain.User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id=?`, id))
}

func (s *Store) GetUserByLogin(ctx context.Context, loginID string) (*domain.User, string, error) {
	var u domain.User
	var passwordHash string
	err := s.db.QueryRowContext(ctx, `SELECT `+userCols+`,password_hash FROM users WHERE lower(login_id)=lower(?)`, loginID).
		Scan(&u.ID, &u.LoginID, &u.DisplayName, &u.Email, &u.Role, &u.Status, &u.AuthProvider,
			&u.OIDCIssuer, &u.OIDCSubject, &u.CreatedAt, &u.UpdatedAt, &u.LastLoginAt, &passwordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	return &u, passwordHash, err
}

func (s *Store) GetUserByOIDC(ctx context.Context, issuer, subject string) (*domain.User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE oidc_issuer=? AND oidc_subject=?`, issuer, subject))
}

// UpdateMessage persists the mutable UX-state columns (archive/important/
// snooze/labels). It never silently falls back to a no-op update: a failure
// here (e.g. an un-migrated column) is surfaced so callers cannot report a
// success that did not happen.
func (s *Store) UpdateMessage(ctx context.Context, m *domain.Message) error {
	labels := m.Labels
	if labels == nil {
		labels = []string{}
	}
	labelsJSON, err := json.Marshal(labels)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE messages SET is_archived=?, is_important=?, snoozed_until=?, labels_json=?, legal_hold=? WHERE id=? AND user_id=?`,
		boolInt(m.IsArchived), boolInt(m.IsImportant), m.SnoozedUntil, string(labelsJSON), boolInt(m.LegalHold), m.ID, m.UserID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ListUsers(ctx context.Context) ([]domain.User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userCols+` FROM users ORDER BY login_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

func (s *Store) UpdateUser(ctx context.Context, u *domain.User) error {
	u.UpdatedAt = now()
	res, err := s.db.ExecContext(ctx, `UPDATE users SET login_id=?,display_name=?,email=?,role=?,status=?,
	 auth_provider=?,oidc_issuer=?,oidc_subject=?,updated_at=?,last_login_at=? WHERE id=?`,
		u.LoginID, u.DisplayName, u.Email, u.Role, u.Status, u.AuthProvider,
		u.OIDCIssuer, u.OIDCSubject, u.UpdatedAt, u.LastLoginAt, u.ID)
	if err == nil {
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
	}
	return err
}

func (s *Store) SetUserPassword(ctx context.Context, userID, passwordHash string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET password_hash=?,auth_provider='local',updated_at=? WHERE id=?`,
		passwordHash, now(), userID)
	if err == nil {
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
	}
	return err
}

func (s *Store) CountAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE role='admin' AND status='active'`).Scan(&n)
	return n, err
}

func (s *Store) CreateSession(ctx context.Context, ss *domain.Session) error {
	ss.CreatedAt, ss.LastSeen = now(), now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO auth_sessions
	 (id,user_id,token_hash,csrf_hash,expires_at,created_at,last_seen,user_agent,ip_address)
	 VALUES (?,?,?,?,?,?,?,?,?)`, ss.ID, ss.UserID, ss.TokenHash, ss.CSRFHash,
		ss.ExpiresAt, ss.CreatedAt, ss.LastSeen, ss.UserAgent, ss.IPAddress)
	return err
}

func (s *Store) GetSessionByTokenHash(ctx context.Context, tokenHash string) (*domain.Session, *domain.User, error) {
	var ss domain.Session
	var u domain.User
	err := s.db.QueryRowContext(ctx, `SELECT s.id,s.user_id,s.token_hash,s.csrf_hash,s.expires_at,s.created_at,s.last_seen,
	 s.user_agent,s.ip_address,`+prefixCols(userCols, "u.")+`
	 FROM auth_sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=? AND s.expires_at>?`,
		tokenHash, now()).Scan(&ss.ID, &ss.UserID, &ss.TokenHash, &ss.CSRFHash, &ss.ExpiresAt,
		&ss.CreatedAt, &ss.LastSeen, &ss.UserAgent, &ss.IPAddress,
		&u.ID, &u.LoginID, &u.DisplayName, &u.Email, &u.Role, &u.Status, &u.AuthProvider,
		&u.OIDCIssuer, &u.OIDCSubject, &u.CreatedAt, &u.UpdatedAt, &u.LastLoginAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	return &ss, &u, err
}

func (s *Store) TouchSession(ctx context.Context, id string, lastSeen int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE auth_sessions SET last_seen=? WHERE id=?`, lastSeen, id)
	return err
}

func (s *Store) DeleteSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM auth_sessions WHERE id=?`, id)
	return err
}

func (s *Store) DeleteUserSessions(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM auth_sessions WHERE user_id=?`, userID)
	return err
}

func (s *Store) GetSettings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key,value FROM system_settings`)
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for key, value := range values {
		if _, err := tx.ExecContext(ctx, `INSERT INTO system_settings(key,value,updated_at)
		 VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`,
			key, value, now()); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) GetOrCreateSetting(ctx context.Context, key, candidate string) (string, error) {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO system_settings(key,value,updated_at) VALUES(?,?,?)
	 ON CONFLICT(key) DO NOTHING`, key, candidate, now()); err != nil {
		return "", err
	}
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM system_settings WHERE key=?`, key).Scan(&value)
	return value, err
}

// ---------- accounts ----------

func (s *Store) CreateAccount(ctx context.Context, a *domain.MailAccount) error {
	a.CreatedAt, a.UpdatedAt = now(), now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO mail_accounts
	 (id,user_id,name,email,status,inbound_protocol,pop3_host,pop3_port,pop3_security,pop3_username,pop3_secret_ref,
	  smtp_host,smtp_port,smtp_security,smtp_username,smtp_auth,smtp_secret_ref,insecure_skip_verify,created_at,updated_at)
	 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.UserID, a.Name, a.Email, a.Status, a.InboundProtocol,
		a.POP3Host, a.POP3Port, a.POP3Security, a.POP3Username, string(a.POP3Secret),
		a.SMTPHost, a.SMTPPort, a.SMTPSecurity, a.SMTPUsername, a.SMTPAuth, string(a.SMTPSecret),
		boolInt(a.InsecureSkipVerify), a.CreatedAt, a.UpdatedAt)
	return err
}

func (s *Store) scanAccount(row interface{ Scan(...any) error }) (*domain.MailAccount, error) {
	var a domain.MailAccount
	var pop3Ref, smtpRef string
	var insecure int
	err := row.Scan(&a.ID, &a.UserID, &a.Name, &a.Email, &a.Status, &a.InboundProtocol,
		&a.POP3Host, &a.POP3Port, &a.POP3Security, &a.POP3Username, &pop3Ref,
		&a.SMTPHost, &a.SMTPPort, &a.SMTPSecurity, &a.SMTPUsername, &a.SMTPAuth, &smtpRef,
		&insecure, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.POP3Secret = domain.SecretRef(pop3Ref)
	a.SMTPSecret = domain.SecretRef(smtpRef)
	a.InsecureSkipVerify = insecure != 0
	return &a, nil
}

const accountCols = `id,user_id,name,email,status,inbound_protocol,pop3_host,pop3_port,pop3_security,pop3_username,pop3_secret_ref,
 smtp_host,smtp_port,smtp_security,smtp_username,smtp_auth,smtp_secret_ref,insecure_skip_verify,created_at,updated_at`

func (s *Store) GetAccount(ctx context.Context, userID, id string) (*domain.MailAccount, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+accountCols+` FROM mail_accounts WHERE id=? AND user_id=?`, id, userID)
	return s.scanAccount(row)
}

func (s *Store) ListAccounts(ctx context.Context, userID string) ([]domain.MailAccount, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+accountCols+` FROM mail_accounts WHERE user_id=? AND status<>? ORDER BY created_at`,
		userID, domain.AccountDeleted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.MailAccount
	for rows.Next() {
		a, err := s.scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

func (s *Store) UpdateAccount(ctx context.Context, a *domain.MailAccount) error {
	a.UpdatedAt = now()
	res, err := s.db.ExecContext(ctx, `UPDATE mail_accounts SET
	 name=?,email=?,status=?,inbound_protocol=?,pop3_host=?,pop3_port=?,pop3_security=?,pop3_username=?,pop3_secret_ref=?,
	 smtp_host=?,smtp_port=?,smtp_security=?,smtp_username=?,smtp_auth=?,smtp_secret_ref=?,
	 insecure_skip_verify=?,updated_at=? WHERE id=? AND user_id=?`,
		a.Name, a.Email, a.Status, a.InboundProtocol,
		a.POP3Host, a.POP3Port, a.POP3Security, a.POP3Username, string(a.POP3Secret),
		a.SMTPHost, a.SMTPPort, a.SMTPSecurity, a.SMTPUsername, a.SMTPAuth, string(a.SMTPSecret),
		boolInt(a.InsecureSkipVerify), a.UpdatedAt, a.ID, a.UserID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SetAccountStatus(ctx context.Context, userID, id string, st domain.AccountStatus) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE mail_accounts SET status=?, updated_at=? WHERE id=? AND user_id=?`, st, now(), id, userID)
	return err
}

// ---------- credential refs ----------

func (s *Store) PutCredentialRef(ctx context.Context, c domain.CredentialRef) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO credential_refs
	 (ref,owner_id,secret_type,provider,label,status,version,created_at) VALUES (?,?,?,?,?,?,?,?)
	 ON CONFLICT(ref) DO UPDATE SET status=excluded.status, version=excluded.version`,
		string(c.Ref), c.OwnerID, c.Type, c.Provider, c.Label, c.Status, c.Version, now())
	return err
}

func (s *Store) TouchCredential(ctx context.Context, ref domain.SecretRef) {
	s.db.ExecContext(ctx, `UPDATE credential_refs SET last_used_at=? WHERE ref=?`, now(), string(ref))
}

func (s *Store) SetCredentialStatus(ctx context.Context, ref domain.SecretRef, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE credential_refs SET status=? WHERE ref=?`, status, string(ref))
	return err
}

// ---------- sync checkpoints ----------

func (s *Store) HasCheckpoint(ctx context.Context, accountID, uidl string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM sync_checkpoints WHERE account_id=? AND uidl=?`, accountID, uidl).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) AddCheckpoint(ctx context.Context, accountID, uidl, messageID string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sync_checkpoints (account_id,uidl,message_id,synced_at)
	 VALUES (?,?,?,?) ON CONFLICT DO NOTHING`, accountID, uidl, messageID, now())
	return err
}

// ---------- messages ----------

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

// InsertMessage stores a message + body + attachments atomically and updates
// the FTS index. Duplicate raw hashes for the same account are rejected via
// the unique index (POP-012 backstop when UIDL is unreliable).
func (s *Store) InsertMessage(ctx context.Context, m *domain.Message, body *domain.MessageBody, atts []domain.Attachment) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	m.CreatedAt = now()
	_, err = tx.ExecContext(ctx, `INSERT INTO messages
	 (id,user_id,account_id,uidl,message_id_hdr,subject,from_name,from_email,to_json,cc_json,reply_to_json,
	  date,size,raw_hash,raw_uri,thread_id,has_attachments,in_reply_to,refs,auth_results,parse_error,created_at)
	 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.ID, m.UserID, m.AccountID, m.UIDL, m.MessageID, m.Subject, m.From.Name, m.From.Email,
		addrJSON(m.To), addrJSON(m.Cc), addrJSON(m.ReplyTo),
		m.Date, m.Size, m.RawHash, m.RawURI, m.ThreadID, boolInt(m.HasAttachments),
		m.InReplyTo, m.References, m.AuthResults, m.ParseError, m.CreatedAt)
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
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO message_bodies (message_id,text_body,html_sanitized,charset) VALUES (?,?,?,?)`,
			m.ID, sealedText, sealedHTML, body.Charset); err != nil {
			return err
		}
	}
	for _, at := range atts {
		status := at.ScanStatus
		if status == "" {
			status = domain.ScanClean
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO attachments (id,message_id,name,mime_type,size,hash,storage_uri,inline_flag,scan_status,scan_detail)
			 VALUES (?,?,?,?,?,?,?,?,?,?)`,
			at.ID, m.ID, at.Name, at.MIMEType, at.Size, at.Hash, at.StorageURI, boolInt(at.Inline),
			string(status), at.ScanDetail); err != nil {
			return err
		}
	}
	if s.fts {
		text := ""
		if body != nil {
			text = body.TextBody
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO messages_fts (message_pk,subject,from_email,text_body) VALUES (?,?,?,?)`,
			m.ID, m.Subject, m.From.Email, text); err != nil {
			return err
		}
	}
	if m.ThreadID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE threads SET last_message_at=MAX(last_message_at,?),
		 message_count=message_count+1 WHERE id=?`, m.Date, m.ThreadID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteMessage removes a message and its dependents within the user's scope
// and returns the object URIs (raw + attachments) that became UNREFERENCED
// and are therefore safe to purge from the object store. Content-addressed
// blobs can be shared across messages, so an object is returned only when no
// surviving message references it (fixes the shared-blob deletion hazard).
func (s *Store) DeleteMessage(ctx context.Context, userID, id string) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var rawURI string
	err = tx.QueryRowContext(ctx, `SELECT raw_uri FROM messages WHERE id=? AND user_id=?`, id, userID).Scan(&rawURI)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	candidates := []string{rawURI}
	rows, err := tx.QueryContext(ctx, `SELECT storage_uri FROM attachments WHERE message_id=?`, id)
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

	if s.fts {
		if _, err := tx.ExecContext(ctx, `DELETE FROM messages_fts WHERE message_pk=?`, id); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE id=? AND user_id=?`, id, userID); err != nil {
		return nil, err
	}

	// A blob is orphaned only if no OTHER message (or its attachments) still
	// points at the same URI after this deletion.
	var orphans []string
	for _, u := range candidates {
		var refs int
		if err := tx.QueryRowContext(ctx,
			`SELECT (SELECT COUNT(*) FROM messages WHERE raw_uri=?) +
			        (SELECT COUNT(*) FROM attachments WHERE storage_uri=?)`, u, u).Scan(&refs); err != nil {
			return nil, err
		}
		if refs == 0 {
			orphans = append(orphans, u)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return orphans, nil
}

// StoredUIDLs returns the set of UIDLs already stored locally for an account
// (checkpoints), used to compute server-delete eligibility.
func (s *Store) StoredUIDLs(ctx context.Context, accountID string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT uidl FROM sync_checkpoints WHERE account_id=?`, accountID)
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

func (s *Store) IsDuplicateHash(ctx context.Context, accountID, rawHash string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM messages WHERE account_id=? AND raw_hash=?`, accountID, rawHash).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

const msgCols = `id,user_id,account_id,uidl,message_id_hdr,subject,from_name,from_email,to_json,cc_json,reply_to_json,
 date,size,raw_hash,raw_uri,thread_id,has_attachments,in_reply_to,refs,auth_results,parse_error,created_at`

// msgSelectCols extends the insert column set with the mutable UX-state columns
// (archive/important/snooze/labels). It is used for every SELECT + scanMessage;
// msgCols stays the immutable ingest set used by InsertMessage.
const msgSelectCols = msgCols + `,is_archived,is_important,snoozed_until,labels_json,legal_hold`

func labelsFromJSON(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

func scanMessage(row interface{ Scan(...any) error }) (*domain.Message, error) {
	var m domain.Message
	var toJ, ccJ, rtJ string
	var hasAtt int
	var isArch, isImp, legalHold int
	var labelsJSON string
	var threadID, parseErr, authRes sql.NullString
	err := row.Scan(&m.ID, &m.UserID, &m.AccountID, &m.UIDL, &m.MessageID, &m.Subject,
		&m.From.Name, &m.From.Email, &toJ, &ccJ, &rtJ,
		&m.Date, &m.Size, &m.RawHash, &m.RawURI, &threadID, &hasAtt,
		&m.InReplyTo, &m.References, &authRes, &parseErr, &m.CreatedAt,
		&isArch, &isImp, &m.SnoozedUntil, &labelsJSON, &legalHold)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	m.To, m.Cc, m.ReplyTo = addrFromJSON(toJ), addrFromJSON(ccJ), addrFromJSON(rtJ)
	m.HasAttachments = hasAtt != 0
	m.IsArchived, m.IsImportant, m.LegalHold = isArch != 0, isImp != 0, legalHold != 0
	m.Labels = labelsFromJSON(labelsJSON)
	m.ThreadID, m.ParseError, m.AuthResults = threadID.String, parseErr.String, authRes.String
	return &m, nil
}

func (s *Store) GetMessage(ctx context.Context, userID, id string) (*domain.Message, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+msgSelectCols+` FROM messages WHERE id=? AND user_id=?`, id, userID)
	return scanMessage(row)
}

func (s *Store) GetMessageByHeader(ctx context.Context, userID, accountID, messageIDHdr string) (*domain.Message, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+msgSelectCols+` FROM messages WHERE user_id=? AND account_id=? AND message_id_hdr=? LIMIT 1`,
		userID, accountID, messageIDHdr)
	return scanMessage(row)
}

func (s *Store) GetBody(ctx context.Context, userID, messageID string) (*domain.MessageBody, error) {
	var b domain.MessageBody
	var html, charset sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT b.message_id, b.text_body, b.html_sanitized, b.charset
	 FROM message_bodies b JOIN messages m ON m.id=b.message_id
	 WHERE b.message_id=? AND m.user_id=?`, messageID, userID).
		Scan(&b.MessageID, &b.TextBody, &html, &charset)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if b.TextBody, err = s.openBody(messageID, "text", b.TextBody); err != nil {
		return nil, err
	}
	if b.HTMLSanitized, err = s.openBody(messageID, "html", html.String); err != nil {
		return nil, err
	}
	b.Charset = charset.String
	return &b, nil
}

func (s *Store) ListAttachments(ctx context.Context, userID, messageID string) ([]domain.Attachment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.id,a.message_id,a.name,a.mime_type,a.size,a.hash,a.storage_uri,a.inline_flag,
	 COALESCE(a.scan_status,'clean'),COALESCE(a.scan_detail,'')
	 FROM attachments a JOIN messages m ON m.id=a.message_id
	 WHERE a.message_id=? AND m.user_id=?`, messageID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Attachment
	for rows.Next() {
		var a domain.Attachment
		var inline int
		var status string
		if err := rows.Scan(&a.ID, &a.MessageID, &a.Name, &a.MIMEType, &a.Size, &a.Hash, &a.StorageURI, &inline, &status, &a.ScanDetail); err != nil {
			return nil, err
		}
		a.Inline = inline != 0
		a.ScanStatus = domain.ScanStatus(status)
		out = append(out, a)
	}
	return out, rows.Err()
}

// Search runs filtered keyword search with keyset (cursor) pagination.
// Cursor format: "<date>:<id>" of the last row from the previous page.
func (s *Store) Search(ctx context.Context, q domain.SearchQuery) (*domain.SearchResult, error) {
	limit := q.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var conds []string
	var args []any
	conds, args = append(conds, "m.user_id=?"), append(args, q.UserID)
	if q.AccountID != "" {
		conds, args = append(conds, "m.account_id=?"), append(args, q.AccountID)
	}
	if q.From != "" {
		conds, args = append(conds, "m.from_email LIKE ?"), append(args, "%"+q.From+"%")
	}
	if q.To != "" {
		conds, args = append(conds, "m.to_json LIKE ?"), append(args, "%"+q.To+"%")
	}
	if q.Subject != "" {
		conds, args = append(conds, "m.subject LIKE ?"), append(args, "%"+q.Subject+"%")
	}
	if q.Since > 0 {
		conds, args = append(conds, "m.date >= ?"), append(args, q.Since)
	}
	if q.Until > 0 {
		conds, args = append(conds, "m.date <= ?"), append(args, q.Until)
	}
	if q.HasAttachment != nil {
		conds, args = append(conds, "m.has_attachments = ?"), append(args, boolInt(*q.HasAttachment))
	}
	if q.IsImportant != nil {
		conds, args = append(conds, "m.is_important = ?"), append(args, boolInt(*q.IsImportant))
	}
	if q.IsArchived != nil {
		conds, args = append(conds, "m.is_archived = ?"), append(args, boolInt(*q.IsArchived))
	}
	if q.Label != "" {
		conds, args = append(conds, "m.labels_json LIKE ?"), append(args, `%"`+q.Label+`"%`)
	}
	switch q.Folder {
	case "inbox":
		conds, args = append(conds, "m.is_archived = 0 AND (m.snoozed_until = 0 OR m.snoozed_until <= ?)"), append(args, now())
	case "archive":
		conds = append(conds, "m.is_archived = 1")
	case "important":
		conds = append(conds, "m.is_important = 1")
	case "snoozed":
		conds, args = append(conds, "m.snoozed_until > ?"), append(args, now())
	}
	if q.Cursor != "" {
		var cDate int64
		var cID string
		if _, err := fmt.Sscanf(q.Cursor, "%d:%s", &cDate, &cID); err == nil {
			conds, args = append(conds, "(m.date < ? OR (m.date = ? AND m.id < ?))"),
				append(args, cDate, cDate, cID)
		}
	}
	join := ""
	if q.Text != "" {
		if s.fts {
			join = " JOIN messages_fts f ON f.message_pk = m.id "
			conds, args = append(conds, "messages_fts MATCH ?"), append(args, ftsQuery(q.Text))
		} else {
			conds = append(conds, `(m.subject LIKE ? OR m.from_email LIKE ? OR EXISTS
			 (SELECT 1 FROM message_bodies b WHERE b.message_id=m.id AND b.text_body LIKE ?))`)
			p := "%" + q.Text + "%"
			args = append(args, p, p, p)
		}
	}
	// #nosec G202 -- concatenated fragments are all static (column list, join, hardcoded conditions); every user value is bound via ? placeholders in args.
	query := `SELECT ` + prefixCols(msgSelectCols, "m.") + ` FROM messages m` + join +
		` WHERE ` + strings.Join(conds, " AND ") +
		` ORDER BY m.date DESC, m.id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
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

// ftsQuery quotes user text so FTS5 operators in mail-derived strings can't
// change query semantics.
func ftsQuery(text string) string {
	terms := strings.Fields(text)
	for i, t := range terms {
		terms[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
	}
	return strings.Join(terms, " ")
}

// ---------- threads ----------

// ResolveThread finds the thread for a new message via In-Reply-To /
// References (MIME-006), falling back to the normalized subject key
// (MIME-007). Creates a thread when none matches.
func (s *Store) ResolveThread(ctx context.Context, userID, accountID string, refs []string, subjectKey string, date int64) (string, error) {
	for _, r := range refs {
		if r == "" {
			continue
		}
		var tid sql.NullString
		err := s.db.QueryRowContext(ctx,
			`SELECT thread_id FROM messages WHERE user_id=? AND account_id=? AND message_id_hdr=? AND thread_id != '' LIMIT 1`,
			userID, accountID, r).Scan(&tid)
		if err == nil && tid.String != "" {
			return tid.String, nil
		}
	}
	if subjectKey != "" && len(refs) > 0 {
		var tid string
		err := s.db.QueryRowContext(ctx,
			`SELECT id FROM threads WHERE user_id=? AND account_id=? AND subject_key=? LIMIT 1`,
			userID, accountID, subjectKey).Scan(&tid)
		if err == nil {
			return tid, nil
		}
	}
	tid := NewID("thr")
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO threads (id,user_id,account_id,subject_key,last_message_at,message_count) VALUES (?,?,?,?,?,0)`,
		tid, userID, accountID, subjectKey, date)
	return tid, err
}

func (s *Store) GetThreadMessages(ctx context.Context, userID, threadID string) ([]domain.Message, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+msgSelectCols+` FROM messages WHERE user_id=? AND thread_id=? ORDER BY date ASC`, userID, threadID)
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

func (s *Store) CreateRule(ctx context.Context, r *domain.MailRule) error {
	r.CreatedAt, r.UpdatedAt = now(), now()
	conds, _ := json.Marshal(r.Conditions)
	acts, _ := json.Marshal(r.Actions)
	_, err := s.db.ExecContext(ctx, `INSERT INTO mail_rules
	 (id,user_id,name,enabled,priority,match_mode,conditions_json,actions_json,stop_on_match,created_at,updated_at)
	 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.UserID, r.Name, boolInt(r.Enabled), r.Priority, r.Match,
		string(conds), string(acts), boolInt(r.StopOnMatch), r.CreatedAt, r.UpdatedAt)
	return err
}

func (s *Store) UpdateRule(ctx context.Context, r *domain.MailRule) error {
	r.UpdatedAt = now()
	conds, _ := json.Marshal(r.Conditions)
	acts, _ := json.Marshal(r.Actions)
	res, err := s.db.ExecContext(ctx, `UPDATE mail_rules SET name=?,enabled=?,priority=?,match_mode=?,
	 conditions_json=?,actions_json=?,stop_on_match=?,updated_at=? WHERE id=? AND user_id=?`,
		r.Name, boolInt(r.Enabled), r.Priority, r.Match, string(conds), string(acts),
		boolInt(r.StopOnMatch), r.UpdatedAt, r.ID, r.UserID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteRule(ctx context.Context, userID, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM mail_rules WHERE id=? AND user_id=?`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanRule(row interface{ Scan(...any) error }) (*domain.MailRule, error) {
	var r domain.MailRule
	var enabled, stop int
	var conds, acts string
	if err := row.Scan(&r.ID, &r.UserID, &r.Name, &enabled, &r.Priority, &r.Match,
		&conds, &acts, &stop, &r.CreatedAt, &r.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	r.Enabled, r.StopOnMatch = enabled != 0, stop != 0
	_ = json.Unmarshal([]byte(conds), &r.Conditions)
	_ = json.Unmarshal([]byte(acts), &r.Actions)
	return &r, nil
}

const ruleCols = `id,user_id,name,enabled,priority,match_mode,conditions_json,actions_json,stop_on_match,created_at,updated_at`

func (s *Store) GetRule(ctx context.Context, userID, id string) (*domain.MailRule, error) {
	return scanRule(s.db.QueryRowContext(ctx, `SELECT `+ruleCols+` FROM mail_rules WHERE id=? AND user_id=?`, id, userID))
}

func (s *Store) ListRules(ctx context.Context, userID string) ([]domain.MailRule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+ruleCols+` FROM mail_rules WHERE user_id=? ORDER BY priority, created_at`, userID)
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
	_, err := s.db.ExecContext(ctx, `INSERT INTO action_cards
	 (id,user_id,message_id,type,title,detail,due,assignee,status,export_target,external_ref,confidence,created_at,updated_at)
	 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.ID, c.UserID, c.MessageID, c.Type, c.Title, c.Detail, c.Due, c.Assignee,
		c.Status, c.ExportTarget, c.ExternalRef, c.Confidence, c.CreatedAt, c.UpdatedAt)
	return err
}

func (s *Store) UpdateActionCard(ctx context.Context, c *domain.ActionCard) error {
	c.UpdatedAt = now()
	res, err := s.db.ExecContext(ctx, `UPDATE action_cards SET type=?,title=?,detail=?,due=?,assignee=?,
	 status=?,export_target=?,external_ref=?,confidence=?,updated_at=? WHERE id=? AND user_id=?`,
		c.Type, c.Title, c.Detail, c.Due, c.Assignee, c.Status, c.ExportTarget, c.ExternalRef,
		c.Confidence, c.UpdatedAt, c.ID, c.UserID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanActionCard(row interface{ Scan(...any) error }) (*domain.ActionCard, error) {
	var c domain.ActionCard
	if err := row.Scan(&c.ID, &c.UserID, &c.MessageID, &c.Type, &c.Title, &c.Detail, &c.Due,
		&c.Assignee, &c.Status, &c.ExportTarget, &c.ExternalRef, &c.Confidence, &c.CreatedAt, &c.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &c, nil
}

func (s *Store) GetActionCard(ctx context.Context, userID, id string) (*domain.ActionCard, error) {
	return scanActionCard(s.db.QueryRowContext(ctx, `SELECT `+actionCardCols+` FROM action_cards WHERE id=? AND user_id=?`, id, userID))
}

func (s *Store) ListActionCards(ctx context.Context, userID, status string, limit int) ([]domain.ActionCard, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT ` + actionCardCols + ` FROM action_cards WHERE user_id=?`
	args := []any{userID}
	if status != "" {
		query += ` AND status=?`
		args = append(args, status)
	}
	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
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
	_, err := s.db.ExecContext(ctx, `INSERT INTO message_collab
	 (message_id,user_id,assignee,status,sla_due,updated_by,updated_at) VALUES (?,?,?,?,?,?,?)
	 ON CONFLICT(message_id) DO UPDATE SET assignee=excluded.assignee, status=excluded.status,
	   sla_due=excluded.sla_due, updated_by=excluded.updated_by, updated_at=excluded.updated_at`,
		mc.MessageID, mc.UserID, mc.Assignee, mc.Status, mc.SLADue, mc.UpdatedBy, mc.UpdatedAt)
	return err
}

func (s *Store) GetMessageCollab(ctx context.Context, userID, messageID string) (*domain.MessageCollab, error) {
	var mc domain.MessageCollab
	var assignee, updatedBy sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT message_id,user_id,COALESCE(assignee,''),status,sla_due,COALESCE(updated_by,''),updated_at
	 FROM message_collab WHERE message_id=? AND user_id=?`, messageID, userID).
		Scan(&mc.MessageID, &mc.UserID, &assignee, &mc.Status, &mc.SLADue, &updatedBy, &mc.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	mc.Assignee, mc.UpdatedBy = assignee.String, updatedBy.String
	return &mc, nil
}

func (s *Store) ListMessageCollab(ctx context.Context, userID, status, assignee string, limit int) ([]domain.MessageCollab, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT message_id,user_id,COALESCE(assignee,''),status,sla_due,COALESCE(updated_by,''),updated_at
	 FROM message_collab WHERE user_id=?`
	args := []any{userID}
	if status != "" {
		q += ` AND status=?`
		args = append(args, status)
	}
	if assignee != "" {
		q += ` AND assignee=?`
		args = append(args, assignee)
	}
	q += ` ORDER BY updated_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
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
	_, err := s.db.ExecContext(ctx, `INSERT INTO message_notes (id,message_id,user_id,author,body,created_at)
	 VALUES (?,?,?,?,?,?)`, n.ID, n.MessageID, n.UserID, n.Author, n.Body, n.CreatedAt)
	return err
}

func (s *Store) ListMessageNotes(ctx context.Context, userID, messageID string) ([]domain.MessageNote, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,message_id,user_id,author,body,created_at
	 FROM message_notes WHERE message_id=? AND user_id=? ORDER BY created_at`, messageID, userID)
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
	_, err := s.db.ExecContext(ctx, `INSERT INTO analyses
	 (id,user_id,target_type,target_id,analysis_type,result_json,model,prompt_version,input_hash,created_at)
	 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.UserID, a.TargetType, a.TargetID, a.AnalysisType, a.ResultJSON,
		a.Model, a.PromptVersion, a.InputHash, a.CreatedAt)
	return err
}

// FindCachedAnalysis returns a previous result for identical input+settings (AI-008).
func (s *Store) FindCachedAnalysis(ctx context.Context, userID, analysisType, inputHash, model string) (*domain.Analysis, error) {
	var a domain.Analysis
	err := s.db.QueryRowContext(ctx, `SELECT id,user_id,target_type,target_id,analysis_type,result_json,model,prompt_version,input_hash,created_at
	 FROM analyses WHERE user_id=? AND analysis_type=? AND input_hash=? AND model=?
	 ORDER BY created_at DESC LIMIT 1`, userID, analysisType, inputHash, model).
		Scan(&a.ID, &a.UserID, &a.TargetType, &a.TargetID, &a.AnalysisType, &a.ResultJSON,
			&a.Model, &a.PromptVersion, &a.InputHash, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// ---------- drafts ----------

func (s *Store) CreateDraft(ctx context.Context, d *domain.Draft, v *domain.DraftVersion) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	d.CreatedAt, d.UpdatedAt = now(), now()
	d.CurrentVersion = 1
	v.Version, v.CreatedAt = 1, now()
	if _, err := tx.ExecContext(ctx, `INSERT INTO drafts
	 (id,user_id,account_id,kind,reply_to_message_id,status,current_version,created_at,updated_at)
	 VALUES (?,?,?,?,?,?,?,?,?)`,
		d.ID, d.UserID, d.AccountID, d.Kind, d.ReplyToMessageID, d.Status, d.CurrentVersion, d.CreatedAt, d.UpdatedAt); err != nil {
		return err
	}
	if err := insertDraftVersion(ctx, tx, d.ID, v); err != nil {
		return err
	}
	return tx.Commit()
}

func insertDraftVersion(ctx context.Context, tx *sql.Tx, draftID string, v *domain.DraftVersion) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO draft_versions
	 (draft_id,version,subject,body_text,body_html,to_json,cc_json,bcc_json,author,created_at)
	 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		draftID, v.Version, v.Subject, v.BodyText, v.BodyHTML,
		addrJSON(v.To), addrJSON(v.Cc), addrJSON(v.Bcc), v.Author, v.CreatedAt)
	return err
}

func (s *Store) AddDraftVersion(ctx context.Context, userID, draftID string, v *domain.DraftVersion) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var cur int
	err = tx.QueryRowContext(ctx,
		`SELECT current_version FROM drafts WHERE id=? AND user_id=? AND status='open'`, draftID, userID).Scan(&cur)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	v.Version, v.CreatedAt = cur+1, now()
	if err := insertDraftVersion(ctx, tx, draftID, v); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE drafts SET current_version=?, updated_at=? WHERE id=?`, v.Version, now(), draftID); err != nil {
		return 0, err
	}
	return v.Version, tx.Commit()
}

func (s *Store) GetDraft(ctx context.Context, userID, id string) (*domain.Draft, *domain.DraftVersion, error) {
	var d domain.Draft
	var replyTo sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id,user_id,account_id,kind,reply_to_message_id,status,current_version,created_at,updated_at
	 FROM drafts WHERE id=? AND user_id=?`, id, userID).
		Scan(&d.ID, &d.UserID, &d.AccountID, &d.Kind, &replyTo, &d.Status, &d.CurrentVersion, &d.CreatedAt, &d.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	d.ReplyToMessageID = replyTo.String
	v, err := s.GetDraftVersion(ctx, userID, id, d.CurrentVersion)
	if err != nil {
		return nil, nil, err
	}
	return &d, v, nil
}

func (s *Store) GetDraftVersion(ctx context.Context, userID, draftID string, version int) (*domain.DraftVersion, error) {
	var v domain.DraftVersion
	var toJ, ccJ, bccJ, html sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT v.draft_id,v.version,v.subject,v.body_text,v.body_html,v.to_json,v.cc_json,v.bcc_json,v.author,v.created_at
	 FROM draft_versions v JOIN drafts d ON d.id=v.draft_id
	 WHERE v.draft_id=? AND v.version=? AND d.user_id=?`, draftID, version, userID).
		Scan(&v.DraftID, &v.Version, &v.Subject, &v.BodyText, &html, &toJ, &ccJ, &bccJ, &v.Author, &v.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	v.BodyHTML = html.String
	v.To, v.Cc, v.Bcc = addrFromJSON(toJ.String), addrFromJSON(ccJ.String), addrFromJSON(bccJ.String)
	return &v, nil
}

func (s *Store) SetDraftStatus(ctx context.Context, userID, id string, st domain.DraftStatus) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE drafts SET status=?, updated_at=? WHERE id=? AND user_id=?`, st, now(), id, userID)
	return err
}

// ---------- approvals ----------

func (s *Store) InsertApproval(ctx context.Context, id, userID, actionType, draftID string, draftVersion int, payloadHash, tokenHash, approver string, expiresAt int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO approvals
	 (id,user_id,action_type,draft_id,draft_version,payload_hash,token_hash,approver,expires_at,status,created_at)
	 VALUES (?,?,?,?,?,?,?,?,?,'pending',?)`,
		id, userID, actionType, draftID, draftVersion, payloadHash, tokenHash, approver, expiresAt, now())
	return err
}

// ConsumeApproval flips a pending, unexpired, hash-matching approval to
// 'used' in one statement — the atomicity makes replays impossible.
func (s *Store) ConsumeApproval(ctx context.Context, tokenHash, payloadHash string) (string, int, error) {
	var id, draftID string
	var version int
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", 0, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, `SELECT id, COALESCE(draft_id,''), COALESCE(draft_version,0) FROM approvals
	 WHERE token_hash=? AND payload_hash=? AND status='pending' AND expires_at > ?`,
		tokenHash, payloadHash, now()).Scan(&id, &draftID, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, errors.New("approval token invalid, expired, consumed, or payload changed")
	}
	if err != nil {
		return "", 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE approvals SET status='used' WHERE id=?`, id); err != nil {
		return "", 0, err
	}
	return draftID, version, tx.Commit()
}

// ---------- outbound ----------

func (s *Store) CreateOutbound(ctx context.Context, o *domain.OutboundMessage) error {
	o.CreatedAt, o.UpdatedAt = now(), now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO outbound_messages
	 (id,user_id,draft_id,draft_version,idempotency_key,message_id_hdr,status,attempts,created_at,updated_at)
	 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		o.ID, o.UserID, o.DraftID, o.DraftVersion, o.IdempotencyKey, o.MessageID, o.Status, o.Attempts, o.CreatedAt, o.UpdatedAt)
	return err
}

const outboundCols = `id,user_id,draft_id,draft_version,idempotency_key,message_id_hdr,status,COALESCE(smtp_response,''),attempts,next_attempt_at,created_at,updated_at`

func (s *Store) GetOutboundByIdemKey(ctx context.Context, userID, key string) (*domain.OutboundMessage, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+outboundCols+`
	 FROM outbound_messages WHERE user_id=? AND idempotency_key=?`, userID, key)
	return scanOutbound(row)
}

// MarkOutboundRetry records a temporary failure and schedules the next
// attempt (SMTP-011). Terminal states go through UpdateOutbound.
func (s *Store) MarkOutboundRetry(ctx context.Context, id, smtpResponse string, attempts int, nextAttemptAt int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE outbound_messages SET status=?, smtp_response=?, attempts=?, next_attempt_at=?, updated_at=? WHERE id=?`,
		domain.OutboundRetryWait, smtpResponse, attempts, nextAttemptAt, now(), id)
	return err
}

// ListDueRetries returns outbound messages awaiting a retry whose backoff has
// elapsed (across all users; the worker acts on the whole outbox).
func (s *Store) ListDueRetries(ctx context.Context, nowTS int64, limit int) ([]domain.OutboundMessage, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+outboundCols+`
	 FROM outbound_messages WHERE status=? AND next_attempt_at <= ? ORDER BY next_attempt_at LIMIT ?`,
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

// CountSentSince counts messages successfully sent for an account since a
// unix timestamp, used for rolling-window rate limiting (SMTP-012). Account
// scope is resolved through the draft since outbound rows key off drafts.
func (s *Store) CountSentSince(ctx context.Context, userID, accountID string, since int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbound_messages o
	 JOIN drafts d ON d.id = o.draft_id
	 WHERE d.user_id=? AND d.account_id=? AND o.status='sent' AND o.updated_at >= ?`,
		userID, accountID, since).Scan(&n)
	return n, err
}

func (s *Store) GetOutbound(ctx context.Context, userID, id string) (*domain.OutboundMessage, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+outboundCols+`
	 FROM outbound_messages WHERE user_id=? AND id=?`, userID, id)
	return scanOutbound(row)
}

func scanOutbound(row interface{ Scan(...any) error }) (*domain.OutboundMessage, error) {
	var o domain.OutboundMessage
	err := row.Scan(&o.ID, &o.UserID, &o.DraftID, &o.DraftVersion, &o.IdempotencyKey,
		&o.MessageID, &o.Status, &o.SMTPResponse, &o.Attempts, &o.NextAttemptAt, &o.CreatedAt, &o.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// UpdateOutbound sets a terminal/immediate state and clears any pending retry.
func (s *Store) UpdateOutbound(ctx context.Context, id string, status domain.OutboundStatus, smtpResponse string, attempts int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE outbound_messages SET status=?, smtp_response=?, attempts=?, next_attempt_at=0, updated_at=? WHERE id=?`,
		status, smtpResponse, attempts, now(), id)
	return err
}

// ---------- jobs ----------

func (s *Store) CreateJob(ctx context.Context, j *domain.Job) error {
	j.CreatedAt, j.UpdatedAt = now(), now()
	stats, _ := json.Marshal(j.Stats)
	meta, _ := json.Marshal(j.Meta)
	_, err := s.db.ExecContext(ctx, `INSERT INTO jobs
	 (id,user_id,type,account_id,status,progress,stats_json,error,meta_json,created_at,updated_at)
	 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		j.ID, j.UserID, j.Type, j.AccountID, j.Status, j.Progress, string(stats), j.Error, string(meta), j.CreatedAt, j.UpdatedAt)
	return err
}

func (s *Store) UpdateJob(ctx context.Context, j *domain.Job) error {
	j.UpdatedAt = now()
	stats, _ := json.Marshal(j.Stats)
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status=?, progress=?, stats_json=?, error=?, updated_at=? WHERE id=?`,
		j.Status, j.Progress, string(stats), j.Error, j.UpdatedAt, j.ID)
	return err
}

// RecoverStaleJobs marks jobs left in queued/running (e.g. after a crash)
// as failed so they are not reported as live forever. The scheduler can then
// re-enqueue eligible work (비기능 "Worker 장애 후 Job 재개").
func (s *Store) RecoverStaleJobs(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status='failed', error='interrupted (process restart)', updated_at=?
		 WHERE status IN ('queued','running')`, now())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (s *Store) GetJob(ctx context.Context, userID, id string) (*domain.Job, error) {
	var j domain.Job
	var stats, meta, accountID, progress, jerr sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id,user_id,type,account_id,status,progress,stats_json,error,meta_json,created_at,updated_at
	 FROM jobs WHERE id=? AND user_id=?`, id, userID).
		Scan(&j.ID, &j.UserID, &j.Type, &accountID, &j.Status, &progress, &stats, &jerr, &meta, &j.CreatedAt, &j.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	j.AccountID, j.Progress, j.Error = accountID.String, progress.String, jerr.String
	if stats.String != "" {
		json.Unmarshal([]byte(stats.String), &j.Stats)
	}
	if meta.String != "" {
		json.Unmarshal([]byte(meta.String), &j.Meta)
	}
	return &j, nil
}

func (s *Store) ListJobs(ctx context.Context, userID string, limit int) ([]domain.Job, error) {
	if limit <= 0 {
		limit = 10
	}
	query := `SELECT id, user_id, type, account_id, status, progress, stats_json, error, meta_json, created_at, updated_at
		FROM jobs WHERE user_id=? ORDER BY updated_at DESC LIMIT ?`
	args := []any{userID, limit}
	if userID == "" {
		query = `SELECT id, user_id, type, account_id, status, progress, stats_json, error, meta_json, created_at, updated_at
			FROM jobs ORDER BY updated_at DESC LIMIT ?`
		args = []any{limit}
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Job
	for rows.Next() {
		var j domain.Job
		var stats, meta, accountID, progress, jerr sql.NullString
		if err := rows.Scan(&j.ID, &j.UserID, &j.Type, &accountID, &j.Status, &progress, &stats, &jerr, &meta, &j.CreatedAt, &j.UpdatedAt); err != nil {
			return nil, err
		}
		j.AccountID, j.Progress, j.Error = accountID.String, progress.String, jerr.String
		if stats.String != "" {
			_ = json.Unmarshal([]byte(stats.String), &j.Stats)
		}
		if meta.String != "" {
			_ = json.Unmarshal([]byte(meta.String), &j.Meta)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ---------- audit ----------

func (s *Store) AppendAudit(ctx context.Context, ev domain.AuditEvent) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_events (at,user_id,actor,action,resource,result,detail) VALUES (?,?,?,?,?,?,?)`,
		now(), ev.UserID, ev.Actor, ev.Action, ev.Resource, ev.Result, ev.Detail)
	return err
}

func (s *Store) SearchAudit(ctx context.Context, userID string, limit int) ([]domain.AuditEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,at,user_id,actor,action,resource,result,COALESCE(detail,'')
	 FROM audit_events WHERE user_id=? ORDER BY id DESC LIMIT ?`, userID, limit)
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

// prefixCols qualifies every column in a comma-separated list with a table
// alias, so joined tables (e.g. the FTS index) cannot shadow column names.
func prefixCols(cols, prefix string) string {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		parts[i] = prefix + strings.TrimSpace(p)
	}
	return strings.Join(parts, ",")
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------- MCP Keys ----------

func (s *Store) CreateMCPKey(ctx context.Context, key *domain.MCPKey) error {
	key.CreatedAt = time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `INSERT INTO mcp_keys (id, user_id, name, key_hash, key_prefix, status, created_at, last_used_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		key.ID, key.UserID, key.Name, key.KeyHash, key.KeyPrefix, key.Status, key.CreatedAt, key.LastUsedAt)
	return err
}

func (s *Store) GetMCPKeyByHash(ctx context.Context, keyHash string) (*domain.MCPKey, *domain.User, error) {
	var k domain.MCPKey
	var u domain.User
	err := s.db.QueryRowContext(ctx, `SELECT k.id, k.user_id, k.name, k.key_hash, k.key_prefix, k.status, k.created_at, k.last_used_at,
		u.id, u.login_id, u.display_name, u.email, u.role, u.status, u.auth_provider, u.created_at, u.updated_at, u.last_login_at
		FROM mcp_keys k JOIN users u ON u.id = k.user_id WHERE k.key_hash = ? AND k.status = 'active'`, keyHash).
		Scan(&k.ID, &k.UserID, &k.Name, &k.KeyHash, &k.KeyPrefix, &k.Status, &k.CreatedAt, &k.LastUsedAt,
			&u.ID, &u.LoginID, &u.DisplayName, &u.Email, &u.Role, &u.Status, &u.AuthProvider, &u.CreatedAt, &u.UpdatedAt, &u.LastLoginAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	return &k, &u, nil
}

func (s *Store) ListMCPKeys(ctx context.Context, userID string) ([]domain.MCPKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, user_id, name, key_hash, key_prefix, status, created_at, last_used_at
		FROM mcp_keys WHERE user_id = ? ORDER BY created_at DESC`, userID)
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
	rows, err := s.db.QueryContext(ctx, `SELECT id, user_id, name, key_hash, key_prefix, status, created_at, last_used_at
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
	var err error
	var res sql.Result
	if userID != "" {
		res, err = s.db.ExecContext(ctx, `UPDATE mcp_keys SET status = 'revoked' WHERE id = ? AND user_id = ?`, keyID, userID)
	} else {
		res, err = s.db.ExecContext(ctx, `UPDATE mcp_keys SET status = 'revoked' WHERE id = ?`, keyID)
	}
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) TouchMCPKey(ctx context.Context, keyID string, lastUsedAt int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE mcp_keys SET last_used_at = ? WHERE id = ?`, lastUsedAt, keyID)
	return err
}

func (s *Store) TryAcquireLease(ctx context.Context, key, nodeID string, durationSec int) (bool, error) {
	nowTS := time.Now().Unix()
	expiresTS := nowTS + int64(durationSec)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var value string
	var updatedAt int64
	err = tx.QueryRowContext(ctx, `SELECT value, updated_at FROM system_settings WHERE key = ?`, key).Scan(&value, &updatedAt)

	type Lease struct {
		NodeID    string `json:"node_id"`
		ExpiresAt int64  `json:"expires_at"`
	}

	var current Lease
	isFree := false
	hasRow := true
	if errors.Is(err, sql.ErrNoRows) {
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
		_, err = tx.ExecContext(ctx, `UPDATE system_settings SET value = ?, updated_at = ?, updated_by = ? WHERE key = ?`,
			string(newVal), nowTS, nodeID, key)
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO system_settings (key, value, updated_at, updated_by) VALUES (?, ?, ?, ?)`,
			key, string(newVal), nowTS, nodeID)
	}
	if err != nil {
		return false, err
	}

	err = tx.Commit()
	if err != nil {
		return false, err
	}
	return true, nil
}

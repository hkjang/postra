package application

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"

	"postra/internal/adapters/persistence"
	"postra/internal/domain"
)

const (
	passwordMemory      = 64 * 1024
	passwordIterations  = 3
	passwordParallelism = 2
	passwordKeyLen      = 32
)

func hashPassword(password string) (string, error) {
	if len(password) < 12 {
		return "", userErrf("password must be at least 12 characters")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, passwordIterations, passwordMemory, passwordParallelism, passwordKeyLen)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", passwordMemory, passwordIterations,
		passwordParallelism, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

func verifyPassword(encoded, password string) bool {
	var memory uint32
	var iterations uint32
	var parallelism uint8
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" {
		return false
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &parallelism); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(expected) != passwordKeyLen || memory != passwordMemory ||
		iterations != passwordIterations || parallelism != passwordParallelism {
		return false
	}
	actual := argon2.IDKey([]byte(password), salt, iterations, memory, parallelism, passwordKeyLen)
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

func token(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func tokenHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (a *App) bootstrapAdmin(ctx context.Context) error {
	n, err := a.Store.CountAdmins(ctx)
	if err != nil || n > 0 || a.Cfg.Auth.BootstrapPassword == "" {
		return err
	}
	hash, err := hashPassword(a.Cfg.Auth.BootstrapPassword)
	if err != nil {
		return fmt.Errorf("bootstrap admin: %w", err)
	}
	u, err := a.Store.GetUser(ctx, DefaultUserID)
	if err != nil {
		return err
	}
	u.LoginID = strings.TrimSpace(a.Cfg.Auth.BootstrapAdmin)
	if u.LoginID == "" {
		u.LoginID = "admin"
	}
	u.DisplayName, u.Role, u.Status, u.AuthProvider = u.LoginID, domain.RoleAdmin, domain.UserActive, "local"
	if err := a.Store.UpdateUser(ctx, u); err != nil {
		return err
	}
	return a.Store.SetUserPassword(ctx, u.ID, hash)
}

func (a *App) NeedsAdminSetup(ctx context.Context) (bool, error) {
	n, err := a.Store.CountAdmins(ctx)
	return n == 0, err
}

func (a *App) SetupInitialAdmin(ctx context.Context, loginID, displayName, password string) (*domain.User, error) {
	n, err := a.Store.CountAdmins(ctx)
	if err != nil {
		return nil, err
	}
	if n != 0 {
		return nil, userErrf("initial administrator already exists")
	}
	hash, err := hashPassword(password)
	if err != nil {
		return nil, err
	}
	u, err := a.Store.GetUser(ctx, DefaultUserID)
	if err != nil {
		return nil, err
	}
	u.LoginID = strings.TrimSpace(loginID)
	u.DisplayName = strings.TrimSpace(displayName)
	if u.DisplayName == "" {
		u.DisplayName = u.LoginID
	}
	if u.LoginID == "" {
		return nil, userErrf("login ID is required")
	}
	u.Role, u.Status, u.AuthProvider = domain.RoleAdmin, domain.UserActive, "local"
	if err := a.Store.UpdateUser(ctx, u); err != nil {
		return nil, err
	}
	if err := a.Store.SetUserPassword(ctx, u.ID, hash); err != nil {
		return nil, err
	}
	return u, nil
}

func (a *App) AuthenticateLocal(ctx context.Context, loginID, password string) (*domain.User, error) {
	u, hash, err := a.Store.GetUserByLogin(ctx, strings.TrimSpace(loginID))
	if err != nil || u.Status != domain.UserActive || hash == "" || !verifyPassword(hash, password) {
		time.Sleep(150 * time.Millisecond)
		return nil, userErrf("invalid login ID or password")
	}
	u.LastLoginAt = time.Now().Unix()
	_ = a.Store.UpdateUser(ctx, u)
	a.audit(WithPrincipal(ctx, principalFor(u, "local")), "login", "user:"+u.ID, "ok", "local")
	return u, nil
}

func principalFor(u *domain.User, method string) domain.Principal {
	return domain.Principal{UserID: u.ID, LoginID: u.LoginID, DisplayName: u.DisplayName, Role: u.Role, AuthMethod: method}
}

func (a *App) CreateSession(ctx context.Context, u *domain.User, userAgent, ip string) (string, string, *domain.Session, error) {
	raw, err := token(32)
	if err != nil {
		return "", "", nil, err
	}
	csrf, err := token(24)
	if err != nil {
		return "", "", nil, err
	}
	hours := a.Cfg.Auth.SessionHours
	if settings, err := a.SystemSettings(ctx); err == nil {
		hours = intSetting(settings, SettingAuthSessionHours, hours)
	}
	if hours <= 0 {
		hours = 12
	}
	ss := &domain.Session{
		ID: persistence.NewID("ses"), UserID: u.ID, TokenHash: tokenHash(raw), CSRFHash: tokenHash(csrf),
		ExpiresAt: time.Now().Add(time.Duration(hours) * time.Hour).Unix(),
		UserAgent: userAgent, IPAddress: ip,
	}
	if err := a.Store.CreateSession(ctx, ss); err != nil {
		return "", "", nil, err
	}
	return raw, csrf, ss, nil
}

func (a *App) AuthenticateSession(ctx context.Context, raw string) (*domain.Session, domain.Principal, error) {
	if raw == "" {
		return nil, domain.Principal{}, domain.ErrNotFound
	}
	ss, u, err := a.Store.GetSessionByTokenHash(ctx, tokenHash(raw))
	if err != nil || u.Status != domain.UserActive {
		return nil, domain.Principal{}, domain.ErrNotFound
	}
	now := time.Now().Unix()
	if now-ss.LastSeen >= 60 {
		_ = a.Store.TouchSession(ctx, ss.ID, now)
	}
	return ss, principalFor(u, u.AuthProvider), nil
}

func (a *App) VerifyCSRF(ss *domain.Session, raw string) bool {
	return ss != nil && subtle.ConstantTimeCompare([]byte(ss.CSRFHash), []byte(tokenHash(raw))) == 1
}

func (a *App) Logout(ctx context.Context, sessionID string) error {
	return a.Store.DeleteSession(ctx, sessionID)
}

func requireAdmin(ctx context.Context) (domain.Principal, error) {
	p, ok := PrincipalFrom(ctx)
	if !ok || !p.IsAdmin() {
		return domain.Principal{}, userErrf("administrator permission required")
	}
	return p, nil
}

type CreateUserInput struct {
	LoginID     string
	DisplayName string
	Email       string
	Role        domain.UserRole
	Password    string
}

func (a *App) AdminCreateUser(ctx context.Context, in CreateUserInput) (*domain.User, error) {
	if _, err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if in.Role != domain.RoleAdmin {
		in.Role = domain.RoleUser
	}
	hash, err := hashPassword(in.Password)
	if err != nil {
		return nil, err
	}
	u := &domain.User{
		ID: persistence.NewID("usr"), LoginID: strings.TrimSpace(in.LoginID),
		DisplayName: strings.TrimSpace(in.DisplayName), Email: strings.TrimSpace(in.Email),
		Role: in.Role, Status: domain.UserActive, AuthProvider: "local",
	}
	if u.LoginID == "" {
		return nil, userErrf("login ID is required")
	}
	if u.DisplayName == "" {
		u.DisplayName = u.LoginID
	}
	if err := a.Store.CreateUser(ctx, u, hash); err != nil {
		return nil, err
	}
	a.audit(ctx, "user_create", "user:"+u.ID, "ok", string(u.Role))
	return u, nil
}

func (a *App) AdminListUsers(ctx context.Context) ([]domain.User, error) {
	if _, err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	return a.Store.ListUsers(ctx)
}

func (a *App) AdminUpdateUser(ctx context.Context, id string, role domain.UserRole, status domain.UserStatus) (*domain.User, error) {
	p, err := requireAdmin(ctx)
	if err != nil {
		return nil, err
	}
	u, err := a.Store.GetUser(ctx, id)
	if err != nil {
		return nil, err
	}
	if role != domain.RoleAdmin {
		role = domain.RoleUser
	}
	if status != domain.UserDisabled {
		status = domain.UserActive
	}
	if u.Role == domain.RoleAdmin && u.Status == domain.UserActive &&
		(role != domain.RoleAdmin || status != domain.UserActive) {
		n, err := a.Store.CountAdmins(ctx)
		if err != nil {
			return nil, err
		}
		if n <= 1 {
			return nil, userErrf("cannot disable or demote the last active administrator")
		}
	}
	u.Role, u.Status = role, status
	if err := a.Store.UpdateUser(ctx, u); err != nil {
		return nil, err
	}
	if status == domain.UserDisabled {
		_ = a.Store.DeleteUserSessions(ctx, u.ID)
	}
	a.audit(ctx, "user_update", "user:"+u.ID, "ok", fmt.Sprintf("by=%s role=%s status=%s", p.UserID, role, status))
	return u, nil
}

func (a *App) AdminResetPassword(ctx context.Context, userID, password string) error {
	if _, err := requireAdmin(ctx); err != nil {
		return err
	}
	u, err := a.Store.GetUser(ctx, userID)
	if err != nil {
		return err
	}
	if u.AuthProvider != "local" {
		return userErrf("OIDC-only users manage their password in Keycloak")
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	if err := a.Store.SetUserPassword(ctx, userID, hash); err != nil {
		return err
	}
	_ = a.Store.DeleteUserSessions(ctx, userID)
	a.audit(ctx, "password_reset", "user:"+userID, "ok", "")
	return nil
}

func ClientIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		return host
	}
	return remoteAddr
}

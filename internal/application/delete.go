package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"postra/internal/domain"
)

// LocalDelete removes a message from local storage (DB rows + object blobs).
// It does not touch the mail server (§5.2 기본 정책: 서버 보존).
func (a *App) LocalDelete(ctx context.Context, messageID string) error {
	uris, err := a.Store.DeleteMessage(ctx, DefaultUserID, messageID)
	if err != nil {
		return err
	}
	for _, u := range uris {
		if u != "" {
			_ = a.Objects.Delete(u)
		}
	}
	a.audit(ctx, "local_delete", "message:"+messageID, "ok", "")
	return nil
}

// ServerDeleteCandidate is one entry of a server-delete preview.
type ServerDeleteCandidate struct {
	UIDL      string `json:"uidl"`
	Number    int    `json:"number"`
	Deletable bool   `json:"deletable"` // true only if already stored locally
	Reason    string `json:"reason,omitempty"`
}

type ServerDeletePreview struct {
	AccountID   string                  `json:"account_id"`
	Candidates  []ServerDeleteCandidate `json:"candidates"`
	Deletable   []string                `json:"deletable_uidls"`
	PayloadHash string                  `json:"payload_hash"`
}

// serverDeletePayloadHash binds an approval to the account and the exact set
// of UIDLs (order-independent) so the approved deletion cannot be widened.
func serverDeletePayloadHash(accountID string, uidls []string) string {
	sorted := append([]string(nil), uidls...)
	sort.Strings(sorted)
	h := sha256.New()
	h.Write([]byte("server_delete\n" + accountID + "\n"))
	for _, u := range sorted {
		h.Write([]byte(u + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ServerDeletePreview connects to the maildrop and reports which messages
// are eligible for deletion — only those already stored locally, so nothing
// unsaved is ever removed (§5.2 삭제 조건). It deletes nothing.
func (a *App) ServerDeletePreview(ctx context.Context, accountID string) (*ServerDeletePreview, error) {
	acc, err := a.Store.GetAccount(ctx, DefaultUserID, accountID)
	if err != nil {
		return nil, err
	}
	sess, err := a.dialInbound(ctx, acc, domain.PurposePOP3Auth)
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	remote, err := sess.UIDL(ctx)
	if err != nil {
		return nil, userErrf("server does not support UIDL; server delete is disabled for safety")
	}
	_ = sess.Quit(ctx)

	stored, err := a.Store.StoredUIDLs(ctx, accountID)
	if err != nil {
		return nil, err
	}
	pv := &ServerDeletePreview{AccountID: accountID}
	for _, rm := range remote {
		c := ServerDeleteCandidate{UIDL: rm.UIDL, Number: rm.Number, Deletable: stored[rm.UIDL]}
		if !c.Deletable {
			c.Reason = "not stored locally"
		} else {
			pv.Deletable = append(pv.Deletable, rm.UIDL)
		}
		pv.Candidates = append(pv.Candidates, c)
	}
	pv.PayloadHash = serverDeletePayloadHash(accountID, pv.Deletable)
	a.audit(ctx, "server_delete_preview", "account:"+accountID, "ok",
		strings.TrimSpace(joinInt("deletable=", len(pv.Deletable))))
	return pv, nil
}

// RequestServerDeleteApproval issues a one-time approval bound to the account
// and the exact UIDL set from a fresh preview (§5.2 별도 승인 토큰 필수).
func (a *App) RequestServerDeleteApproval(ctx context.Context, accountID, approver string, ttlSeconds int) (*ServerDeletePreview, *domain.ApprovalToken, error) {
	pv, err := a.ServerDeletePreview(ctx, accountID)
	if err != nil {
		return nil, nil, err
	}
	if len(pv.Deletable) == 0 {
		return pv, nil, userErrf("no locally-stored messages are eligible for server deletion")
	}
	tok, err := a.Issue(ctx, domain.ApprovalRequest{
		UserID: DefaultUserID, ActionType: "server_delete",
		PayloadHash: pv.PayloadHash, TTLSeconds: ttlSeconds, Approver: approver,
	})
	if err != nil {
		return nil, nil, err
	}
	a.audit(ctx, "server_delete_approval_issue", "account:"+accountID, "ok", approver)
	return pv, &tok, nil
}

type ServerDeleteResult struct {
	Deleted   []string `json:"deleted"`
	Uncertain []string `json:"uncertain"` // QUIT outcome unclear (delete_uncertain)
	Skipped   []string `json:"skipped"`
}

// ServerDelete deletes the approved UIDLs from the maildrop. The approval
// token must match the account + UIDL set exactly; any change invalidates it.
// If QUIT is uncertain, affected UIDLs are reported as uncertain rather than
// assumed deleted (delete_uncertain, §5.2 장애 처리).
func (a *App) ServerDelete(ctx context.Context, accountID string, uidls []string, approvalToken string) (*ServerDeleteResult, error) {
	acc, err := a.Store.GetAccount(ctx, DefaultUserID, accountID)
	if err != nil {
		return nil, err
	}
	payloadHash := serverDeletePayloadHash(accountID, uidls)
	if err := a.VerifyAndConsume(ctx, approvalToken, payloadHash); err != nil {
		a.audit(ctx, "server_delete", "account:"+accountID, "denied", err.Error())
		return nil, userErrf("server delete rejected: %v", err)
	}

	// Re-verify local storage at execution time — never delete unsaved mail.
	stored, err := a.Store.StoredUIDLs(ctx, accountID)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, u := range uidls {
		if stored[u] {
			want[u] = true
		}
	}

	sess, err := a.dialInbound(ctx, acc, domain.PurposePOP3Auth)
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	remote, err := sess.UIDL(ctx)
	if err != nil {
		return nil, userErrf("server does not support UIDL; aborting delete")
	}

	res := &ServerDeleteResult{}
	var deletedThisSession []string
	for _, rm := range remote {
		if !want[rm.UIDL] {
			continue
		}
		if err := sess.Delete(ctx, rm.Number); err != nil {
			res.Skipped = append(res.Skipped, rm.UIDL)
			continue
		}
		deletedThisSession = append(deletedThisSession, rm.UIDL)
	}
	// DELE only marks for deletion; QUIT commits it. An uncertain QUIT means
	// we cannot know whether the deletions were applied.
	if err := sess.Quit(ctx); err != nil {
		res.Uncertain = deletedThisSession
		a.audit(ctx, "server_delete", "account:"+accountID, "uncertain",
			joinInt("uncertain=", len(res.Uncertain)))
		return res, nil
	}
	res.Deleted = deletedThisSession
	a.audit(ctx, "server_delete", "account:"+accountID, "ok", joinInt("deleted=", len(res.Deleted)))
	return res, nil
}

func joinInt(prefix string, n int) string {
	return prefix + itoa(n)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

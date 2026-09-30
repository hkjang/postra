package application

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"postra/internal/domain"
)

// A Sent-folder copy carries Bcc; it is kept, and sent mail is never unread.
func TestSentCopyKeepsBccAndIsRead(t *testing.T) {
	raw := "From: Me <me@corp.local>\r\nTo: bob@example.com\r\nBcc: Boss <boss@corp.local>, audit@corp.local\r\n" +
		"Subject: 계약 전달\r\nDate: Tue, 14 Jul 2026 09:00:00 +0900\r\nMessage-ID: <bcc-1@corp.local>\r\n\r\n본문\r\n"
	folders := &fakeFolders{inbox: map[string]string{}, sent: map[string]string{"3.1": raw}, sentName: "Sent"}
	app, acc := sentTestApp(t, folders)
	syncAndWait(t, app, acc.ID)
	sent := sentSearch(t, app, "sent")
	if len(sent) != 1 {
		t.Fatalf("sent = %+v", sent)
	}
	var bcc []string
	for _, a := range sent[0].Bcc {
		bcc = append(bcc, a.Email)
	}
	if !slices.Equal(bcc, []string{"boss@corp.local", "audit@corp.local"}) || sent[0].Bcc[0].Name != "Boss" {
		t.Fatalf("Bcc = %+v", sent[0].Bcc)
	}
	if !sent[0].IsRead {
		t.Fatal("sent mail stored as unread")
	}
}

// Postra's record of a send names its Bcc recipients; what went over the wire
// never does — recipients must not learn who else was copied.
func TestSendRecordKeepsBccThatTheWireNeverCarries(t *testing.T) {
	app, _, smtp, _ := newTestApp(t)
	acc := mustAccount(t, app)
	ctx := WithActor(context.Background(), "test")
	dv, err := app.CreateDraft(ctx, CreateDraftInput{AccountID: acc.ID, Kind: "new", To: []string{"bob@example.com"},
		Bcc: []string{"boss@corp.local"}, Subject: "숨은 참조 확인", Body: "본문"})
	if err != nil {
		t.Fatal(err)
	}
	_, tok, err := app.RequestSendApproval(ctx, dv.Draft.ID, "tester", 60)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Send(ctx, SendInput{DraftID: dv.Draft.ID, ApprovalToken: tok.Token}); err != nil {
		t.Fatal(err)
	}
	if len(smtp.sent) != 1 || strings.Contains(strings.ToLower(string(smtp.sent[0].raw)), "bcc:") {
		t.Fatalf("the transmitted message discloses Bcc:\n%s", smtp.sent[0].raw)
	}
	if !slices.Contains(smtp.sent[0].env.To, "boss@corp.local") {
		t.Fatal("the Bcc recipient was not in the envelope, so was never delivered")
	}
	sent := sentSearch(t, app, "sent")
	if len(sent) != 1 || len(sent[0].Bcc) != 1 || sent[0].Bcc[0].Email != "boss@corp.local" || !sent[0].IsRead {
		t.Fatalf("recorded copy = %+v", sent)
	}
}

func TestBccHeaderEncodesNamesAndDropsInvalidAddresses(t *testing.T) {
	got := bccHeader([]domain.Address{{Name: "김 부장", Email: "boss@corp.local"}, {Email: "not an address\r\nX-Injected: 1"}, {Email: "audit@corp.local"}})
	if strings.Contains(got, "X-Injected") || strings.Count(got, "\r\n") != 1 || !strings.Contains(got, "<boss@corp.local>") || !strings.Contains(got, "audit@corp.local") || strings.Contains(got, "김") {
		t.Fatalf("header = %q", got)
	}
	if bccHeader(nil) != "" {
		t.Fatal("empty Bcc produced a header")
	}
}

// Awaiting reply: sent mail that is still the last message of its conversation.
func TestAwaitingReplyIsTheLastWordInTheConversation(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	ctx := WithActor(context.Background(), "test")
	owner := DefaultUserID
	base := time.Now().Add(-48 * time.Hour).Unix()
	insert := func(id, thread, mailbox string, at int64) {
		t.Helper()
		m := &domain.Message{ID: id, UserID: owner, AccountID: "acc_x", UIDL: id, Subject: id, MessageID: "<" + id + "@x>",
			RawHash: id, ThreadID: thread, Date: at, Mailbox: mailbox}
		if err := app.Store.InsertMessage(ctx, m, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	insert("q-answered", "t1", domain.MailboxInbox, base)
	insert("s-answered", "t1", domain.MailboxSent, base+60)
	insert("a-answered", "t1", domain.MailboxInbox, base+120) // they replied
	insert("q-pending", "t2", domain.MailboxInbox, base)
	insert("s-pending", "t2", domain.MailboxSent, base+60) // my reply is the last word
	insert("s-first", "t3", domain.MailboxSent, base)
	insert("s-followup", "t3", domain.MailboxSent, base+300) // a nudge supersedes the first
	insert("s-alone", "", domain.MailboxSent, base+10)       // no thread: nothing can answer it

	res, err := app.Search(ctx, domain.SearchQuery{Folder: "sent", AwaitingReply: true, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range res.Messages {
		got = append(got, m.ID)
	}
	if !slices.Equal(got, []string{"s-followup", "s-pending", "s-alone"}) {
		t.Fatalf("awaiting reply = %v", got)
	}
	// The flag means nothing outside the sent view.
	all, _ := app.Search(ctx, domain.SearchQuery{AwaitingReply: true, Limit: 50})
	if len(all.Messages) != 3 {
		t.Fatalf("awaiting_reply changed the received view: %d messages", len(all.Messages))
	}
}

// IDLE wakes up on every new inbox message; it must not re-read Sent each time.
func TestIdleSyncsReadTheSentFolderAtMostEveryInterval(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	old := sentFolderIdleInterval
	sentFolderIdleInterval = time.Hour
	t.Cleanup(func() { sentFolderIdleInterval = old })
	idle := SyncOptions{fromIdle: true}
	if !app.sentFolderDue("acc", idle) {
		t.Fatal("the first IDLE sync skipped Sent")
	}
	if app.sentFolderDue("acc", idle) {
		t.Fatal("a second IDLE sync inside the interval re-read Sent")
	}
	if !app.sentFolderDue("acc", SyncOptions{}) {
		t.Fatal("a scheduled or manual sync skipped Sent")
	}
	if !app.sentFolderDue("other", idle) {
		t.Fatal("the throttle leaked across accounts")
	}
	sentFolderIdleInterval = 0
	if !app.sentFolderDue("acc", idle) {
		t.Fatal("an IDLE sync after the interval skipped Sent")
	}
}

// "Did I reply?" is answered by sent mail, matched on who it went to.
func TestAskUsesSentMailAndKeepsItsSlots(t *testing.T) {
	app, _, _, ai := newTestApp(t)
	ai.response = `{"summary":"답장함","answer":"네, 회신했습니다.","evidence_message_ids":["s-bob"]}`
	ctx := WithActor(context.Background(), "test")
	now := time.Now().Unix()
	for i := range 8 { // a busy inbox that would fill every slot
		id := "in-" + string(rune('a'+i))
		if err := app.Store.InsertMessage(ctx, &domain.Message{ID: id, UserID: DefaultUserID, AccountID: "acc_x", UIDL: id, Subject: "견적 문의 " + id,
			From: domain.Address{Email: "bob@partner.test"}, MessageID: "<" + id + "@x>", RawHash: id, Date: now - int64(i*60)},
			&domain.MessageBody{MessageID: id, TextBody: "견적"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.Store.InsertMessage(ctx, &domain.Message{ID: "s-bob", UserID: DefaultUserID, AccountID: "acc_x", UIDL: "sent:s-bob", Subject: "Re: 견적 문의",
		From: domain.Address{Email: "me@corp.local"}, To: []domain.Address{{Email: "bob@partner.test"}}, MessageID: "<s-bob@x>", RawHash: "s-bob",
		Date: now - 30, Mailbox: domain.MailboxSent}, &domain.MessageBody{MessageID: "s-bob", TextBody: "회신드립니다"}, nil); err != nil {
		t.Fatal(err)
	}
	res, err := app.Ask(ctx, AskInput{Question: "bob한테 견적 답장했어?", Mode: "keyword", SearchText: "견적", From: "bob@partner.test"})
	if err != nil {
		t.Fatal(err)
	}
	received := 0
	for _, s := range res.Retrieval.Sources {
		if s.Mailbox != domain.MailboxSent {
			received++
		}
	}
	if received == 0 || len(res.Retrieval.Sources) > 6 {
		t.Fatalf("the busy inbox was not retrieved, or the evidence overflowed: %+v", res.Retrieval.Sources)
	}
	var sentSource *AskSource
	for i := range res.Retrieval.Sources {
		if res.Retrieval.Sources[i].MessageID == "s-bob" {
			sentSource = &res.Retrieval.Sources[i]
		}
	}
	if sentSource == nil || sentSource.Mailbox != domain.MailboxSent {
		t.Fatalf("the reply was not used as evidence: %+v", res.Retrieval.Sources)
	}
	prompt := ai.lastRequest.User + ai.lastRequest.Untrusted
	if !strings.Contains(prompt, `"mailbox":"sent"`) || !strings.Contains(prompt, `"to":["bob@partner.test"]`) {
		t.Fatal("the model was not told which evidence the user sent, or to whom")
	}
}

// An address the identity provider says is unverified is no link key: where
// users set their own email unchecked, it would hand over another person's
// local account. Providers that omit the claim keep the old behaviour.
func TestOIDCLinkRefusesAnUnverifiedEmail(t *testing.T) {
	for name, tc := range map[string]struct {
		verified *bool
		linked   bool
	}{
		"unverified": {verified(false), false},
		"verified":   {verified(true), true},
		"no claim":   {nil, true},
	} {
		t.Run(name, func(t *testing.T) {
			app, _, _, _ := newTestApp(t)
			ctx := context.Background()
			u := &domain.User{ID: "local-" + strings.ReplaceAll(name, " ", "-"), LoginID: "local-person", Role: domain.RoleUser, Status: domain.UserActive, AuthProvider: "local", Email: "person@corp.local"}
			if err := app.Store.CreateUser(ctx, u, ""); err != nil {
				t.Fatal(err)
			}
			got := app.linkExistingLocalUser(ctx, oidcRuntime{Issuer: "https://kc/realms/x"},
				oidcClaims{Subject: "sub-1", Email: "person@corp.local", EmailVerified: tc.verified})
			if (got != nil) != tc.linked {
				t.Fatalf("linked=%v, want %v", got != nil, tc.linked)
			}
		})
	}
}

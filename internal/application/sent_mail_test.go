package application

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"postra/internal/domain"
)

// fakeFolders is an IMAP server with an INBOX and, unless sentName is empty,
// a Sent folder. UIDs are "validity.uid" per folder; tests make the two
// folders collide on purpose.
type fakeFolders struct {
	mu        sync.Mutex
	inbox     map[string]string // uidl -> raw
	sent      map[string]string
	sentName  string
	lookupErr error
	selectErr error
}

type fakeFolderSession struct {
	d      *fakeFolders
	folder map[string]string
}

func (d *fakeFolders) Dial(context.Context, domain.POP3DialOptions) (domain.POP3Session, error) {
	return &fakeFolderSession{d: d, folder: d.inbox}, nil
}

func (s *fakeFolderSession) numbered() []string {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	var uidls []string
	for u := range s.folder {
		uidls = append(uidls, u)
	}
	sort.Strings(uidls)
	return uidls
}

func (s *fakeFolderSession) UIDL(context.Context) ([]domain.RemoteMessage, error) {
	var out []domain.RemoteMessage
	for i, u := range s.numbered() {
		out = append(out, domain.RemoteMessage{Number: i + 1, UIDL: u, Size: int64(len(s.folder[u]))})
	}
	return out, nil
}
func (s *fakeFolderSession) List(ctx context.Context) ([]domain.RemoteMessage, error) {
	return s.UIDL(ctx)
}
func (s *fakeFolderSession) Retrieve(_ context.Context, n int) (io.ReadCloser, error) {
	u := s.numbered()[n-1]
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	return io.NopCloser(strings.NewReader(s.folder[u])), nil
}
func (s *fakeFolderSession) Top(ctx context.Context, n, _ int) (io.ReadCloser, error) {
	return s.Retrieve(ctx, n)
}
func (s *fakeFolderSession) Delete(context.Context, int) error { return nil }
func (s *fakeFolderSession) Quit(context.Context) error        { return nil }
func (s *fakeFolderSession) Close() error                      { return nil }
func (s *fakeFolderSession) SentMailbox(context.Context) (string, error) {
	return s.d.sentName, s.d.lookupErr
}
func (s *fakeFolderSession) SelectMailbox(name string) error {
	if s.d.selectErr != nil {
		return s.d.selectErr
	}
	if name != s.d.sentName {
		return fmt.Errorf("no such mailbox %q", name)
	}
	s.folder = s.d.sent
	return nil
}

func outgoingMail(id, to, subject, body string) string {
	return fmt.Sprintf("From: Me <me@corp.local>\r\nTo: %s\r\nSubject: %s\r\n"+
		"Date: Tue, 14 Jul 2026 09:00:00 +0900\r\nMessage-ID: <%s@corp.local>\r\n"+
		"Content-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n", to, subject, id, body)
}

func sentTestApp(t *testing.T, folders *fakeFolders) (*App, *domain.MailAccount) {
	t.Helper()
	app, _, _, _ := newTestApp(t)
	app.IMAP = folders
	return app, imapAccount(t, app)
}

func sentSearch(t *testing.T, app *App, folder string) []domain.Message {
	t.Helper()
	res, err := app.Search(WithActor(context.Background(), "test"), domain.SearchQuery{Folder: folder, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return res.Messages
}

func subjects(ms []domain.Message) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.Subject)
	}
	sort.Strings(out)
	return out
}

func TestSyncIngestsTheSentFolderApartFromReceivedMail(t *testing.T) {
	folders := &fakeFolders{
		// Both folders use UID 1.10: the Sent UID must not be skipped as seen.
		inbox:    map[string]string{"1.10": testMail("in-1", "받은 메일", "hello")},
		sent:     map[string]string{"1.10": outgoingMail("out-1", "bob@example.com", "보낸 메일", "reply")},
		sentName: "Sent",
	}
	app, acc := sentTestApp(t, folders)
	job := syncAndWait(t, app, acc.ID)
	if job.Status != domain.JobSucceeded || job.Stats["new"] != 1 || job.Stats["sent_new"] != 1 {
		t.Fatalf("job = %s stats=%v", job.Status, job.Stats)
	}
	// Every existing view is received mail only.
	for _, folder := range []string{"", "inbox", "important", "archive"} {
		for _, m := range sentSearch(t, app, folder) {
			if m.Mailbox != domain.MailboxInbox {
				t.Fatalf("view %q shows sent mail %q", folder, m.Subject)
			}
		}
	}
	if got := subjects(sentSearch(t, app, "")); !slices.Equal(got, []string{"받은 메일"}) {
		t.Fatalf("default view = %v", got)
	}
	sent := sentSearch(t, app, "sent")
	if len(sent) != 1 || sent[0].Subject != "보낸 메일" || sent[0].Mailbox != domain.MailboxSent || sent[0].To[0].Email != "bob@example.com" {
		t.Fatalf("sent view = %+v", sent)
	}

	again := syncAndWait(t, app, acc.ID)
	if again.Stats["new"] != 0 || again.Stats["sent_new"] != 0 || len(sentSearch(t, app, "sent")) != 1 {
		t.Fatalf("a re-sync duplicated mail: %v", again.Stats)
	}
}

// Sent mail is not "mail to act on": rules, AI triage and embeddings — and so
// semantic search — read received mail only.
func TestSentMailStaysOutOfReceivedMailAutomation(t *testing.T) {
	folders := &fakeFolders{
		inbox:    map[string]string{"1.1": testMail("in-2", "견적 요청", "quote")},
		sent:     map[string]string{"2.1": outgoingMail("out-2", "bob@example.com", "견적 회신", "quote")},
		sentName: "Sent",
	}
	app, acc := sentTestApp(t, folders)
	ctx := WithActor(context.Background(), "test")
	if _, err := app.CreateRule(ctx, domain.MailRule{Name: "tag quotes", Enabled: true, Match: "all",
		Conditions: []domain.RuleCondition{{Field: "subject", Operator: "contains", Value: "견적"}},
		Actions:    []domain.RuleAction{{Type: "add_label", Value: "quote"}}}); err != nil {
		t.Fatal(err)
	}
	syncAndWait(t, app, acc.ID)
	for _, m := range sentSearch(t, app, "sent") {
		if slices.Contains(m.Labels, "quote") {
			t.Fatal("a rule fired on sent mail")
		}
	}
	if in := sentSearch(t, app, ""); len(in) != 1 || !slices.Contains(in[0].Labels, "quote") {
		t.Fatal("the rule did not fire on received mail, so this test proves nothing")
	}
	sentID := sentSearch(t, app, "sent")[0].ID
	triage, err := app.Store.MessagesNeedingTriage(ctx, acc.UserID, 0, 100)
	if err != nil || slices.Contains(triage, sentID) {
		t.Fatalf("sent mail offered to AI triage: %v %v", triage, err)
	}
	embed, err := app.Store.MessagesMissingEmbeddings(ctx, acc.UserID, "", 100)
	if err != nil || slices.Contains(embed, sentID) || len(embed) != 1 {
		t.Fatalf("sent mail offered for embedding: %v %v", embed, err)
	}
}

// A reply and the mail it answers are one conversation, whichever side each
// came from.
func TestSentMailJoinsItsThread(t *testing.T) {
	question := testMail("q-3", "회의 일정", "언제 가능하세요?")
	answer := "From: Me <me@corp.local>\r\nTo: alice@example.com\r\nSubject: Re: 회의 일정\r\n" +
		"Date: Tue, 14 Jul 2026 11:00:00 +0900\r\nMessage-ID: <a-3@corp.local>\r\n" +
		"In-Reply-To: <q-3@example.com>\r\nReferences: <q-3@example.com>\r\n\r\n목요일 가능합니다\r\n"
	folders := &fakeFolders{inbox: map[string]string{"1.1": question}, sent: map[string]string{"2.1": answer}, sentName: "Sent"}
	app, acc := sentTestApp(t, folders)
	syncAndWait(t, app, acc.ID)
	in, sent := sentSearch(t, app, ""), sentSearch(t, app, "sent")
	if len(in) != 1 || len(sent) != 1 || in[0].ThreadID == "" || in[0].ThreadID != sent[0].ThreadID {
		t.Fatalf("reply not threaded with its question: in=%+v sent=%+v", in, sent)
	}
}

func TestSyncSucceedsWithoutAReadableSentFolder(t *testing.T) {
	for name, folders := range map[string]*fakeFolders{
		"no sent folder": {inbox: map[string]string{"1.1": testMail("in-4", "a", "b")}},
		"lookup fails":   {inbox: map[string]string{"1.1": testMail("in-5", "a", "b")}, sentName: "Sent", lookupErr: errors.New("LIST rejected")},
		"select refused": {inbox: map[string]string{"1.1": testMail("in-6", "a", "b")}, sentName: "Sent", selectErr: errors.New("NO [NONEXISTENT] no such mailbox")},
	} {
		t.Run(name, func(t *testing.T) {
			app, acc := sentTestApp(t, folders)
			job := syncAndWait(t, app, acc.ID)
			if job.Status != domain.JobSucceeded || job.Stats["new"] != 1 {
				t.Fatalf("received mail sync failed with the Sent folder: %s %v", job.Status, job.Stats)
			}
		})
	}
}

// Mail sent through Postra is sent mail at once — including for POP3, which
// has no Sent folder — and the server's own Sent copy of it is not a second one.
func TestSendRecordsTheSentCopyOnce(t *testing.T) {
	app, _, smtp, _ := newTestApp(t)
	acc := mustAccount(t, app) // POP3
	ctx := WithActor(context.Background(), "test")
	dv, err := app.CreateDraft(ctx, CreateDraftInput{AccountID: acc.ID, Kind: "new", To: []string{"bob@example.com"}, Subject: "보고서 전달", Body: "첨부 확인 부탁드립니다"})
	if err != nil {
		t.Fatal(err)
	}
	_, tok, err := app.RequestSendApproval(ctx, dv.Draft.ID, "tester", 60)
	if err != nil {
		t.Fatal(err)
	}
	out, err := app.Send(ctx, SendInput{DraftID: dv.Draft.ID, ApprovalToken: tok.Token})
	if err != nil || out.Status != domain.OutboundSent {
		t.Fatalf("send: %+v %v", out, err)
	}
	sent := sentSearch(t, app, "sent")
	if len(sent) != 1 || sent[0].Subject != "보고서 전달" || sent[0].Mailbox != domain.MailboxSent || sent[0].MessageID == "" {
		t.Fatalf("sent copy = %+v", sent)
	}
	if len(sentSearch(t, app, "")) != 0 {
		t.Fatal("the sent copy appeared as received mail")
	}
	if len(smtp.sent) != 1 {
		t.Fatalf("smtp saw %d messages", len(smtp.sent))
	}
}

// The server saved Postra's submission to Sent — same Message-ID, other bytes
// (servers add headers). The Sent sync must not make it a second message.
func TestSentSyncSkipsTheServerCopyOfAPostraSend(t *testing.T) {
	folders := &fakeFolders{inbox: map[string]string{}, sent: map[string]string{}, sentName: "Sent"}
	app, acc := sentTestApp(t, folders)
	ctx := WithActor(context.Background(), "test")
	dv, err := app.CreateDraft(ctx, CreateDraftInput{AccountID: acc.ID, Kind: "new", To: []string{"bob@example.com"}, Subject: "계약서", Body: "검토 부탁드립니다"})
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
	smtp := app.SMTP.(*fakeSMTP)
	folders.mu.Lock()
	folders.sent["7.1"] = "X-Server-Saved: yes\r\n" + string(smtp.sent[0].raw)
	folders.mu.Unlock()

	job := syncAndWait(t, app, acc.ID)
	if job.Stats["sent_new"] != 0 || job.Stats["sent_duplicate"] != 1 {
		t.Fatalf("server copy handled wrongly: %v", job.Stats)
	}
	if got := sentSearch(t, app, "sent"); len(got) != 1 {
		t.Fatalf("sent view holds %d copies of one message", len(got))
	}
}

// A send whose outcome is unknown was maybe not delivered: no sent copy.
func TestUncertainSendRecordsNoSentCopy(t *testing.T) {
	app, _, smtp, _ := newTestApp(t)
	smtp.uncertain = true
	acc := mustAccount(t, app)
	ctx := WithActor(context.Background(), "test")
	dv, err := app.CreateDraft(ctx, CreateDraftInput{AccountID: acc.ID, Kind: "new", To: []string{"bob@example.com"}, Subject: "s", Body: "b"})
	if err != nil {
		t.Fatal(err)
	}
	_, tok, _ := app.RequestSendApproval(ctx, dv.Draft.ID, "tester", 60)
	_, _ = app.Send(ctx, SendInput{DraftID: dv.Draft.ID, ApprovalToken: tok.Token})
	if got := sentSearch(t, app, "sent"); len(got) != 0 {
		t.Fatalf("an uncertain send was recorded as sent: %+v", got)
	}
}

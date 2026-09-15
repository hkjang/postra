package application

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/mail"
	"net/textproto"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	adsmtp "postra/internal/adapters/smtp"
	"postra/internal/domain"
)

// A real loopback SMTP transaction, not the application's fakeSMTP port.
// The fixture never contacts a user's configured relay or delivers externally.
func TestSMTPIntegrationRenderedAttachmentApprovalAndIdempotency(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		name := "accepted"
		if uncertain {
			name = "lost-final-response"
		}
		t.Run(name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			var authCalls, dataCalls atomic.Int32
			received := make(chan []byte, 2)
			done := make(chan struct{})
			go func() {
				defer close(done)
				for {
					conn, err := listener.Accept()
					if err != nil {
						return
					}
					_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
					func() {
						defer conn.Close()
						wire := textproto.NewConn(conn)
						if wire.PrintfLine("220 localhost Postra integration relay") != nil {
							return
						}
						for {
							line, err := wire.ReadLine()
							if err != nil {
								return
							}
							command, _, _ := strings.Cut(line, " ")
							switch command {
							case "EHLO":
								_ = wire.PrintfLine("250-localhost\r\n250-AUTH PLAIN LOGIN\r\n250 8BITMIME")
							case "HELO", "MAIL", "RCPT", "RSET":
								_ = wire.PrintfLine("250 ok")
							case "AUTH":
								authCalls.Add(1)
								_ = wire.PrintfLine("535 AUTH must not be used for this offline relay")
							case "DATA":
								_ = wire.PrintfLine("354 send content")
								data, err := wire.ReadDotBytes()
								if err != nil {
									return
								}
								dataCalls.Add(1)
								received <- data
								if uncertain {
									return
								}
								_ = wire.PrintfLine("250 queued locally")
							case "QUIT":
								_ = wire.PrintfLine("221 bye")
								return
							default:
								_ = wire.PrintfLine("500 unsupported command")
							}
						}
					}()
				}
			}()
			a, _, _, _ := newTestApp(t)
			a.SMTP = adsmtp.Client{}
			ctx := settingsAdmin()
			account := mustAccount(t, a)
			account.SMTPPort = listener.Addr().(*net.TCPAddr).Port
			account.SMTPAuth = "none"
			if err := a.Store.UpdateAccount(ctx, account); err != nil {
				t.Fatal(err)
			}
			draft, err := a.CreateDraft(ctx, CreateDraftInput{AccountID: account.ID, To: []string{"recipient@corp.local"}, Bcc: []string{"hidden@corp.local"}, Subject: "오프라인 HTML 통합 시험", Body: "안녕하세요.\n\n- 구성도 확인\n- 일정 회신", Format: "markdown"})
			if err != nil {
				t.Fatal(err)
			}
			draft, err = a.AddDraftAttachment(ctx, AddDraftAttachmentInput{DraftID: draft.Draft.ID, Name: "설명.txt", DataBase64: base64.StdEncoding.EncodeToString([]byte("첨부 근거"))})
			if err != nil {
				t.Fatal(err)
			}
			_, approval, err := a.RequestSendApproval(ctx, draft.Draft.ID, "loopback-test", 60)
			if err != nil {
				t.Fatal(err)
			}
			input := SendInput{DraftID: draft.Draft.ID, ApprovalToken: approval.Token, IdempotencyKey: "smtp-integration-only"}
			out, err := a.Send(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			if uncertain && out.Status != domain.OutboundUncertain {
				t.Fatalf("missing uncertain state: %s", out.Status)
			}
			if !uncertain && out.Status != domain.OutboundSent {
				t.Fatalf("not sent: %s", out.Status)
			}
			replay, err := a.Send(ctx, input)
			if err != nil || replay.ID != out.ID {
				t.Fatalf("idempotency replay: %+v %v", replay, err)
			}
			select {
			case raw := <-received:
				message, err := mail.ReadMessage(bytes.NewReader(raw))
				if err != nil {
					t.Fatal(err)
				}
				if message.Header.Get("Bcc") != "" {
					t.Fatal("Bcc disclosed in MIME")
				}
				types := []string{}
				var walk func(string, io.Reader)
				walk = func(contentType string, reader io.Reader) {
					kind, params, err := mime.ParseMediaType(contentType)
					if err != nil {
						t.Fatal(err)
					}
					types = append(types, kind)
					if strings.HasPrefix(kind, "multipart/") {
						parts := multipart.NewReader(reader, params["boundary"])
						for {
							part, err := parts.NextPart()
							if err == io.EOF {
								break
							}
							if err != nil {
								t.Fatal(err)
							}
							walk(part.Header.Get("Content-Type"), part)
						}
					}
				}
				walk(message.Header.Get("Content-Type"), message.Body)
				if strings.Join(types, ",") != "multipart/mixed,multipart/alternative,text/plain,text/html,text/plain" {
					t.Fatalf("SMTP changed MIME: %v", types)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("relay did not receive DATA")
			}
			if authCalls.Load() != 0 || dataCalls.Load() != 1 {
				t.Fatalf("AUTH=%d DATA=%d", authCalls.Load(), dataCalls.Load())
			}
			_ = listener.Close()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("fixture didn't stop")
			}
		})
	}
}

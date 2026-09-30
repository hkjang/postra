package imap

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
)

// mailboxServer scripts LIST and a per-mailbox SELECT, so a test can check
// which folder the client picks and that UIDs are read from that folder.
func mailboxServer(t *testing.T, list []string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		io.WriteString(conn, "* OK IMAP4rev1 ready\r\n")
		validity := "1"
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			sp := strings.SplitN(line, " ", 3)
			if len(sp) < 2 {
				continue
			}
			tag, cmd := sp[0], strings.ToUpper(sp[1])
			switch cmd {
			case "LIST":
				for _, l := range list {
					io.WriteString(conn, l+"\r\n")
				}
				fmt.Fprintf(conn, "%s OK LIST completed\r\n", tag)
			case "SELECT":
				validity = "1"
				if !strings.Contains(line, "INBOX") {
					validity = "900" // a different folder, a different UID space
				}
				io.WriteString(conn, "* 1 EXISTS\r\n")
				fmt.Fprintf(conn, "* OK [UIDVALIDITY %s] ok\r\n", validity)
				fmt.Fprintf(conn, "%s OK [READ-WRITE] SELECT completed\r\n", tag)
			case "FETCH":
				uid := 10
				if validity != "1" {
					uid = 55
				}
				fmt.Fprintf(conn, "* 1 FETCH (UID %s RFC822.SIZE 20)\r\n", strconv.Itoa(uid))
				fmt.Fprintf(conn, "%s OK FETCH completed\r\n", tag)
			case "LOGOUT":
				io.WriteString(conn, "* BYE\r\n")
				fmt.Fprintf(conn, "%s OK LOGOUT completed\r\n", tag)
				return
			default:
				fmt.Fprintf(conn, "%s OK\r\n", tag)
			}
		}
	}()
	return ln.Addr().String()
}

func dialMailboxServer(t *testing.T, list []string) *session {
	t.Helper()
	s := dial(t, mailboxServer(t, list))
	t.Cleanup(func() { s.Close() })
	return s.(*session)
}

func TestSentMailboxPrefersTheSpecialUseAttribute(t *testing.T) {
	s := dialMailboxServer(t, []string{
		`* LIST (\HasNoChildren) "/" "INBOX"`,
		`* LIST (\HasNoChildren) "/" "Sent"`, // a name match, listed first
		`* LIST (\HasNoChildren \Sent) "/" "[Gmail]/Sent Mail"`,
	})
	if got, err := s.SentMailbox(context.Background()); err != nil || got != "[Gmail]/Sent Mail" {
		t.Fatalf("SentMailbox = %q, %v", got, err)
	}
}

func TestSentMailboxFallsBackToWellKnownNames(t *testing.T) {
	for name, list := range map[string][]string{
		"INBOX.Sent Items": {`* LIST (\HasNoChildren) "." "INBOX"`, `* LIST (\HasNoChildren) "." "INBOX.Sent Items"`},
		"Sent Messages":    {`* LIST (\HasNoChildren) "/" INBOX`, `* LIST (\HasNoChildren) "/" "Sent Messages"`},
		// 보낸편지함 in modified UTF-7, as Korean servers name it.
		"&vPSwuNO4ycDVaA-": {`* LIST (\HasNoChildren) "/" "INBOX"`, `* LIST (\HasNoChildren) "/" "&vBvHQNO4ycDVaA-"`, `* LIST (\HasNoChildren) "/" "&vPSwuNO4ycDVaA-"`},
	} {
		t.Run(name, func(t *testing.T) {
			s := dialMailboxServer(t, list)
			if got, err := s.SentMailbox(context.Background()); err != nil || got != name {
				t.Fatalf("SentMailbox = %q, %v; want %q (the server's own spelling, for SELECT)", got, err, name)
			}
		})
	}
}

func TestSentMailboxIgnoresUnselectableAndOtherFolders(t *testing.T) {
	s := dialMailboxServer(t, []string{
		`* LIST (\Noselect \HasChildren) "/" "Sent"`,
		`* LIST (\HasNoChildren \Drafts) "/" "Drafts"`,
		`* LIST (\HasNoChildren) "/" "Sentinel"`,
	})
	if got, err := s.SentMailbox(context.Background()); err != nil || got != "" {
		t.Fatalf("SentMailbox = %q, %v; want none", got, err)
	}
}

// After selecting the Sent folder, enumeration reads that folder's UIDs.
func TestSelectMailboxSwitchesTheUIDSpace(t *testing.T) {
	s := dialMailboxServer(t, []string{`* LIST (\Sent) "/" "Sent"`})
	inbox, err := s.UIDL(context.Background())
	if err != nil || len(inbox) != 1 || inbox[0].UIDL != "1.10" {
		t.Fatalf("INBOX UIDL = %+v, %v", inbox, err)
	}
	name, _ := s.SentMailbox(context.Background())
	if err := s.SelectMailbox(name); err != nil {
		t.Fatal(err)
	}
	sent, err := s.UIDL(context.Background())
	if err != nil || len(sent) != 1 || sent[0].UIDL != "900.55" {
		t.Fatalf("Sent UIDL = %+v, %v", sent, err)
	}
}

func TestDecodeModifiedUTF7(t *testing.T) {
	for in, want := range map[string]string{
		"&vPSwuNO4ycDVaA-":    "보낸편지함",
		"&vPSwuA- &07jJwNVo-": "보낸 편지함",
		"INBOX":               "INBOX",
		"Tom &- Jerry":        "Tom & Jerry",
		"&broken":             "&broken",
		"&!!!-":               "&!!!-",
	} {
		if got := decodeModifiedUTF7(in); got != want {
			t.Errorf("decodeModifiedUTF7(%q) = %q, want %q", in, got, want)
		}
	}
}
